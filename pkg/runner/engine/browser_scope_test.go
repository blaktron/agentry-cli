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
