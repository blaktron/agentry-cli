package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The run's decision on the registry's verdict (agentry-cli#58): a known
// high/critical advisory refuses unless the flag or the policy allows it,
// and the receipt line always says what was checked and why it ran.
func TestVulnerabilityVerdict(t *testing.T) {
	bad := mcpinstall.VulnStatus{Ref: "npm:minimist@1.2.5", Status: "vulnerable", MaxSeverity: "critical",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-xvch-5gv4-984h", Severity: "critical", FixedIn: []string{"1.2.6"}}}}
	ok := mcpinstall.VulnStatus{Ref: "pypi:mcp-server-fetch@0.6.2", Status: "safe"}
	down := mcpinstall.VulnStatus{Ref: "npm:x@1", Status: "unreachable", Err: "dial tcp: refused"}

	line, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{ok, down}, "https://reg.example", false, false)
	if err != nil || !strings.Contains(line, "checked against https://reg.example") || !strings.Contains(line, "unreachable (dial tcp: refused)") {
		t.Fatalf("clean+unreachable: %q %v", line, err)
	}
	_, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{ok, bad}, "https://reg.example", false, false)
	if err == nil || !strings.Contains(err.Error(), "npm:minimist@1.2.5 has a known critical vulnerability (GHSA-xvch-5gv4-984h), fixed in 1.2.6") || !strings.Contains(err.Error(), "--allow-vulnerable-mcp") {
		t.Fatalf("refusal: %v", err)
	}
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", true, false)
	if err != nil || !strings.HasSuffix(line, "; run allowed by --allow-vulnerable-mcp") {
		t.Fatalf("flag override: %q %v", line, err)
	}
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", false, true)
	if err != nil || !strings.HasSuffix(line, "; run allowed by policy mcpServers.allowVulnerable") {
		t.Fatalf("policy override: %q %v", line, err)
	}
}

// With the gate's rating served (agentry-cli#501): a High the gate rates
// medium runs, one it rates high refuses and names the rating and the
// advisory, and the flag and the policy lift a high and a critical alike.
func TestVulnerabilityVerdictOnTheGatesRating(t *testing.T) {
	score := func(f float64) *float64 { return &f }
	medium := mcpinstall.VulnStatus{Ref: "npm:@modelcontextprotocol/sdk@1.25.2", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-71", Severity: "high", GateSeverity: "medium", CVSSScore: score(7.1)}}}
	high := mcpinstall.VulnStatus{Ref: "npm:@modelcontextprotocol/sdk@1.25.1", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-87", Severity: "high", GateSeverity: "high", CVSSScore: score(8.7), FixedIn: []string{"1.25.2"}}}}
	critical := mcpinstall.VulnStatus{Ref: "npm:@bytebase/dbhub@0.20.0", Status: "vulnerable", MaxSeverity: "critical",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-cr", Severity: "critical", GateSeverity: "critical", CVSSScore: score(9.8)}}}

	line, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{medium}, "https://reg.example", false, false)
	if err != nil || strings.Contains(line, "allowed by") {
		t.Fatalf("a medium runs with no override: %q %v", line, err)
	}
	_, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{medium, high}, "https://reg.example", false, false)
	if err == nil || !strings.Contains(err.Error(), "npm:@modelcontextprotocol/sdk@1.25.1 has an advisory the gate rates high (GHSA-87 [advisory high, CVSS 8.7]), fixed in 1.25.2") ||
		strings.Contains(err.Error(), "sdk@1.25.2 has") {
		t.Fatalf("high refusal: %v", err)
	}
	for _, st := range []mcpinstall.VulnStatus{high, critical} {
		if _, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{st}, "https://reg.example", false, false); err == nil {
			t.Fatalf("%s must refuse", st.Ref)
		}
		if line, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{st}, "https://reg.example", true, false); err != nil || !strings.HasSuffix(line, "; run allowed by --allow-vulnerable-mcp") {
			t.Fatalf("%s flag: %q %v", st.Ref, line, err)
		}
		if line, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{st}, "https://reg.example", false, true); err != nil || !strings.HasSuffix(line, "; run allowed by policy mcpServers.allowVulnerable") {
			t.Fatalf("%s policy: %q %v", st.Ref, line, err)
		}
	}
}

