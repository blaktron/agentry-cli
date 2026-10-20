package engine

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestTheBrowserScopeLineSaysWhoAllowedThePublicWeb: a public scope the
// person agreed to for the Coding agent is not credited to the policy's key.
func TestTheBrowserScopeLineSaysWhoAllowedThePublicWeb(t *testing.T) {
	b := &browserRun{proxy: "p"}
	if l := browserScopeLine(&spec.BrowserCap{Public: true, Agreed: true}, b); !strings.Contains(l, "agreed to for this agent") || strings.Contains(l, "allowPublicWeb") {
		t.Errorf("agreed: %q", l)
	}
	if l := browserScopeLine(&spec.BrowserCap{Public: true}, b); !strings.Contains(l, "browser.allowPublicWeb") {
		t.Errorf("a release's: %q", l)
	}
}

// TestTheCodingAgentsProgramsAreNamedBesideItsPack: the receipt's knowledge
// line says the Coding agent's programs can read an attached pack.
func TestTheCodingAgentsProgramsAreNamedBesideItsPack(t *testing.T) {
	p := Params{
		Plan:      &spec.Plan{Capabilities: spec.CapSummary{Coder: &spec.CoderGrant{Commands: true}}},
		Knowledge: []AttachedPack{{Name: "notes", Digest: "sha256:ab", Pages: 1, Bytes: 1}},
	}
	if l := enforcedAtStart(p)["knowledge"]; !strings.Contains(l, "the Coding agent's programs run in that container") {
		t.Errorf("knowledge line: %q", l)
	}
	p.Plan.Capabilities.Coder = &spec.CoderGrant{}
	if l := enforcedAtStart(p)["knowledge"]; strings.Contains(l, "Coding agent") {
		t.Errorf("with its programs off: %q", l)
	}
}
