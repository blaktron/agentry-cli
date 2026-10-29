package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// codexSubscriptionLimit is the plan's usage limit as codex 0.162.0 met it
// on a ChatGPT plan: a stream that ended early, retried five times, then
// given up on with the variant "other" and only the message to say why.
const codexSubscriptionLimit = "stream disconnected before completion: The ChatGPT user has reached their Subscription Sharing usage limit. Ask the user to try again after their usage limit resets or use an API key instead."

// codexHarness plays an app-server for one test: answer is called with each
// request after the handshake and thread/start.
func codexHarness(t *testing.T, signIn bool, answer func(stdio *fakeStdio, id int, method string, params map[string]any)) SessionDriver {
	t.Helper()
	ad, _ := Get(CodexImageAdapterName)
	stdio := newFakeStdio()
	drv, err := ad.Session(context.Background(), stdio, SessionOptions{ModelSignIn: signIn})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for line := range stdio.lines {
			var m struct {
				ID     *int           `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(line, &m) != nil || m.ID == nil {
				continue
			}
			switch m.Method {
			case "initialize":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"userAgent":"codex"}}`)
			case "thread/start":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"thread":{"id":"t1"}}}`)
			default:
				answer(stdio, *m.ID, m.Method, m.Params)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return drv
}

// limitedTurn answers every turn/start as codex did when the plan's usage
// limit was reached.
func limitedTurn(stdio *fakeStdio, id int, method string, _ map[string]any) {
	if method != "turn/start" {
		return
	}
	turn := "u" + itoa(id)
	stdio.say(`{"id":` + itoa(id) + `,"result":{"turn":{"id":"` + turn + `","status":"inProgress"}}}`)
	detail, _ := json.Marshal(codexSubscriptionLimit)
	for i := 1; i <= 5; i++ {
		stdio.say(`{"method":"error","params":{"error":{"message":"Reconnecting... ` + itoa(i) + `/5","codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":null}},"additionalDetails":` + string(detail) + `},"willRetry":true,"threadId":"t1","turnId":"` + turn + `"}}`)
	}
	final := `{"message":` + string(detail) + `,"codexErrorInfo":"other","additionalDetails":null}`
	stdio.say(`{"method":"error","params":{"error":` + final + `,"willRetry":false,"threadId":"t1","turnId":"` + turn + `"}}`)
	stdio.say(`{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"` + turn + `","status":"failed","error":` + final + `}}}`)
}

func drainEvents(drv SessionDriver) []Event {
	done := make(chan []Event, 1)
	go func() {
		var evs []Event
		for ev := range drv.Events() {
			evs = append(evs, ev)
		}
		done <- evs
	}()
	_ = drv.Close()
	return <-done
}

// The plan's usage limit that reaches codex as a stream that ended early is
// the plan's limit on a sign-in, said once at each turn that meets it, in
// codex's words without the stream's; the retries codex makes on its own are
// not errors of the turn.
func TestCodexsSubscriptionLimitIsThePlansLimitAtEachTurn(t *testing.T) {
	drv := codexHarness(t, true, limitedTurn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := drv.Prompt(ctx, "go"); err == nil {
			t.Fatalf("turn %d: a refused turn returned no error", i+1)
		}
	}
	evs := drainEvents(drv)
	limits := eventsOf(evs, EventPlanLimit)
	if len(limits) != 2 {
		t.Fatalf("plan-limit events %+v, want one for each turn", limits)
	}
	for _, l := range limits {
		if !strings.HasPrefix(l.Text, "The ChatGPT user has reached their Subscription Sharing usage limit.") {
			t.Errorf("plan-limit text %q", l.Text)
		}
	}
	if errs := eventsOf(evs, EventError); len(errs) != 0 {
		t.Errorf("errors %+v: the retries are not the turn's errors, and the limit is said as the limit", errs)
	}
}

// On a key, the same answer is one error, not six.
func TestCodexsRetriesAreNotErrorsOnAKey(t *testing.T) {
	drv := codexHarness(t, false, limitedTurn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := drv.Prompt(ctx, "go"); err == nil {
		t.Fatal("a refused turn returned no error")
	}
	evs := drainEvents(drv)
	if errs := eventsOf(evs, EventError); len(errs) != 1 || errs[0].Text != codexSubscriptionLimit {
		t.Fatalf("errors %+v", errs)
	}
	if len(eventsOf(evs, EventPlanLimit)) != 0 {
		t.Fatal("a key run has no plan to limit it")
	}
}

// Interrupt is turn/interrupt of the turn in flight, and the turn ends with
// what the agent had said, as codex ends it.
func TestCodexInterruptStopsTheTurnInFlight(t *testing.T) {
	asked := make(chan map[string]any, 1)
	drv := codexHarness(t, false, func(stdio *fakeStdio, id int, method string, params map[string]any) {
		switch method {
		case "turn/start":
			stdio.say(`{"id":` + itoa(id) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`)
			stdio.say(`{"method":"item/completed","params":{"threadId":"t1","turnId":"u1","item":{"type":"agentMessage","id":"m1","text":"Half an answer"}}}`)
		case "turn/interrupt":
			asked <- params
			stdio.say(`{"id":` + itoa(id) + `,"result":{}}`)
			stdio.say(`{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"u1","status":"interrupted","error":null}}}`)
		}
	})
	it, ok := drv.(Interrupter)
	if !ok {
		t.Fatal("the codex driver cannot interrupt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type answer struct {
		turn *Turn
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		tr, err := drv.Prompt(ctx, "go")
		got <- answer{tr, err}
	}()
	// The turn is in flight, with its id, once its first message is in.
	evs := make(chan []Event, 1)
	said := make(chan struct{})
	go func() {
		var all []Event
		for ev := range drv.Events() {
			if ev.Kind == EventAssistantText && len(eventsOf(all, EventAssistantText)) == 0 {
				close(said)
			}
			all = append(all, ev)
		}
		evs <- all
	}()
	select {
	case <-said:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	if err := it.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if p := <-asked; p["threadId"] != "t1" || p["turnId"] != "u1" {
		t.Fatalf("turn/interrupt params %v", p)
	}
	a := <-got
	if a.err != nil || a.turn == nil || a.turn.StopReason != "interrupted" || a.turn.Text != "Half an answer" {
		t.Fatalf("Prompt after an interrupt: %+v, %v", a.turn, a.err)
	}
	_ = drv.Close()
	<-evs
}

// With no turn in flight there is nothing to stop.
func TestCodexInterruptWithNoTurnDoesNothing(t *testing.T) {
	drv := codexHarness(t, false, func(*fakeStdio, int, string, map[string]any) {
		t.Error("an interrupt with no turn reached the harness")
	})
	if err := drv.(Interrupter).Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt with no turn: %v", err)
	}
	_ = drainEvents(drv)
}