// TestRunPlanReportsEveryRefusal (agentry-cli#126): --plan on a local
// workspace (no attestation) whose manifest declares a server with a
// known critical advisory reports both refusals, in the order a run meets
// them, so a client that lifts the unsigned gate with the person's
// acceptance has already shown the vulnerability. refusal stays the first
// for clients that read one.
func TestRunPlanReportsEveryRefusal(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.scans = map[string]string{"@acme/osv-mcp": `{"status":"vulnerable","maxSeverity":"critical",` +
		`"advisories":[{"id":"GHSA-test-0001","severity":"critical","summary":"s","fixedIn":["1.2.4"]}]}`}
	prev := serverResolutions
	serverResolutions = func(context.Context, io.Writer, string, *policy.Policy, *manifest.Manifest) ([]mcpinstall.Resolved, error) {
		return []mcpinstall.Resolved{{Source: "npm", Package: "@acme/osv-mcp", Version: "1.2.3", ID: "npm-acme-osv-mcp",
			BinName: "osv-mcp", Integrity: "sha512-test"}}, nil
	}
	t.Cleanup(func() { serverResolutions = prev })

	ws := t.TempDir()
	for name, body := range skillFiles("demo", "capabilities:\n  mcpServers:\n    - npm: \"@acme/osv-mcp\"\n      version: 1.2.3\n") {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(extra ...string) (runPlanOutput, string) {
		t.Helper()
		args := append([]string{"--registry", fr.url(), "--json", "run", "--local", ws, "--plan", "--policy", policyPath}, extra...)
		stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
		if err != nil {
			t.Fatalf("plan: %v (stderr %q)", err, stderr)
		}
		var out runPlanOutput
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
		}
		return out, stdout
	}

	out, stdout := plan()
	if len(out.Refusals) != 2 || out.Refusals[0].Kind != refusalAttestation || out.Refusals[1].Kind != refusalVulnerableServer {
		t.Fatalf("refusals = %+v", out.Refusals)
	}
	if !strings.Contains(out.Refusals[0].Message, "--allow-unsigned") || !strings.Contains(out.Refusals[1].Message, "GHSA-test-0001") ||
		!strings.Contains(out.Refusals[1].Message, "--allow-vulnerable-mcp") {
		t.Fatalf("refusal messages = %+v", out.Refusals)
	}
	if out.Refusals[0].Lift != "--allow-unsigned" || out.Refusals[1].Lift != "--allow-vulnerable-mcp" {
		t.Fatalf("lifts = %+v", out.Refusals)
	}
	if out.Refusal != out.Refusals[0].Message {
		t.Fatalf("refusal = %q, want the first of refusals", out.Refusal)
	}
	if !strings.Contains(stdout, `"kind":"vulnerable_server"`) {
		t.Fatalf("kind on the wire:\n%s", stdout)
	}

	// With the unsigned gate lifted, the vulnerability is what remains.
	out, _ = plan("--allow-unsigned")
	if len(out.Refusals) != 1 || out.Refusals[0].Kind != refusalVulnerableServer || out.Refusal != out.Refusals[0].Message {
		t.Fatalf("with --allow-unsigned: %+v / %q", out.Refusals, out.Refusal)
	}
	// Both lifted: nothing to refuse, and neither field is on the wire.
	_, stdout = plan("--allow-unsigned", "--allow-vulnerable-mcp")
	if strings.Contains(stdout, `"refusal`) {
		t.Fatalf("a plan with nothing to refuse carries a refusal field:\n%s", stdout)
	}
}
