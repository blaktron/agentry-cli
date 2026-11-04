package persistent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// withPacks refuses a segment that attaches a pack, as an engine that
// cannot start one does.
func withPacks(seg localrun.Segment) error {
	if len(seg.Grants.Knowledge) > 0 {
		return errors.New("the engine could not start it")
	}
	return nil
}

// refuseWith has st refuse each segment refuse answers an error for.
func refuseWith(st *starter, refuse func(localrun.Segment) error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.refuse = refuse
}

// told drains what the chat was sent so far and says whether a notice
// began with prefix.
func told(events <-chan session.Event, prefix string) bool {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if ev.Kind == session.KindNotice && strings.HasPrefix(ev.Text, prefix) {
				return true
			}
		default:
			return false
		}
	}
}

// TestAChangeThatAddsWhatTheNextSegmentCannotStartWithIsTakenBack: a pack
// added while the agent runs, whose segment cannot start, is switched off;
// the agent goes on without it and answers the turn there once, the chat
// and the envelope say so, and the next change to what the agent is given
// clears the record, while a name the agent sets itself does not.
func TestAChangeThatAddsWhatTheNextSegmentCannotStartWithIsTakenBack(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	store := &fakeStore{packs: map[string]string{"handbook": "sha256:one"}}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, nil, store.open(), store.contents())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	defer detach()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(Change{AddKnowledge: "handbook", Name: new("Robin"), Consent: true}); err != nil {
		t.Fatal(err)
	}
	refuseWith(st, withPacks)
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatalf("the turn after a change that could not start: %v", err)
	}
	select {
	case <-sv.Done():
		t.Fatal("the agent stopped for a change")
	default:
	}
	st.mu.Lock()
	segments, last := len(st.segments), st.segments[len(st.segments)-1]
	sent := st.last.sentTurns()
	st.mu.Unlock()
	if segments != 2 || len(last.Grants.Knowledge) != 0 {
		t.Fatalf("segments %d; the agent went on with the pack", segments)
	}
	if len(sent) != 1 || !strings.HasSuffix(sent[0], "second") {
		t.Errorf("the segment it went on in was sent %q", sent)
	}
	recorded := false
	for _, e := range last.Persistent.Roster {
		recorded = recorded || e.ID == KnowledgeEntryID("handbook") && !e.On && e.State == StateNotApplied
	}
	if !recorded {
		t.Errorf("the envelope's roster does not name the pack as not applied: %+v", last.Persistent.Roster)
	}
	st1 := sv.Status()
	if na := st1.NotApplied; na == nil || !strings.Contains(na.Reason, "the engine could not start it") || len(na.Entries) != 1 || na.Entries[0] != KnowledgeEntryID("handbook") {
		t.Fatalf("what was not applied: %+v", na)
	}
	if st1.Pending {
		t.Error("pending after the change was taken back: the next turn would try it again")
	}
	c, _ := LoadConfig(home, "assistant")
	if e, _ := BuildRoster(assistantAgent(t), c, nil).Entry(KnowledgeEntryID("handbook")); e.On || c.DisplayName != "Robin" {
		t.Errorf("after taking it back: the pack on %v, the name %q", e.On, c.DisplayName)
	}
	if !told(events, "A change could not be applied") {
		t.Error("the chat was not told the change was taken back")
	}
	if err := sv.Send(ctx, "third"); err != nil || len(st.segments) != 2 {
		t.Fatalf("a turn after: segments %d, %v", len(st.segments), err)
	}
	if _, err := sv.Change(Change{Name: new("Wren")}); err != nil {
		t.Fatal(err)
	}
	if sv.Status().NotApplied == nil {
		t.Error("a name cleared the record")
	}
	refuseWith(st, nil)
	if _, err := sv.Change(Change{Switch: &Switch{ID: KnowledgeEntryID("handbook"), On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if sv.Status().NotApplied != nil {
		t.Error("switching the pack on again did not clear the record")
	}
}

// TestAChangeThatTakesSomethingAwayIsNeverTakenBack: a change that switched
// something off, whose segment cannot start, is not undone, since that
// would hand the agent what the person withdrew: the agent stops, saying
// why, and the change is kept.
func TestAChangeThatTakesSomethingAwayIsNeverTakenBack(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryShare, On: false}}); err != nil {
		t.Fatal(err)
	}
	refuseWith(st, func(seg localrun.Segment) error {
		if seg.Grants.Share == "" {
			return errors.New("the engine could not start it")
		}
		return nil
	})
	if err := sv.Send(ctx, "second"); err == nil || !strings.Contains(err.Error(), "never put back without you") {
		t.Fatalf("a narrowing that could not start: %v", err)
	}
	select {
	case <-sv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept running")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, seg := range st.segments[1:] {
		if seg.Grants.Share != "" {
			t.Fatal("a segment was started with the share the person switched off")
		}
	}
	c, _ := LoadConfig(home, "assistant")
	if e, _ := BuildRoster(assistantAgent(t), c, nil).Entry(EntryShare); e.On || c.NotApplied != nil {
		t.Errorf("the change was not kept: share on %v, not applied %+v", e.On, c.NotApplied)
	}
}

// TestAnAgentStopsWhenItCannotStartWithoutTheChangeEither: when the segment
// without what the change added cannot start either, the agent stops
// saying both reasons, and the person's change is kept for the next start.
func TestAnAgentStopsWhenItCannotStartWithoutTheChangeEither(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	store := &fakeStore{packs: map[string]string{"handbook": "sha256:one"}}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, nil, store.open(), store.contents())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(Change{AddKnowledge: "handbook", Consent: true}); err != nil {
		t.Fatal(err)
	}
	refuseWith(st, func(localrun.Segment) error { return errors.New("no engine") })
	err := sv.Send(ctx, "second")
	if err == nil || !strings.Contains(err.Error(), "could not start without the change either") {
		t.Fatalf("both starts failing: %v", err)
	}
	select {
	case <-sv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept running with no segment")
	}
	c, _ := LoadConfig(home, "assistant")
	if e, _ := BuildRoster(assistantAgent(t), c, nil).Entry(KnowledgeEntryID("handbook")); !e.On || c.NotApplied != nil {
		t.Errorf("the change was not kept for the next start: the pack on %v, not applied %+v", e.On, c.NotApplied)
	}
}

// TestAChangeWhoseKeyIsNotInTheKeychainIsRefused: a stored secret, a git
// host account or a calendar feed whose value is not in the keychain is
// refused when the change is made, and the config is left as it was.
func TestAChangeWhoseKeyIsNotInTheKeychainIsRefused(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, fakeSecrets{}, nil, Contents{})
	before, _ := LoadConfig(home, "assistant")
	for name, ch := range map[string]Change{
		"a secret":           {SetSecret: &StoredSecret{Name: "BRAVE_API_KEY", Set: time.Now()}, Consent: true},
		"a git host account": {SetAccount: &GitAccount{Host: "github.com", User: "person"}, Consent: true},
		"a calendar feed":    {SetCalendarFeed: &CalendarFeed{Name: "work", Host: "calendar.example.com", Set: time.Now()}, Consent: true},
	} {
		if _, err := sv.Change(ch); err == nil || !strings.Contains(err.Error(), "not in the keychain") {
			t.Errorf("%s with nothing in the keychain: %v", name, err)
		}
	}
	after, _ := LoadConfig(home, "assistant")
	if len(after.Secrets) != len(before.Secrets) || len(after.Accounts) != len(before.Accounts) || len(after.CalendarFeeds) != len(before.CalendarFeeds) {
		t.Error("a refused change reached the config")
	}
}
