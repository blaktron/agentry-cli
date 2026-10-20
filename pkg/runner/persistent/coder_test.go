package persistent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

func coderAgent(t *testing.T) Agent {
	t.Helper()
	a, err := Lookup("coder")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// devServer is a dev server as config.yaml keeps it.
var devServer = DevHost{Host: "deploy@devbox.example", HostKey: "SHA256:" + strings.Repeat("A", 43)}

// TestCoderEgressConsentRecorded: the Coding agent's open egress, its
// programs and its browser are on by default and asked for before its first
// segment; once agreed, the segment is given them and its receipt's roster
// records the egress entry on, with the sentence the person agreed to.
func TestCoderEgressConsentRecorded(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := coderAgent(t)
	if err := SaveConfig(home, a.Name, Config{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	opts := Options{Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default(), Docker: &dockerx.Fake{}}
	st := &starter{}
	opts.StartSegment = st.start
	var ce *ConsentError
	if _, err := Start(context.Background(), opts); !errors.As(err, &ce) {
		t.Fatalf("the first start before any agreement: %v", err)
	}
	var asked []string
	for _, e := range ce.Entries {
		asked = append(asked, e.ID)
	}
	for _, id := range []string{EntryShare, EntryCommands, EntryEgress, EntryBrowser} {
		if !slices.Contains(asked, id) {
			t.Errorf("the start did not ask for %s (asked %v)", id, asked)
		}
	}
	if len(st.segments) != 0 {
		t.Fatal("a segment started before the person agreed")
	}

	c, err := LoadConfig(home, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	RecordConsent(r, &c)
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	sv, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	seg := st.segments[0]
	if g := seg.Grants.Coder; g == nil || !g.Commands || !g.OpenEgress || !g.Browser || g.DevHost || seg.Grants.DevHost != nil {
		t.Fatalf("the segment's grant: %+v, dev server %+v", seg.Grants.Coder, seg.Grants.DevHost)
	}
	egress, _ := r.Entry(EntryEgress)
	var recorded bool
	for _, e := range seg.Persistent.Roster {
		if e.ID == EntryEgress {
			recorded = e.On && e.Line == egress.Line && e.Line == c.Consented[EntryEgress] && strings.Contains(e.Line, "open egress")
		}
	}
	if !recorded {
		t.Errorf("the receipt's roster does not record the agreed open egress: %+v", seg.Persistent.Roster)
	}
}

// TestTheAssistantIsNeverGivenTheCodingAgentsGrants: the Assistant's roster
// has none of the Coding agent's entries, a switch or a dev server for it is
// refused, and Apply gives it no grant of the Coding agent's.
func TestTheAssistantIsNeverGivenTheCodingAgentsGrants(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := assistantAgent(t)
	c := Config{Enabled: true}
	r := BuildRoster(a, c, nil)
	for _, id := range []string{EntryCommands, EntryEgress, EntryBrowser, EntryDevHost} {
		if _, ok := r.Entry(id); ok {
			t.Errorf("the Assistant's roster has %s", id)
		}
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryEgress, On: true}, Consent: true}); err == nil {
		t.Error("the Assistant was switched to open egress")
	}
	d := devServer
	if _, err := ApplyChange(home, a, &c, Change{DevHost: &d, Consent: true}); err == nil {
		t.Error("the Assistant was given a dev server")
	}
	c.DevHost = &d
	g, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, func(context.Context, DevHost) (*targethost.Host, error) {
		t.Error("the Assistant's dev server was readied")
		return nil, errors.New("no")
	})
	if err != nil || g.Coder != nil || g.DevHost != nil {
		t.Errorf("the Assistant's grants: %+v, %v", g, err)
	}
}

// TestTheDevServerIsSavedPinnedAndReadiedEachSegment: a dev server is saved
// only in its checked form and asked for like any widening; Apply readies
// it from that form when its entry is on, and not when it is off; a change
// to it restarts a running agent; removing it takes its agreement away.
func TestTheDevServerIsSavedPinnedAndReadiedEachSegment(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := coderAgent(t)
	c := Config{Enabled: true}
	RecordConsent(BuildRoster(a, c, nil), &c)

	for name, bad := range map[string]DevHost{
		"a password in the host": {Host: "deploy:pw@devbox.example", HostKey: devServer.HostKey},
		"a port written as 22":   {Host: "deploy@devbox.example:22", HostKey: devServer.HostKey},
		"no host key":            {Host: devServer.Host},
		"an MD5 host key":        {Host: devServer.Host, HostKey: "MD5:aa:bb"},
	} {
		d := bad
		if _, err := ApplyChange(home, a, &c, Change{DevHost: &d, Consent: true}); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	d := devServer
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, Change{DevHost: &d}); !errors.As(err, &ce) || len(ce.Entries) != 1 || ce.Entries[0].ID != EntryDevHost ||
		!strings.Contains(ce.Entries[0].Line, devServer.Host) || !strings.Contains(ce.Entries[0].Line, devServer.HostKey) {
		t.Fatalf("a dev server without agreement: %v", err)
	}
	restart, err := ApplyChange(home, a, &c, Change{DevHost: &d, Consent: true})
	if err != nil || !restart {
		t.Fatalf("saving the dev server: restart %v, %v", restart, err)
	}

	var readied []DevHost
	host := &targethost.Host{ID: spec.DevHostID}
	ready := func(_ context.Context, got DevHost) (*targethost.Host, error) {
		readied = append(readied, got)
		return host, nil
	}
	g, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, ready)
	if err != nil || g.Coder == nil || !g.Coder.DevHost || g.DevHost != host || len(readied) != 1 || readied[0] != devServer {
		t.Fatalf("with the dev server on: grant %+v, host %v, readied %v, %v", g.Coder, g.DevHost, readied, err)
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, func(context.Context, DevHost) (*targethost.Host, error) {
		return nil, errors.New("host key changed")
	}); err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("a dev server that would not ready: %v", err)
	}

	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryDevHost, On: false}}); err != nil {
		t.Fatal(err)
	}
	readied = nil
	g, _, _, err = Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, ready)
	if err != nil || g.Coder.DevHost || g.DevHost != nil || len(readied) != 0 {
		t.Fatalf("with the dev server off: %+v, %v, readied %v", g.Coder, err, readied)
	}

	if _, err := ApplyChange(home, a, &c, Change{ClearDevHost: true}); err != nil || c.DevHost != nil || c.Consented[EntryDevHost] != "" {
		t.Fatalf("removing the dev server: %+v, %v", c, err)
	}
	if _, err := ApplyChange(home, a, &c, Change{ClearDevHost: true}); err == nil {
		t.Error("a dev server that is not there was removed")
	}
}

// TestTheCodingAgentsPackLineSaysItsProgramsReadIt: a pack the Coding agent
// searches is readable by the programs it runs, and its consent sentence
// says so, as the Assistant's does not need to.
func TestTheCodingAgentsPackLineSaysItsProgramsReadIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Config{Knowledge: []string{"notes"}}
	e, _ := BuildRoster(coderAgent(t), c, nil).Entry(KnowledgeEntryID("notes"))
	if !strings.Contains(e.Line, "programs it runs can read the pack") {
		t.Errorf("the Coding agent's pack line: %q", e.Line)
	}
	e, _ = BuildRoster(assistantAgent(t), c, nil).Entry(KnowledgeEntryID("notes"))
	if strings.Contains(e.Line, "programs") {
		t.Errorf("the Assistant's pack line: %q", e.Line)
	}
}
