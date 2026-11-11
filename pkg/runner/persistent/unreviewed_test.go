package persistent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestWithoutAIReviewIsTheAgentsOwnSwitch: an agent that declares the web
// tools has a Without AI Review entry, off for every agent and asked about
// when switched on. With the web tools on and no TypeSafe key, the start's
// refusal names it; switched on, the segment's web tools run without a
// scanner and the plan says so. An agent without the web tools has no such
// entry.
func TestWithoutAIReviewIsTheAgentsOwnSwitch(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	noKey := Openers{
		Web: func(context.Context) (*engine.WebRun, error) { return nil, drafter.ErrNoJevKey },
		Unreviewed: func(context.Context) (*engine.WebRun, error) {
			return &engine.WebRun{Scan: toolsrv.WebScanUnreviewed}, nil
		},
	}
	isUnreviewed := func(e RosterEntry) bool { return e.ID == EntryUnreviewed }

	a := assistantAgent(t)
	c := Config{Enabled: true}
	e, ok := BuildRoster(a, c, nil).Entry(EntryUnreviewed)
	if !ok || e.On || e.Kind != KindTools || e.Reason != "" || !strings.Contains(e.Line, "unread") {
		t.Fatalf("the Assistant's Without AI Review entry: %+v (%v)", e, ok)
	}
	p := installPublished(t, home, "acme", "webby", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    web: true\n"})
	if e, ok := BuildRoster(p, Config{Enabled: true}, nil).Entry(EntryUnreviewed); !ok || e.On {
		t.Fatalf("a published agent's Without AI Review entry is on before the person switches it: %+v (%v)", e, ok)
	}
	if _, ok := BuildRoster(installPublished(t, home, "acme", "clocky", "1.0.0", publishedRelease{}), Config{Enabled: true}, nil).Entry(EntryUnreviewed); ok {
		t.Fatal("an agent without the web tools has a Without AI Review entry")
	}

	// The web tools on, the bypass off, no key: the refusal names the bypass.
	for _, id := range []string{EntryWeb} {
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), noKey, nil, fakeSecrets{})
	if err == nil || !strings.Contains(err.Error(), "agentry agent grant assistant unreviewed on") || !strings.Contains(err.Error(), "TypeSafe key") {
		t.Fatalf("the web tools with no key: %v", err)
	}

	// Switching the bypass on asks, and once agreed the segment has the web
	// tools with no scanner.
	on := Change{Switch: &Switch{ID: EntryUnreviewed, On: true}}
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, on); !errors.As(err, &ce) || !slices.ContainsFunc(ce.Entries, isUnreviewed) {
		t.Fatalf("the bypass switched on without agreement: %v", err)
	}
	on.Consent = true
	if _, err := ApplyChange(home, a, &c, on); err != nil {
		t.Fatal(err)
	}
	g, _, done, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), noKey, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Web == nil || g.Web.Scan != toolsrv.WebScanUnreviewed || g.Web.JevKey != "" {
		t.Fatalf("given %+v", g.Web)
	}
	hand := Config{Enabled: true, Switches: map[string]bool{EntryWeb: true, EntryUnreviewed: true}}
	if !slices.ContainsFunc(Unconsented(BuildRoster(a, hand, nil), hand), isUnreviewed) {
		t.Fatal("the bypass switched on by hand, with no agreement, is not held to consent")
	}

	// The plan of such a segment says its web tools run without AI Review.
	pc := Config{Enabled: true, Switches: map[string]bool{EntryWeb: true, EntryUnreviewed: true}}
	RecordConsent(BuildRoster(p, pc, nil), &pc)
	pg, _, pdone, err := ApplyWith(context.Background(), home, p, pc, BuildRoster(p, pc, nil), noKey, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	pdone()
	sp, err := localrun.Segment{Release: p.Release, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: pg}.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sp.Plan.Capabilities.WebTools || !sp.Plan.Capabilities.WebUnreviewed || !strings.Contains(sp.Plan.Enforcement["web"], "UNREAD by AI Review") {
		t.Fatalf("the plan: web %v, unreviewed %v, %q", sp.Plan.Capabilities.WebTools, sp.Plan.Capabilities.WebUnreviewed, sp.Plan.Enforcement["web"])
	}
}

// TestAnOrganizationThatLocksAIReviewForbidsTheBypass: while an
// organization's managed settings lock review.source, the entry carries the
// organization's reason, switching it on is refused, and a config switched
// on by hand stops the start, naming how to switch it off.
func TestAnOrganizationThatLocksAIReviewForbidsTheBypass(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	holdCreditsPaid(t, "https://registry.example", managedsettings.ReviewSource)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	e, _ := BuildRoster(a, c, nil).Entry(EntryUnreviewed)
	if !strings.Contains(e.Reason, "acme's managed settings lock how AI Review is done") {
		t.Fatalf("the entry under a lock: %+v", e)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryUnreviewed, On: true}, Consent: true}); err == nil || !strings.Contains(err.Error(), "acme's managed settings") {
		t.Fatalf("switching the bypass on under a lock: %v", err)
	}
	hand := Config{Enabled: true, Switches: map[string]bool{EntryWeb: true, EntryUnreviewed: true}}
	RecordConsent(BuildRoster(a, hand, nil), &hand)
	o := Openers{Unreviewed: func(context.Context) (*engine.WebRun, error) {
		return &engine.WebRun{Scan: toolsrv.WebScanUnreviewed}, nil
	}}
	if _, _, _, err := ApplyWith(context.Background(), home, a, hand, BuildRoster(a, hand, nil), o, nil, fakeSecrets{}); err == nil || !strings.Contains(err.Error(), "agentry agent grant assistant unreviewed off") {
		t.Fatalf("a hand-switched bypass under a lock: %v", err)
	}
}

// TestTheUnreviewedOpenerNeedsNoTypeSafeKey: the policy's opener gives the
// web tools without a scanner and without the person's TypeSafe key.
func TestTheUnreviewedOpenerNeedsNoTypeSafeKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	w, err := LocalUnreviewedWebOpener(policy.Default())(context.Background())
	if err != nil || w == nil || w.Scan != toolsrv.WebScanUnreviewed || w.JevKey != "" {
		t.Fatalf("%+v, %v", w, err)
	}
}
