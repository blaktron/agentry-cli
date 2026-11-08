package persistent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// A running agent's desktop entry switched on reads pending until the next
// turn's segment, which Apply gives the application; switched off, the next
// segment has none. Nothing asks for the agent to stop.
func TestADesktopEntrySwitchesWhileTheAgentRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	applied := &Applied{}
	if e, ok := BuildRoster(a, c, applied).Entry(DesktopEntryID("blender")); !ok || e.On || e.State != StateOff {
		t.Fatalf("Blender's entry before the switch: %+v %v", e, ok)
	}
	_, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: DesktopEntryID("blender"), On: true}})
	var ce *ConsentError
	if !errors.As(err, &ce) || len(ce.Entries) != 1 || ce.Entries[0].ID != DesktopEntryID("blender") {
		t.Fatalf("switching Blender on without agreeing: %v", err)
	}
	restart, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: DesktopEntryID("blender"), On: true}, Consent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("the switch is not a change the next turn applies")
	}
	if e, _ := BuildRoster(a, c, applied).Entry(DesktopEntryID("blender")); e.State != StatePending {
		t.Fatalf("Blender switched on while the agent runs reads %s, want pending", e.State)
	}
	r := BuildRoster(a, c, nil)
	g, ap, done, err := Apply(context.Background(), home, a, c, r, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Desktop == nil || len(g.Desktop.Servers) != 1 || g.Desktop.Servers[0].ID != "blender" || g.Desktop.ComputerUse != "" || len(g.DesktopExec) != 0 {
		t.Fatalf("the next segment's desktop line: %+v exec %v", g.Desktop, g.DesktopExec)
	}
	if !slices.Contains(ap.On, DesktopEntryID("blender")) {
		t.Fatalf("applied %v", ap.On)
	}
	if e, _ := BuildRoster(a, c, &ap).Entry(DesktopEntryID("blender")); e.State == StatePending {
		t.Fatal("still pending once the segment has it")
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: DesktopEntryID("blender"), On: false}}); err != nil {
		t.Fatal(err)
	}
	g, _, done, err = Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Desktop != nil {
		t.Fatalf("switched off, the next segment still has %+v", g.Desktop)
	}
}

// The exec agreement is a switch of its own, given only with its
// application; off, the application's code tools ask each time.
func TestTheExecAgreementIsGivenWithItsApplicationAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	a := assistantAgent(t)
	c := Config{Enabled: true, Switches: map[string]bool{DesktopExecID("blender"): true}}
	g, ap, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Desktop != nil || slices.Contains(ap.On, DesktopExecID("blender")) {
		t.Fatalf("an exec agreement without its application gave %+v, applied %v", g.Desktop, ap.On)
	}
	c.Switches[DesktopEntryID("blender")] = true
	c.Switches[EntryComputer] = true
	g, _, done, err = Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Desktop == nil || g.Desktop.ComputerUse != manifest.ComputerUseApp || !slices.Equal(g.DesktopExec, []string{"blender"}) {
		t.Fatalf("with Blender, its agreement and computer use on: %+v exec %v", g.Desktop, g.DesktopExec)
	}
	// A configuration switched on by hand, never agreed to, is what the
	// next start asks about.
	var unagreed []string
	for _, e := range Unconsented(BuildRoster(a, c, nil), c) {
		unagreed = append(unagreed, e.ID)
	}
	for _, id := range []string{DesktopEntryID("blender"), DesktopExecID("blender"), EntryComputer} {
		if !slices.Contains(unagreed, id) {
			t.Errorf("%s switched on by hand is not asked about: %v", id, unagreed)
		}
	}
}

// An entry that is on reads needs-set-up while no Agentry Desktop answers,
// and is given all the same: its calls are refused until one does.
func TestADesktopEntryNeedsAgentryDesktopAndIsGivenAllTheSame(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	a := assistantAgent(t)
	c := Config{Enabled: true, Switches: map[string]bool{DesktopEntryID("blender"): true}}
	e, _ := BuildRoster(a, c, nil).Entry(DesktopEntryID("blender"))
	if e.State != StateNeedsSetUp || e.Reason != desktopNotAnswering {
		t.Fatalf("with no Agentry Desktop: %+v", e)
	}
	g, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Desktop == nil {
		t.Fatal("an entry that needs Agentry Desktop was left out of the segment")
	}
}

// A published agent's release line narrows what it is offered; an agent
// that does not declare the key is offered nothing.
func TestTheReleaseLineNarrowsTheDesktopEntries(t *testing.T) {
	pub := Agent{Name: "acme.helper", Release: &localrun.Installed{}, Caps: &manifest.Persistent{Desktop: true},
		DesktopLine: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "unreal"}}}}
	var ids []string
	for _, e := range desktopEntries(pub) {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{DesktopEntryID("unreal")}) {
		t.Fatalf("a release that narrows to Unreal Engine is offered %v", ids)
	}
	if es := desktopEntries(Agent{Name: "x", Release: &localrun.Installed{}, Caps: &manifest.Persistent{Clock: true}}); len(es) != 0 {
		t.Fatalf("an agent that does not declare the key is offered %+v", es)
	}
}
