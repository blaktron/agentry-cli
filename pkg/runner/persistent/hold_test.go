package persistent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// heldGoogle is a running Assistant whose Google Workspace is on but not
// signed in, as a set-up finds it, with the signoff a sign-in must keep.
func heldGoogle(t *testing.T) (string, *starter, *Supervisor) {
	t.Helper()
	withImageServers(t)
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	c, _ := LoadConfig(home, a.Name)
	c.MailProviders = googleOn
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "server:gog", On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword}, nil, Contents{})
	return home, st, sv
}

// segmentsOf counts the segments st started.
func segmentsOf(st *starter) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.segments)
}

// noticed waits for a notice on events that begins with prefix.
func noticed(t *testing.T, events <-chan session.Event, prefix string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the chat closed before a notice %q", prefix)
			}
			if ev.Kind == session.KindNotice && strings.HasPrefix(ev.Text, prefix) {
				return
			}
		case <-deadline:
			t.Fatalf("no notice %q", prefix)
		}
	}
}

var googleSignIn = SignInRecord{Key: "gog", SignIn: ImageSignIn{Account: "person@example.com", At: time.Now()}}

// TestAHeldAgentTakesItsSetUpAtTheNextSegment: a hold ends the running
// segment between turns, a message sent meanwhile waits, the sign-in the
// set-up records is a change the supervisor writes, and resuming starts
// the next segment with the server signed in, the message answered there.
func TestAHeldAgentTakesItsSetUpAtTheNextSegment(t *testing.T) {
	home, st, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	first := st.last
	h, err := HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if ended, _ := first.Ended(); !ended {
		t.Fatal("the hold left the segment running, holding the server's volume")
	}
	if sv.Status().Held != "Google Workspace (gog)" {
		t.Errorf("the status does not say what is being set up: %+v", sv.Status())
	}
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(ctx, "while held") }()
	noticed(t, events, "Setting up Google Workspace (gog): your message waits")
	select {
	case err := <-sent:
		t.Fatalf("a message during the hold did not wait: %v", err)
	default:
	}
	if _, err := h.Change(ctx, Change{SignIn: &googleSignIn}); err != nil {
		t.Fatalf("recording the sign-in through the hold: %v", err)
	}
	if err := h.Resume(ctx, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("the waiting message: %v", err)
	}
	st.mu.Lock()
	n, last := len(st.segments), st.segments[len(st.segments)-1]
	sentTurns := st.last.sentTurns()
	st.mu.Unlock()
	if n != 2 || len(last.Grants.ImageServers) != 1 || last.Grants.ImageServers[0].Account != "person@example.com" {
		t.Fatalf("segments %d; the next was given %+v", n, last.Grants.ImageServers)
	}
	if len(sentTurns) != 1 || !strings.HasSuffix(sentTurns[0], "while held") {
		t.Errorf("the next segment was sent %q", sentTurns)
	}
	detach()
	done := false
	for ev := range events {
		done = done || ev.Kind == session.KindNotice && strings.HasPrefix(ev.Text, "Google Workspace (gog) is set up")
		if ev.Kind == session.KindSessionEnd {
			t.Error("the chat saw the held segment end")
		}
	}
	if !done {
		t.Error("the chat was not told the set-up finished")
	}
}

// TestAHoldEndsWhenItsClientGoesAway: a client that closes its connection
// mid-hold, or never resumes before the bound, leaves the agent running on
// a new segment, and the chat is told the set-up did not finish.
func TestAHoldEndsWhenItsClientGoesAway(t *testing.T) {
	home, st, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	defer detach()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	h, err := HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	eventually(t, "a segment after a dropped hold", func() bool { return segmentsOf(st) == 2 })
	noticed(t, events, "Setting up Google Workspace (gog) did not finish")
	if err := sv.Send(ctx, "after a dropped hold"); err != nil {
		t.Fatalf("a turn after a dropped hold: %v", err)
	}

	old := holdBound
	holdBound = 200 * time.Millisecond
	t.Cleanup(func() { holdBound = old })
	h, err = HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	eventually(t, "a segment after the hold's bound", func() bool { return segmentsOf(st) == 3 })
}

// TestAStaleHoldCannotEndANewerOne: a connection whose hold its bound ended
// can neither resume nor record into the hold another client took since.
func TestAStaleHoldCannotEndANewerOne(t *testing.T) {
	home, st, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	old := holdBound
	holdBound = 200 * time.Millisecond
	stale, err := HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stale hold's bound", func() bool { return segmentsOf(st) == 2 })
	holdBound = old
	t.Cleanup(func() { holdBound = old })
	fresh, err := HoldAgent(ctx, home, "assistant", "Microsoft 365")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Change(ctx, Change{SignIn: &googleSignIn}); err == nil {
		t.Error("a stale hold recorded a sign-in")
	}
	if err := stale.Resume(ctx, true); err == nil {
		t.Error("a stale hold resumed")
	}
	if sv.Status().Held != "Microsoft 365" || segmentsOf(st) != 2 {
		t.Fatalf("the newer hold was ended: %+v, %d segments", sv.Status(), segmentsOf(st))
	}
	if err := fresh.Resume(ctx, true); err != nil {
		t.Fatalf("the newer hold's own resume: %v", err)
	}
}

