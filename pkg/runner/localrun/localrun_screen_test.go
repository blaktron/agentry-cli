package localrun

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// A plan whose screen scope only --allow-screen allowed is reused only by a
// run with the flag; one the policy or Unsafe Mode allowed, or with no
// screen scope, is reused either way.
func TestAScreenScopeTheFlagAllowedIsReusedOnlyWithIt(t *testing.T) {
	plan := func(by string) *spec.Plan {
		return &spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{ComputerUse: "screen", ScreenAllowedBy: by}}}
	}
	if screenStillAllowed(plan(spec.ScreenByFlag), false) || !screenStillAllowed(plan(spec.ScreenByFlag), true) {
		t.Error("the flag's plan")
	}
	for _, p := range []*spec.Plan{plan(spec.ScreenByPolicy), plan(spec.ScreenByUnsafe), {}} {
		if !screenStillAllowed(p, false) {
			t.Errorf("%+v is not reused", p.Capabilities.Desktop)
		}
	}
}
