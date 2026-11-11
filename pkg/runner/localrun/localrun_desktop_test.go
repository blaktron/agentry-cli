package localrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The desktop line a run's manifest carries is not part of the release's
// digest: a persistent agent's segment writes it from the switches the
// person has on. A plan is reused only while its desktop line is the one the
// manifest compiles to now, so a switch turned off is no longer bound and a
// switch turned on is.
func TestAPlanIsReusedOnlyWithTheDesktopLineItWasCompiledWith(t *testing.T) {
	computer := &manifest.Desktop{ComputerUse: manifest.ComputerUseApp, Servers: []manifest.DesktopServer{{ID: "blender"}}}
	noComputer := &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}}}
	twoApps := &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}, {ID: "unreal"}}}
	for _, tc := range []struct {
		name          string
		before, after *manifest.Desktop
		// What the plan holds after the change: a desktop line, computer
		// use, and how many applications.
		line, computerUse bool
		apps              int
	}{
		{"computer use switched off", computer, noComputer, true, false, 1},
		{"computer use switched on", noComputer, computer, true, true, 1},
		{"every entry switched off", computer, nil, false, false, 0},
		{"the first entry switched on", nil, computer, true, true, 1},
		{"an application added", noComputer, twoApps, true, false, 2},
		{"an application removed", twoApps, noComputer, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := desktopPlanInputs(t, tc.before)
			if _, _, err := EnsurePlan(in); err != nil {
				t.Fatal(err)
			}
			in.Manifest = withDesktopLine(in.Manifest, tc.after)
			plan, _, err := EnsurePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			d := plan.Capabilities.Desktop
			if (d != nil) != tc.line || d.HasComputerUse() != tc.computerUse || (d != nil && len(d.Servers) != tc.apps) {
				t.Fatalf("the plan's desktop line is %+v: the plan compiled before the change was reused", d)
			}
		})
	}
}

// A desktop line that did not change reuses the plan, whatever it holds:
// narrowed tools, an application the person chooses, the windows it
// reaches, and a screen scope the policy allows.
func TestAnUnchangedDesktopLineReusesThePlan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		line        func() *manifest.Desktop
		allowScreen bool
	}{
		{"app scope with narrowed tools and a chosen application", func() *manifest.Desktop {
			return &manifest.Desktop{ComputerUse: manifest.ComputerUseApp, Servers: []manifest.DesktopServer{
				{ID: "blender", Tools: []string{"get_scene_info", "execute_code"}},
				{ID: "unreal", Use: manifest.DesktopUseComputer},
				{Choose: true},
			}}
		}, false},
		{"computer use with no application named", func() *manifest.Desktop {
			return &manifest.Desktop{ComputerUse: manifest.ComputerUseApp}
		}, false},
		{"screen scope the policy allows", func() *manifest.Desktop {
			return &manifest.Desktop{ComputerUse: manifest.ComputerUseScreen}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := desktopPlanInputs(t, tc.line())
			in.Policy.Desktop.AllowScreen = tc.allowScreen
			first, planDir, err := EnsurePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			if !first.Capabilities.Desktop.HasComputerUse() {
				t.Fatalf("the first compile's desktop line: %+v", first.Capabilities.Desktop)
			}
			compiled, err := os.Stat(filepath.Join(planDir, "plan.json"))
			if err != nil {
				t.Fatal(err)
			}
			in.Manifest = withDesktopLine(in.Manifest, tc.line())
			if _, _, err := EnsurePlan(in); err != nil {
				t.Fatal(err)
			}
			reused, err := os.Stat(filepath.Join(planDir, "plan.json"))
			if err != nil || !os.SameFile(compiled, reused) {
				t.Fatalf("the plan was compiled again (%v)", err)
			}
		})
	}
}

// A plan whose desktop line the policy now refuses is not reused: the run
// compiles again and is refused by the policy.
func TestADesktopLineThePolicyNowRefusesIsNotReused(t *testing.T) {
	in := desktopPlanInputs(t, &manifest.Desktop{ComputerUse: manifest.ComputerUseApp, Servers: []manifest.DesktopServer{{ID: "blender"}}})
	if _, _, err := EnsurePlan(in); err != nil {
		t.Fatal(err)
	}
	off := false
	in.Policy.Desktop.Allow = &off
	_, _, err := EnsurePlan(in)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy {
		t.Fatalf("a desktop line the policy refuses: %v", err)
	}
	if want := policy.DesktopRefused; !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal %q does not say %q", err, want)
	}
}

// The Assistant's segment compiles with the desktop line its grants make
// now: computer use switched off after a segment that had it on is not in
// the next segment's plan.
func TestTheAssistantsNextSegmentDropsComputerUseSwitchedOff(t *testing.T) {
	home := t.TempDir()
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	segment := func(d *manifest.Desktop) *spec.Plan {
		t.Helper()
		sp, err := Segment{Builtin: Assistant(), Home: home, Policy: pol, PolicySHA: "sha", Build: "b1", Grants: Grants{Desktop: d}}.Plan(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return sp.Plan
	}
	if on := segment(&manifest.Desktop{ComputerUse: manifest.ComputerUseApp}); !on.Capabilities.Desktop.HasComputerUse() {
		t.Fatalf("the segment with computer use on: %+v", on.Capabilities.Desktop)
	}
	if off := segment(nil); off.Capabilities.Desktop != nil {
		t.Fatalf("the segment after computer use was switched off still has %+v", off.Capabilities.Desktop)
	}
}

// desktopPlanInputs are a run's plan inputs whose manifest carries the
// desktop line d, from a release that is not a built-in (a built-in is
// never granted the person's computer outside the persistent mode), with
// a policy of its own.
func desktopPlanInputs(t *testing.T, d *manifest.Desktop) PlanInputs {
	t.Helper()
	in := runPlanInputs(t, KnowledgeCreator(), io.Discard)
	in.Meta.Source = "local"
	in.Web, in.RegistrySearch, in.WebSearchedOnly, in.WebGivenOnly, in.WebRender, in.Review, in.Design = false, false, false, false, false, false, false
	pol := *in.Policy
	in.Policy = &pol
	in.Manifest = withDesktopLine(in.Manifest, d)
	return in
}

// withDesktopLine is a copy of m with the desktop line d.
func withDesktopLine(m *manifest.Manifest, d *manifest.Desktop) *manifest.Manifest {
	out := *m
	out.Capabilities.Desktop = d
	return &out
}

// The Coding agent's segment compiles with computer use beside its coding
// toolchain (its programs, open egress and the browser), with the computer
// use entry's tools; without the grant it has none.
func TestTheCodingAgentsSegmentTakesComputerUseBesideItsToolchain(t *testing.T) {
	home := t.TempDir()
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	coder := &spec.CoderGrant{Commands: true, OpenEgress: true, Browser: true}
	segment := func(d *manifest.Desktop) *spec.Plan {
		t.Helper()
		sp, err := Segment{Builtin: Coder(), Home: home, Policy: pol, PolicySHA: "sha", Build: "b1", Grants: Grants{Coder: coder, Desktop: d}}.Plan(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return sp.Plan
	}
	on := segment(&manifest.Desktop{ComputerUse: manifest.ComputerUseApp})
	if d := on.Capabilities.Desktop; !d.HasComputerUse() || len(d.ComputerTools) == 0 {
		t.Fatalf("the Coding agent's segment with computer use on: %+v", d)
	}
	if off := segment(nil); off.Capabilities.Desktop != nil {
		t.Fatalf("the Coding agent's segment without the grant has %+v", off.Capabilities.Desktop)
	}
}