// TestASignInIsRecordedByItsHoldAlone: a sign-in record from a connection
// that holds nothing, or one that carries more than its provider, is
// refused; a held record keeps the person's own signoff and client.
func TestASignInIsRecordedByItsHoldAlone(t *testing.T) {
	home, _, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Configure(ctx, home, assistantAgent(t), ClientCLI, Change{SignIn: &googleSignIn}); err == nil || !strings.Contains(err.Error(), "set-up that holds the agent") {
		t.Fatalf("a sign-in from a client that holds nothing: %v", err)
	}
	c, _ := LoadConfig(home, "assistant")
	editSignIn(&c, "gog", func(in *ImageSignIn) { in.Signoff, in.ClientID = "kept", "kept-client" })
	_ = SaveConfig(home, "assistant", c)
	h, err := HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Change(ctx, Change{SignIn: &googleSignIn, Switch: &Switch{ID: "server:gog:writes", On: true}, Consent: true}); err == nil {
		t.Error("a sign-in carrying a switch was taken")
	}
	bad := SignInRecord{Key: "gog", SignIn: ImageSignIn{Account: "person@example.com", Granted: []string{"no-such-switch"}}}
	if _, err := h.Change(ctx, Change{SignIn: &bad}); err == nil {
		t.Error("a sign-in granting a switch the server has not was taken")
	}
	forged := SignInRecord{Key: "gog", SignIn: ImageSignIn{Account: "-o=x@example.com"}}
	if _, err := h.Change(ctx, Change{SignIn: &forged}); err == nil {
		t.Error("an account that reads as a flag was taken")
	}
	if _, err := h.Change(ctx, Change{SignIn: &googleSignIn}); err != nil {
		t.Fatal(err)
	}
	if err := h.Resume(ctx, true); err != nil {
		t.Fatal(err)
	}
	c, _ = LoadConfig(home, "assistant")
	if in := c.SignIns["gog"]; in.Account != "person@example.com" || in.Signoff != "kept" || in.ClientID != "kept-client" {
		t.Errorf("the record after the sign-in: %+v", in)
	}
}

// TestASecondHoldIsRefusedAndAStopEndsAHold: while one client holds the
// agent another's hold is refused; stopping the agent ends the hold, and
// its client's changes are refused after.
func TestASecondHoldIsRefusedAndAStopEndsAHold(t *testing.T) {
	home, _, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	h, err := HoldAgent(ctx, home, "assistant", "Google Workspace (gog)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if _, err := HoldAgent(ctx, home, "assistant", "Microsoft 365"); err == nil || !strings.Contains(err.Error(), "held for a set-up already") {
		t.Fatalf("a second hold: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := sv.Stop(ctx); stopped <- err }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stop waited on the hold")
	}
	if _, err := h.Change(ctx, Change{SignIn: &googleSignIn}); err == nil {
		t.Error("a hold's change was taken after the agent stopped")
	}
}

// TestASetUpOnARunningAgentIsHeld: runServerSetUp on a running agent holds
// it, records through the hold, switches the provider on and resumes; on
// a stopped one it records under the lock.
func TestASetUpOnARunningAgentIsHeld(t *testing.T) {
	home, st, sv := heldGoogle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	a := assistantAgent(t)
	var during string
	err := runServerSetUp(ctx, home, a, ClientCLI, "Google Workspace (gog)", nil, func(_ Config, edit signInEdit) error {
		during = sv.Status().Held
		return edit("gog", func(in *ImageSignIn) { in.Account, in.At = "person@example.com", time.Now() })
	})
	if err != nil || during == "" {
		t.Fatalf("a set-up on a running agent: %v (held %q)", err, during)
	}
	if segmentsOf(st) != 2 {
		t.Fatalf("%d segments after the set-up", segmentsOf(st))
	}
	failed := errors.New("the browser sign-in timed out")
	if err := runServerSetUp(ctx, home, a, ClientCLI, "Google Workspace (gog)", nil, func(Config, signInEdit) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("a failed set-up: %v", err)
	}
	if sv.Status().Held != "" || segmentsOf(st) != 3 {
		t.Fatalf("after a failed set-up the agent is %+v on %d segments", sv.Status(), segmentsOf(st))
	}
	if _, err := sv.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	var told []string
	if err := runServerSetUp(ctx, home, a, ClientCLI, "Microsoft 365", func(p offer.MailProvider) { told = append(told, p.Key) }, func(_ Config, edit signInEdit) error {
		return edit("ms365", func(in *ImageSignIn) { in.AgentryApp = true })
	}); err != nil {
		t.Fatalf("a set-up on a stopped agent: %v", err)
	}
	if c, _ := LoadConfig(home, "assistant"); !c.SignIns["ms365"].AgentryApp || len(told) != 1 {
		t.Errorf("the stopped set-up recorded %+v, told %v", c.SignIns["ms365"], told)
	}
}
