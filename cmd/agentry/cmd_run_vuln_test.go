package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The run's decision on the registry's verdict: a known high/critical
// advisory refuses unless the flag or the policy allows it, and the
// receipt line always says what was checked and why it ran.
func TestVulnerabilityVerdict(t *testing.T) {
	bad := mcpinstall.VulnStatus{Ref: "npm:minimist@1.2.5", Server: "npm:minimist", Status: "vulnerable", MaxSeverity: "critical",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-xvch-5gv4-984h", Severity: "critical", FixedIn: []string{"1.2.6"}}}}
	ok := mcpinstall.VulnStatus{Ref: "pypi:mcp-server-fetch@0.6.2", Status: "safe"}
	down := mcpinstall.VulnStatus{Ref: "npm:x@1", Status: "unreachable", Err: "dial tcp: refused"}

	line, err := vulnerabilityVerdict([]mcpinstall.VulnStatus{ok, down}, "https://reg.example", false, false)
	if err != nil || !strings.Contains(line, "checked against https://reg.example") || !strings.Contains(line, "pypi:mcp-server-fetch@0.6.2 no known advisories;") ||
		!strings.Contains(line, "npm:x@1 not checked: no answer from the registry (dial tcp: refused)") {
		t.Fatalf("clean+unreachable: %q %v", line, err)
	}
	_, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{ok, bad}, "https://reg.example", false, false)
	if err == nil || !strings.Contains(err.Error(), "npm:minimist@1.2.5 has a known critical vulnerability (GHSA-xvch-5gv4-984h), fixed in 1.2.6") || !strings.Contains(err.Error(), "--allow-vulnerable-mcp") {
		t.Fatalf("refusal: %v", err)
	}
	if got, want := receipt.ReasonOf(err), (&receipt.Reason{Code: receipt.ReasonMCPVulnerable, Names: []string{"npm:minimist"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reason = %+v, want %+v", got, want)
	}
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", true, false)
	if err != nil || !strings.HasSuffix(line, "; run allowed by --allow-vulnerable-mcp") {
		t.Fatalf("flag override: %q %v", line, err)
	}
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", false, true)
	if err != nil || !strings.HasSuffix(line, "; run allowed by policy mcpServers.allowVulnerable") {
		t.Fatalf("policy override: %q %v", line, err)
	}
	// Both overrides: the flag is the one named. Neither is named where
	// nothing needed one.
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", true, true)
	if err != nil || !strings.HasSuffix(line, "; run allowed by --allow-vulnerable-mcp") {
		t.Fatalf("both overrides: %q %v", line, err)
	}
	line, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{ok}, "https://reg.example", true, true)
	if err != nil || strings.Contains(line, "allowed by") {
		t.Fatalf("an override beside a clean server: %q %v", line, err)
	}
	// The refusal says how to lift it, both ways, and names every refusing
	// server, each with all the versions that fix it.
	_, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad}, "https://reg.example", false, false)
	if err == nil || !strings.HasSuffix(err.Error(), "pin a fixed version, or override with --allow-vulnerable-mcp or policy mcpServers.allowVulnerable") {
		t.Fatalf("how to lift the refusal: %v", err)
	}
	worse := mcpinstall.VulnStatus{Ref: "pypi:requests@2.0.0", Server: "pypi:requests", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []mcpinstall.VulnAdvisory{{ID: "GHSA-aaaa-bbbb-cccc", Severity: "high", FixedIn: []string{"2.0.1", "2.1.0"}}}}
	_, err = vulnerabilityVerdict([]mcpinstall.VulnStatus{bad, ok, worse}, "https://reg.example", false, false)
	if err == nil || !strings.Contains(err.Error(), "npm:minimist@1.2.5 has a known critical vulnerability (GHSA-xvch-5gv4-984h), fixed in 1.2.6; pypi:requests@2.0.0 has a known high vulnerability (GHSA-aaaa-bbbb-cccc), fixed in 2.0.1, 2.1.0 — pin") {
		t.Fatalf("two refusing servers: %v", err)
	}
	if got, want := receipt.ReasonOf(err), (&receipt.Reason{Code: receipt.ReasonMCPVulnerable, Names: []string{"npm:minimist", "pypi:requests"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reason = %+v, want %+v", got, want)
	}
}

// With the gate's rating served: a High the gate rates medium runs, one
// it rates high refuses and names the rating and the advisory, and the
// flag and the policy lift a high and a critical alike.
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

// TestRunPlanReportsEveryRefusal: --plan on a local workspace (no
// attestation) whose manifest declares a server with a known critical
// advisory reports both refusals, in the order a run meets them, so a
// client that lifts the unsigned gate with the person's acceptance has
// already shown the vulnerability. refusal stays the first for clients
// that read one.
func TestRunPlanReportsEveryRefusal(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.scans = map[string]string{"@acme/osv-mcp": `{"status":"vulnerable","maxSeverity":"critical",` +
		`"advisories":[{"id":"GHSA-test-0001","severity":"critical","summary":"s","fixedIn":["1.2.4"]}]}`}
	prev := serverResolutions
	var resolveErr error
	serverResolutions = func(context.Context, io.Writer, string, *policy.Policy, *manifest.Manifest) ([]mcpinstall.Resolved, error) {
		if resolveErr != nil {
			return nil, resolveErr
		}
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
	// The verdict is recorded in the plan, whatever the refusal.
	if !strings.Contains(string(out.Plan), "mcpServers.vulnerabilities") || !strings.Contains(string(out.Plan), "checked against "+fr.url()+": ") {
		t.Fatalf("the plan does not record the registry's verdict:\n%s", out.Plan)
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

	// A run, as against the preview, stops at the refusal: with the policy
	// class, naming the advisory, before anything is compiled.
	dry := func(extra ...string) (string, string, error) {
		args := append([]string{"--registry", fr.url(), "--json", "run", "--local", ws, "--dry-run", "--allow-unsigned", "--yes", "--policy", policyPath}, extra...)
		return runCLI(t, strings.NewReader(""), args...)
	}
	_, stderr, err := dry()
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "GHSA-test-0001") || !strings.Contains(err.Error(), "--allow-vulnerable-mcp") {
		t.Fatalf("a vulnerable server ran: %v", err)
	}
	if !strings.Contains(stderr, "declared MCP servers, per the registry: ") {
		t.Errorf("stderr does not say what the registry answered: %q", stderr)
	}
	// Lifted by the flag or by the policy, the run goes on, and the plan it
	// compiled says on what footing.
	enforcement := func(stdout string) string {
		t.Helper()
		var plan struct {
			Enforcement map[string]string `json:"enforcement"`
		}
		if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
			t.Fatalf("not the plan: %v\n%s", err, stdout)
		}
		return plan.Enforcement["mcpServers.vulnerabilities"]
	}
	stdout, _, err = dry("--allow-vulnerable-mcp")
	if err != nil || !strings.HasPrefix(enforcement(stdout), "checked against "+fr.url()+": ") || !strings.HasSuffix(enforcement(stdout), "; run allowed by --allow-vulnerable-mcp") {
		t.Fatalf("with the flag: %v\n%s", err, stdout)
	}
	allowing := policy.Default()
	allowing.MCPServers.AllowVulnerable = true
	if err := policy.Save(policyPath, allowing); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = dry()
	if err != nil || !strings.HasSuffix(enforcement(stdout), "; run allowed by policy mcpServers.allowVulnerable") {
		t.Fatalf("with the policy: %v\n%s", err, stdout)
	}
	// A server whose pins cannot be resolved fails the run's verification.
	resolveErr = errors.New("registry unreachable")
	_, _, err = dry("--allow-vulnerable-mcp")
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("err = %v", err)
	}
}
