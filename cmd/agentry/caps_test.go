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
	doc := `schemaVersion: "1"
name: capdemo
namespace: acme
kind: skill
version: 1.0.0
summary: "cap rendering fixture"
entrypoints:
  skill: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
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

func TestDefaultTarget(t *testing.T) {
	tests := []struct {
		kind manifest.Kind
		want string
	}{
		{manifest.KindSkill, manifest.InterfaceClaudeSkill},
		{manifest.KindMCPServer, manifest.InterfaceMCP},
		{manifest.KindPromptAgent, manifest.InterfaceAgentsMD},
	}
	for _, tt := range tests {
		if got := defaultTarget(tt.kind); got != tt.want {
			t.Errorf("defaultTarget(%s) = %s, want %s", tt.kind, got, tt.want)
		}
	}
}
