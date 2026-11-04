package persistent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// startPublished starts acme/ada at version, agreed to, returning its
// supervisor.
func startAda(t *testing.T, home string, st *starter) (Agent, *Supervisor) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := installPublished(t, home, "acme", "ada", "1.0.0", publishedRelease{})
	c, err := LoadConfig(home, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	sv, err := Start(context.Background(), Options{Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: st.start})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return a, sv
}

// TestAnUpdateOfARunningAgentReachesTheNextTurn: a newer release installed
// while the agent runs is taken by a change that asks for it, the next turn
// starts a segment from it, the conversation continuing, and the older
// release's folder goes once that segment has started.
func TestAnUpdateOfARunningAgentReachesTheNextTurn(t *testing.T) {
	home := shortHome(t)
	st := &starter{resumed: true}
	old, sv := startAda(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	next := installPublished(t, home, "acme", "ada", "1.1.0", publishedRelease{})
	if !sv.Status().Pending {
		t.Fatal("a newer release installed is not pending")
	}
	if _, running, err := Configure(ctx, home, next, ClientCLI, Change{Release: true}); err != nil || !running {
		t.Fatalf("taking the release on a running agent: running %v, %v", running, err)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n, last := len(st.segments), st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if n != 2 || last.Release == nil || last.Release.Meta.Ref != "acme/ada@1.1.0" {
		t.Fatalf("segments %d; the next ran %+v", n, last.Release)
	}
	if last.Persistent.ContinueID == "" {
		t.Error("the update did not continue the conversation")
	}
	if _, err := os.Stat(old.Release.Dir); !os.IsNotExist(err) {
		t.Errorf("the older release's folder is still there: %v", err)
	}
	if sv.Status().Pending {
		t.Error("pending after the turn that took the update")
	}
}

// TestAnUpdateAsksForWhatIsNotAgreedTo: a change that takes a newly
// installed release is refused while what it has on is not agreed to, and
// records the agreement when it is given.
func TestAnUpdateAsksForWhatIsNotAgreedTo(t *testing.T) {
	home := shortHome(t)
	st := &starter{resumed: true}
	_, sv := startAda(t, home, st)
	next := installPublished(t, home, "acme", "ada", "1.1.0", publishedRelease{})
	c, _ := LoadConfig(home, next.Name)
	delete(c.Consented, EntryShare)
	if err := SaveConfig(home, next.Name, c); err != nil {
		t.Fatal(err)
	}
	var ce *ConsentError
	if _, err := sv.Change(Change{Release: true}); !errors.As(err, &ce) || len(ce.Entries) == 0 {
		t.Fatalf("an update with what is not agreed to: %v", err)
	}
	if _, err := sv.Change(Change{Release: true, Consent: true}); err != nil {
		t.Fatalf("the update agreed to: %v", err)
	}
	if c, _ := LoadConfig(home, next.Name); c.Consented[EntryShare] == "" {
		t.Error("the agreement was not recorded")
	}
}

// TestAnUpdateNoSegmentCanStartOnIsSetAside: a release no segment can start
// on is set aside, the agent going on with the release it had and saying
// why, its folder kept.
func TestAnUpdateNoSegmentCanStartOnIsSetAside(t *testing.T) {
	home := shortHome(t)
	st := &starter{resumed: true}
	old, sv := startAda(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	defer detach()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	installPublished(t, home, "acme", "ada", "1.1.0", publishedRelease{})
	if _, err := sv.Change(Change{Release: true}); err != nil {
		t.Fatal(err)
	}
	refuseWith(st, func(seg localrun.Segment) error {
		if seg.Release != nil && seg.Release.Meta.Version == "1.1.0" {
			return errors.New("the release does not start")
		}
		return nil
	})
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatalf("the turn after an update that does not start: %v", err)
	}
	st.mu.Lock()
	last := st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if last.Release.Meta.Version != "1.0.0" || sv.Status().Pending {
		t.Fatalf("the agent went on with %s, pending %v", last.Release.Meta.Version, sv.Status().Pending)
	}
	if _, err := os.Stat(old.Release.Dir); err != nil {
		t.Errorf("the release the agent went on with lost its folder: %v", err)
	}
	if !told(events, "The update to acme/ada@1.1.0 could not be started") {
		t.Error("the chat was not told the update was set aside")
	}
}

// TestAHandOverMovesTheAgentToANewSupervisor: a hand-over waits for the turn
// in flight, stops the agent, lets its lock go and starts it again.
func TestAHandOverMovesTheAgentToANewSupervisor(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	sv := startAssistant(t, home, &starter{resumed: true})
	started := false
	err := HandOver(context.Background(), home, assistantAgent(t), ClientCLI, func(context.Context) error {
		if held, _ := Held(home, "assistant"); held {
			t.Error("the new supervisor was started while the old one held the agent")
		}
		started = true
		return nil
	})
	if err != nil || !started {
		t.Fatalf("hand-over: started %v, %v", started, err)
	}
	select {
	case <-sv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the old supervisor kept running")
	}
}

// TestARefusalOfAnUnknownCommandIsAnOlderSupervisor: a supervisor's
// refusal of a line naming no command it knows is an older supervisor's;
// its other refusals, malformed ones included, are not.
func TestARefusalOfAnUnknownCommandIsAnOlderSupervisor(t *testing.T) {
	if !OlderSupervisor(&RefusedError{Class: "malformed", Text: unknownCommand + " attach, send, status"}) {
		t.Error("an unknown command's refusal is not an older supervisor's")
	}
	for _, err := range []error{&RefusedError{Class: "malformed", Text: "send needs a text"}, &RefusedError{Class: "busy", Text: "busy"}, errors.New("busy")} {
		if OlderSupervisor(err) {
			t.Errorf("%v is taken for an older supervisor", err)
		}
	}
}

// TestAHandOverLeavesAnotherClientsAgent: an agent another client runs is
// not handed over: the person is told to change it there.
func TestAHandOverLeavesAnotherClientsAgent(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	sv, err := Start(context.Background(), Options{Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: (&starter{}).start})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	err = HandOver(context.Background(), home, assistantAgent(t), ClientCLI, func(context.Context) error {
		t.Error("another client's agent was started again")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "Agentry Desktop") {
		t.Fatalf("a hand-over of the desktop's agent: %v", err)
	}
	select {
	case <-sv.Done():
		t.Fatal("the desktop's agent was stopped")
	default:
	}
}

// TestAnInstallRecordIsHeldToItsForm: a record whose version or digest is
// not of its form is refused, so neither can lead a release folder
// elsewhere.
func TestAnInstallRecordIsHeldToItsForm(t *testing.T) {
	home := shortHome(t)
	a := installPublished(t, home, "acme", "ada", "1.0.0", publishedRelease{})
	r, err := LoadRelease(home, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Release{
		"a version that climbs": {Ref: r.Ref, Version: "../../x", Digest: r.Digest, Source: r.Source},
		"a digest that climbs":  {Ref: r.Ref, Version: r.Version, Digest: "../../..", Source: r.Source},
	} {
		if _, err := SaveRelease(home, bad); err != nil {
			continue
		}
		if _, err := LoadRelease(home, a.Name); err == nil {
			t.Errorf("%s: the record was taken", name)
		}
	}
}
