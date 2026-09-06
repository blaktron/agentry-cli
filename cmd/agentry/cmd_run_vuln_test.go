package main

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
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
