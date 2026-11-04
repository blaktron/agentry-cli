package persistent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// startKeyed starts the Assistant with the test's own keychain, packs and
// contents.
func startKeyed(t *testing.T, home string, st *starter, keys Secrets, packs Packs, cs Contents) *Supervisor {
	t.Helper()
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start, Secrets: keys, Packs: packs, Contents: cs,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

// TestAnImageServerSwitchedOnWhileRunningJoinsAtTheNextTurn: Google
// Workspace, signed in and switched on while the Assistant runs, is pending
// (never installing, since it installs nothing) and the next turn starts a
// segment that is given it.
func TestAnImageServerSwitchedOnWhileRunningJoinsAtTheNextTurn(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	enable(t, home)
	c, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	c.MailProviders = googleOn
	c.SignIns = map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now()}}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword}, nil, Contents{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments[0].Grants.ImageServers) != 0 {
		t.Fatal("the first segment was given Google Workspace switched off")
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: "server:gog", On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if e, _ := sv.Roster().Entry("server:gog"); e.State != StatePending || !sv.Status().Pending {
		t.Fatalf("switched on while running: %+v, pending %v", e, sv.Status().Pending)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 || len(st.segments[1].Grants.ImageServers) != 1 || st.segments[1].Grants.ImageServers[0].Key != "gog" {
		t.Fatalf("segments %d; the next was given %+v", len(st.segments), st.segments[len(st.segments)-1].Grants.ImageServers)
	}
	if e, _ := sv.Roster().Entry("server:gog"); e.State != StateOn || sv.Status().Pending {
		t.Errorf("after the turn: %+v, pending %v", e, sv.Status().Pending)
	}
	// Signed in again, as another account: still on, and given anew.
	c, err = LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	c.SignIns["gog"] = ImageSignIn{Account: "other@example.com", At: time.Now()}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	if e, _ := sv.Roster().Entry("server:gog"); e.State != StatePending || !sv.Status().Pending {
		t.Fatalf("signed in again: %+v, pending %v", e, sv.Status().Pending)
	}
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 3 || st.segments[2].Grants.ImageServers[0].Account != "other@example.com" {
		t.Fatalf("segments %d after signing in again", len(st.segments))
	}
}

// TestAnImageServersSetUpCountsInTheKey: what an image server is given and
// when it was signed in count in the key, so a set-up done while the agent
// runs restarts the segment; a server switched on before its sign-in gives
// nothing, and restarts nothing.
func TestAnImageServersSetUpCountsInTheKey(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a := assistantAgent(t)
	base := Config{Enabled: true, MailProviders: []string{"microsoft"}, Switches: map[string]bool{"server:ms365": true, "server:ms365:writes": true},
		SignIns: map[string]ImageSignIn{"ms365": {ClientID: testEntraClient, Tenant: TenantOrganizations, Account: "person@contoso.com",
			At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Granted: []string{"writes"}}}}
	key := func(edit func(*ImageSignIn)) string {
		c := base
		c.SignIns = map[string]ImageSignIn{"ms365": base.SignIns["ms365"]}
		in := c.SignIns["ms365"]
		edit(&in)
		c.SignIns["ms365"] = in
		return grantsKey(home, a, c)
	}
	was := key(func(*ImageSignIn) {})
	for name, edit := range map[string]func(*ImageSignIn){
		"signed in again":    func(in *ImageSignIn) { in.At = in.At.Add(time.Hour) },
		"another account":    func(in *ImageSignIn) { in.Account = "other@contoso.com" },
		"another client":     func(in *ImageSignIn) { in.ClientID = "00000000-0000-4000-8000-000000000001" },
		"another tenant":     func(in *ImageSignIn) { in.Tenant = "contoso.onmicrosoft.com" },
		"a signoff":          func(in *ImageSignIn) { in.Signoff = "Sent by my assistant" },
		"a read-only scope":  func(in *ImageSignIn) { in.Granted = nil },
		"the client set now": func(in *ImageSignIn) { in.Client = time.Now() },
	} {
		if key(edit) == was {
			t.Errorf("%s: the key did not change", name)
		}
	}
	waiting := Config{Enabled: true, MailProviders: []string{"microsoft"}}
	off := grantsKey(home, a, waiting)
	waiting.Switches = map[string]bool{"server:ms365": true}
	if grantsKey(home, a, waiting) != off {
		t.Error("a server switched on before its sign-in changed the key")
	}
}

// TestADevServerSavedAgainRestartsTheSegment: saving the Coding agent's dev
// server again, its sign-in perhaps new, restarts the segment though the
// host and its key are the same; a client's own stamp is not kept, and a
// dev server switched off is not followed.
func TestADevServerSavedAgainRestartsTheSegment(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	d := devServer
	d.SavedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{DevHost: &d, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if c.DevHost == nil || c.DevHost.SavedAt.IsZero() || c.DevHost.SavedAt.Equal(d.SavedAt) {
		t.Fatalf("the dev server was saved as %+v", c.DevHost)
	}
	// As if saved an hour ago, so the next save's stamp differs whatever
	// the clock's resolution.
	c.DevHost.SavedAt = c.DevHost.SavedAt.Add(-time.Hour)
	before := configSignatures(a, c, BuildRoster(a, c, nil))
	restart, err := ApplyChange(home, a, &c, Change{DevHost: &devServer, Consent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Error("the dev server saved again restarts nothing")
	}
	if got := before.changed(configSignatures(a, c, BuildRoster(a, c, nil))); !slices.Equal(got, []string{EntryDevHost}) {
		t.Errorf("the entries given anew: %v", got)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryDevHost, On: false}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := configSignatures(a, c, BuildRoster(a, c, nil))[EntryDevHost]; ok {
		t.Error("a dev server switched off is followed")
	}
}

// fakeStore is a local store whose packs' digests a test moves.
type fakeStore struct {
	mu    sync.Mutex
	packs map[string]string
}

func (f *fakeStore) contents() Contents {
	return Contents{Pack: func(_ context.Context, name string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		d, ok := f.packs[name]
		if !ok {
			return "", errors.New("no such pack")
		}
		return d, nil
	}}
}

func (f *fakeStore) set(name, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if digest == "" {
		delete(f.packs, name)
		return
	}
	f.packs[name] = digest
}

func (f *fakeStore) open() Packs {
	return func(_ context.Context, names []string) ([]engine.AttachedPack, func(), error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make([]engine.AttachedPack, 0, len(names))
		for _, n := range names {
			out = append(out, engine.AttachedPack{Name: n, Digest: f.packs[n]})
		}
		return out, func() {}, nil
	}
}

// TestAPackBuiltAgainReachesTheNextTurn: a knowledge pack the person
// attached and builds again while the agent runs is pending, and the next
// turn starts a segment that attaches the new build; a pack switched off
// is not followed.
func TestAPackBuiltAgainReachesTheNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	store := &fakeStore{packs: map[string]string{"handbook": "sha256:one"}}
	c, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	a := assistantAgent(t)
	if _, err := ApplyChange(home, a, &c, Change{AddKnowledge: "handbook", Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, nil, store.open(), store.contents())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if sv.Status().Pending {
		t.Fatal("pending before anything changed")
	}
	store.set("handbook", "sha256:two")
	if e, _ := sv.Roster().Entry(KnowledgeEntryID("handbook")); !sv.Status().Pending || e.State != StatePending {
		t.Fatalf("a pack built again: pending %v, its entry %s", sv.Status().Pending, e.State)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	digests := func(i int) []string {
		var out []string
		for _, p := range st.segments[i].Grants.Knowledge {
			out = append(out, p.Digest)
		}
		return out
	}
	if len(st.segments) != 2 || !slices.Equal(digests(1), []string{"sha256:two"}) {
		t.Fatalf("segments %d; the last attached %v", len(st.segments), digests(len(st.segments)-1))
	}
	if sv.Status().Pending {
		t.Error("pending after the turn that applied it")
	}
	// A pack gone from the store is not a change: the running segment
	// keeps the copy it attached, and the agent keeps running.
	store.set("handbook", "")
	if sv.Status().Pending {
		t.Error("a pack gone from the store is pending")
	}
	if err := sv.Send(ctx, "still here"); err != nil || len(st.segments) != 2 {
		t.Fatalf("a turn after the pack went: segments %d, %v", len(st.segments), err)
	}
	store.set("handbook", "sha256:two")
	if _, err := sv.Change(Change{Switch: &Switch{ID: KnowledgeEntryID("handbook"), On: false}}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	store.set("handbook", "sha256:three")
	if sv.Status().Pending {
		t.Error("a pack switched off is followed")
	}
}

// TestAChangeThroughTheSocketReachesARunningAgent: Configure, as a client
// calls it, reaches a running agent through its owner socket, and the next
// turn applies it.
func TestAChangeThroughTheSocketReachesARunningAgent(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	defer func() { _, _ = sv.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	r, running, err := Configure(ctx, home, assistantAgent(t), ClientCLI, Change{Switch: &Switch{ID: EntryShare, On: false}})
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("Configure did not reach the running agent")
	}
	if e, _ := r.Entry(EntryShare); e.State != StatePending {
		t.Fatalf("the share through the socket: %+v", e)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 || st.segments[1].Grants.Share != "" {
		t.Fatalf("segments %d", len(st.segments))
	}
}
