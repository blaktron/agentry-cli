package persistent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// stubApplications has the roster read these as the person's approved
// applications, as Agentry Desktop lists them, for the test; none and an
// error is an Agentry Desktop that does not answer.
func stubApplications(t *testing.T, err error, apps ...desktop.Application) {
	t.Helper()
	prev := listApplications
	listApplications = func() ([]desktop.Application, error) { return apps, err }
	t.Cleanup(func() { listApplications = prev })
}

var (
	listedBlender = desktop.Application{ID: "blender", Name: "Blender", From: desktop.FromLibrary, MCP: true, Ready: true}
	listedUnreal  = desktop.Application{ID: "unreal", Name: "Unreal Engine", From: desktop.FromLibrary, MCP: true}
)

// A running agent's desktop entry switched on reads pending until the next
// turn's segment, which Apply gives the application; switched off, the next
// segment has none. Nothing asks for the agent to stop.
func TestADesktopEntrySwitchesWhileTheAgentRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	stubApplications(t, nil, listedBlender)
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
	stubApplications(t, nil, listedBlender)
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
// and is given all the same: its calls are refused until one does. Agentry
// Desktop not answering lists nothing, and the application switched on is
// still offered.
func TestADesktopEntryNeedsAgentryDesktopAndIsGivenAllTheSame(t *testing.T) {
	stubApplications(t, desktop.ErrNoService)
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
	for _, e := range desktopEntries(pub, nil) {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{DesktopEntryID("unreal"), DesktopExecID("unreal")}) {
		t.Fatalf("a release that narrows to unreal is offered %v", ids)
	}
	if es := desktopEntries(Agent{Name: "x", Release: &localrun.Installed{}, Caps: &manifest.Persistent{Clock: true}}, nil); len(es) != 0 {
		t.Fatalf("an agent that does not declare the key is offered %+v", es)
	}
}

// Without a release line the agent is offered the applications the person
// approved for their MCP servers, as Agentry Desktop lists them, by their
// names: one approved for its windows alone is not an MCP entry. An older
// Agentry Desktop that lists nothing leaves only computer use.
func TestTheRosterOffersThePersonsApplications(t *testing.T) {
	a := Agent{Name: "acme.helper", Release: &localrun.Installed{}, Caps: &manifest.Persistent{Desktop: true}}
	stubApplications(t, nil, listedBlender, desktop.Application{ID: "kdenlive", Name: "Kdenlive", From: desktop.FromPerson, Computer: true},
		desktop.Application{ID: "my-cad", Name: "My CAD", From: desktop.FromPerson, MCP: true})
	var got []string
	for _, e := range desktopEntries(a, nil) {
		got = append(got, e.ID+"="+e.Name)
	}
	want := []string{"desktop:blender=Blender on your computer", "desktop:blender:exec=Blender: code without asking",
		"desktop:my-cad=My CAD on your computer", "desktop:my-cad:exec=My CAD: code without asking", "desktop:computer=Computer use"}
	if !slices.Equal(got, want) {
		t.Fatalf("offered %v, want %v", got, want)
	}
	// A switch keeps its application offered while Agentry Desktop lists
	// nothing; a switch whose id cannot be an application's offers nothing.
	stubApplications(t, desktop.ErrNoListing)
	got = nil
	switches := map[string]bool{DesktopEntryID("my-cad"): true, DesktopEntryID("chosen"): true, DesktopEntryID("Not An ID"): true}
	for _, e := range desktopEntries(a, switches) {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, []string{DesktopEntryID("my-cad"), DesktopExecID("my-cad"), EntryComputer}) {
		t.Fatalf("switched on while Agentry Desktop lists nothing: %v", got)
	}
	got = nil
	for _, e := range desktopEntries(a, nil) {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, []string{EntryComputer}) {
		t.Fatalf("with an Agentry Desktop that lists nothing: %v", got)
	}
}

// TestTheCodingAgentIsOfferedThePersonsComputer: the Coding agent declares
// the person's computer, so its roster offers each approved application and
// computer use, all off; computer use switched on asks the person, and once
// agreed the next segment is given computer use in app scope beside the
// coding toolchain; switched off, it has none.
func TestTheCodingAgentIsOfferedThePersonsComputer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	stubApplications(t, nil, listedBlender)
	a := coderAgent(t)
	if !a.UsesDesktop() {
		t.Fatal("the Coding agent does not declare the person's computer")
	}
	c := Config{Enabled: true}
	r := BuildRoster(a, c, nil)
	for _, id := range []string{DesktopEntryID("blender"), DesktopExecID("blender"), EntryComputer} {
		if e, ok := r.Entry(id); !ok || e.On {
			t.Fatalf("%s: %+v offered %v, want offered and off", id, e, ok)
		}
	}
	_, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryComputer, On: true}})
	var ce *ConsentError
	if !errors.As(err, &ce) || len(ce.Entries) != 1 || ce.Entries[0].ID != EntryComputer {
		t.Fatalf("switching computer use on without agreeing: %v", err)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryComputer, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	g := desktopGrant(a, c.Switches, func(id string) bool { return c.Switches[id] })
	if g == nil || g.Line.ComputerUse != manifest.ComputerUseApp || len(g.Line.Servers) != 0 {
		t.Fatalf("the next segment's desktop line: %+v", g)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryComputer, On: false}}); err != nil {
		t.Fatal(err)
	}
	if g := desktopGrant(a, c.Switches, func(id string) bool { return c.Switches[id] }); g != nil {
		t.Fatalf("switched off, the next segment still has %+v", g.Line)
	}
}
