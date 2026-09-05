package main

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

// parseCaps builds Capabilities from manifest YAML so the custom scalar
// forms ("none", "workspace", "*") exercise the real decoders.
func parseCaps(t *testing.T, capsYAML string) manifest.Capabilities {
	t.Helper()
	doc := `schemaVersion: "2"
name: capdemo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "cap rendering fixture"
publish:
  include: [agent.yaml, AGENT.md]
` + capsYAML
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
	return m.Capabilities
}

func TestCapabilityLinesZero(t *testing.T) {
	lines := capabilityLines(manifest.Capabilities{})
	if len(lines) != 1 || lines[0] != "none declared (pure text agent)" {
		t.Errorf("zero caps = %v", lines)
	}
	if capabilityNeedsConfirm(manifest.Capabilities{}) {
		t.Error("zero caps must not need confirmation")
	}
}

func TestCapabilityLinesFull(t *testing.T) {
	caps := parseCaps(t, `capabilities:
  network:
    egress: [api.pagerduty.com, api.example.com]
  filesystem:
    read: workspace
    write: none
  exec:
    commands: [git, jq]
  secrets: [PAGERDUTY_TOKEN]
  dataAccess: [tickets, email]
`)
	got := strings.Join(capabilityLines(caps), "\n")
	want := strings.Join([]string{
		"network.egress: api.pagerduty.com, api.example.com",
		"filesystem.read: workspace",
		"filesystem.write: none",
		"exec: commands: git, jq",
		"secrets: PAGERDUTY_TOKEN",
		"dataAccess: tickets, email",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if !capabilityNeedsConfirm(caps) {
		t.Error("egress + exec must need confirmation")
	}
}

func TestCapabilityLinesWildcardEgress(t *testing.T) {
	caps := parseCaps(t, `capabilities:
  network:
    egress: ["*"]
`)
	lines := capabilityLines(caps)
	if lines[0] != `network.egress: * (unrestricted — policy-gated)` {
		t.Errorf("wildcard egress line = %q", lines[0])
	}
	if !capabilityNeedsConfirm(caps) {
		t.Error("wildcard egress must need confirmation")
	}
}

func TestCapabilityNeedsConfirmTable(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{"egress none", "capabilities:\n  network:\n    egress: none\n", false},
		{"egress host", "capabilities:\n  network:\n    egress: [api.example.com]\n", true},
		{"exec none", "capabilities:\n  exec: none\n", false},
		{"exec commands", "capabilities:\n  exec:\n    commands: [git]\n", true},
		{"fs and secrets only", "capabilities:\n  filesystem:\n    read: workspace\n  secrets: [TOKEN]\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capabilityNeedsConfirm(parseCaps(t, tt.yaml)); got != tt.want {
				t.Errorf("needsConfirm = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDefaultTarget: the default install target follows runtime.interfaces
// (first emittable entry), never the run modes, and is claude-skill when
// nothing usable is declared.
func TestDefaultTarget(t *testing.T) {
	tests := []struct {
		interfaces []string
		want       string
	}{
		{nil, manifest.InterfaceClaudeSkill},
		{[]string{manifest.InterfaceClaudeSkill, manifest.InterfaceAgentsMD}, manifest.InterfaceClaudeSkill},
		{[]string{manifest.InterfaceAgentsMD, manifest.InterfaceClaudeSkill}, manifest.InterfaceAgentsMD},
		{[]string{"mcp", manifest.InterfaceAgentsMD}, manifest.InterfaceAgentsMD}, // v1 archive: the withdrawn target is skipped
		{[]string{"mcp"}, manifest.InterfaceClaudeSkill},
	}
	for _, tt := range tests {
		m := &manifest.Manifest{Modes: []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive}}
		m.Runtime.Interfaces = tt.interfaces
		if got := defaultTarget(m); got != tt.want {
			t.Errorf("defaultTarget(%v) = %s, want %s", tt.interfaces, got, tt.want)
		}
	}
}

// TestParseModes: the --modes grammar is the manifest's closed set.
func TestParseModes(t *testing.T) {
	got, err := parseModes(" interactive, autonomous ")
	if err != nil || len(got) != 2 || got[0] != manifest.ModeInteractive || got[1] != manifest.ModeAutonomous {
		t.Errorf("parseModes = %v, %v", got, err)
	}
	for _, bad := range []string{"", "daemon", "autonomous,autonomous", "autonomous,"} {
		if _, err := parseModes(bad); err == nil && bad != "autonomous," {
			t.Errorf("parseModes(%q) accepted", bad)
		}
	}
	if got := modesLabel([]manifest.Mode{}); got != "none" {
		t.Errorf("empty label = %q", got)
	}
}
