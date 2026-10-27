package persistent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// aCoderHandoff answers an upkeep turn with a handoff that names the files
// and the last command, and any other turn with an echo.
func aCoderHandoff(text string) string {
	if strings.HasPrefix(text, "Upkeep:") {
		return "owed: the failing test fixed\nctx: files internal/cart/cart.go and cart_test.go; go test ./internal/cart failed on TestTotal\ntried: rounding in Total\nopen: the discount rule"
	}
	return "echo: " + text
}

// enableAgent enables an agent that keeps no memory with its roster agreed.
func enableAgent(t *testing.T, home string, a Agent) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c := Config{Enabled: true}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
}

// startNoMemory starts an agent that keeps no memory. Its memory is never
// counted: a count would mean a one-shot over a volume it does not have.
func startNoMemory(t *testing.T, home string, a Agent, st *starter, now func() time.Time) *Supervisor {
	t.Helper()
	opts := Options{
		Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start,
		CountFacts:   func(context.Context) int { t.Error("the memory of an agent that keeps none was counted"); return 0 },
		MemoryDigest: func(context.Context) string { t.Error("the memory of an agent that keeps none was read"); return "" },
	}
	if a.DesktopOnly {
		// An agent that runs only in the desktop is started by it.
		opts.Client = ClientDesktop
	}
	if now != nil {
		opts.Now, opts.Tick = now, 10*time.Millisecond
	}
	sv, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

// TestTheCodingAgentCompactsOnRequestWithAHandoffAlone: an agent that keeps
// no memory is compacted when asked. Its upkeep turn asks for the handoff
// alone, never a memory skill, and says what a handoff of work on files
// holds; the new conversation opens with the handoff quoted, without
// claiming anything was saved to a memory.
func TestTheCodingAgentCompactsOnRequestWithAHandoffAlone(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	enableAgent(t, home, a)
	st := &starter{answer: aCoderHandoff}
	sv := startNoMemory(t, home, a, st, nil)
	if err := sv.Send(context.Background(), "fix the cart test"); err != nil {
		t.Fatal(err)
	}
	first := st.last
	out, err := sv.Compact(context.Background())
	if err != nil || !out.Swapped || out.Trigger != UpkeepManual || out.HandoffForm != HandoffKeyed {
		t.Fatalf("compact = %+v, %v", out, err)
	}
	if out.FactsBefore != -1 || out.FactsAfter != -1 {
		t.Errorf("facts = %d → %d, want -1, not counted", out.FactsBefore, out.FactsAfter)
	}
	raw := first.rawTurns()
	if len(raw) != 2 || !strings.HasSuffix(raw[1], upkeepTextNoMemory) {
		t.Fatalf("the upkeep turn = %q", raw)
	}
	if strings.Contains(raw[1], "memory") {
		t.Errorf("the upkeep turn of an agent with no memory names one: %q", raw[1])
	}
	for _, want := range []string{"handoff", "files", "command"} {
		if !strings.Contains(raw[1], want) {
			t.Errorf("the upkeep turn does not say %q: %q", want, raw[1])
		}
	}
	if err := sv.Send(context.Background(), "where were we?"); err != nil {
		t.Fatal(err)
	}
	sent := st.last.sentTurns()
	if len(sent) != 1 || !strings.Contains(sent[0], "Agentry compacted your conversation") ||
		!strings.Contains(sent[0], strconv.Quote(aCoderHandoff("Upkeep:"))) || !strings.HasSuffix(sent[0], "where were we?") {
		t.Fatalf("the new conversation's first turn = %q", sent)
	}
	if strings.Contains(sent[0], "memory") {
		t.Errorf("the new conversation's first turn names a memory: %q", sent[0])
	}
	recs := upkeepLines(t, st.segments[len(st.segments)-1].OutDir)
	if len(recs) != 1 || !recs[0].Swapped || recs[0].Trigger != UpkeepManual {
		t.Fatalf("the receipt = %+v", recs)
	}
	var upkeepTurns int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && upkeepTurns == 0; time.Sleep(10 * time.Millisecond) {
		entries, _ := Transcript(home, a.Name, 0)
		for _, e := range entries {
			if e.Kind == session.KindUserTurn && e.Upkeep {
				upkeepTurns++
			}
		}
	}
	if upkeepTurns != 1 {
		t.Errorf("transcript: %d upkeep turns, want 1", upkeepTurns)
	}
}

// TestMainCompactsOnRequest: Main, which keeps no memory either, is
// compacted when asked.
func TestMainCompactsOnRequest(t *testing.T) {
	home := shortHome(t)
	a, err := Lookup("main")
	if err != nil {
		t.Fatal(err)
	}
	enableAgent(t, home, a)
	st := &starter{answer: aHandoff}
	sv := startNoMemory(t, home, a, st, nil)
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	out, err := sv.Compact(context.Background())
	if err != nil || !out.Swapped {
		t.Fatalf("compact = %+v, %v", out, err)
	}
}

// TestTheCodingAgentCompactsInUse: a turn that filled three quarters of the
// model's window is followed by a compaction before the person's next
// message, as for an agent that keeps a memory.
func TestTheCodingAgentCompactsInUse(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	enableAgent(t, home, a)
	st := &starter{answer: aCoderHandoff, context: 800_000, limit: 1_000_000}
	sv := startNoMemory(t, home, a, st, nil)
	if err := sv.Send(context.Background(), "a long refactor"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); !sv.inUseDue() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	st.mu.Lock()
	st.context = 1_000
	st.mu.Unlock()
	if err := sv.Send(context.Background(), "and the next file"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 {
		t.Fatalf("segments = %d, want the compaction's second", len(st.segments))
	}
	if sent := st.last.sentTurns(); len(sent) != 1 || !strings.Contains(sent[0], "compacted your conversation") {
		t.Fatalf("the next message = %q", sent)
	}
}

// TestTheCodingAgentCompactsWhenIdleAndNeverConsolidates: two hours after the
// person's last message, a large conversation is compacted by the heartbeat's
// ticker; an agent with no memory has nothing to consolidate, so no other
// upkeep turn follows.
func TestTheCodingAgentCompactsWhenIdleAndNeverConsolidates(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	enableAgent(t, home, a)
	then := time.Now().Add(-3 * time.Hour)
	if err := saveUpkeep(home, a.Name, upkeepState{LastPersonTurn: then, ContextTokens: 150_000, ContextLimit: 1_000_000, TurnsSince: 4, MemoryWrites: 3}); err != nil {
		t.Fatal(err)
	}
	st := &starter{answer: aCoderHandoff, resumed: true}
	startNoMemory(t, home, a, st, time.Now)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		st.mu.Lock()
		n := len(st.segments)
		st.mu.Unlock()
		if n >= 2 {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.segments) != 2 {
		t.Fatalf("segments = %d, want the idle compaction's second", len(st.segments))
	}
	for _, seg := range st.all {
		for _, r := range seg.rawTurns() {
			if strings.HasSuffix(r, consolidateText) {
				t.Fatalf("an agent with no memory was sent a consolidation: %q", r)
			}
		}
	}
}

// TestTheMemoryFramesStayTheAssistants: the handoff an agent that keeps a
// memory carries says what it produced is in the memory; one that keeps none
// is told only that this is its handoff.
func TestTheMemoryFramesStayTheAssistants(t *testing.T) {
	h := "open: the car"
	if got := handoffCarried(h, true); !strings.Contains(got, "in your memory") || !strings.Contains(got, strconv.Quote(h)) {
		t.Errorf("with a memory: %q", got)
	}
	if got := handoffCarried(h, false); strings.Contains(got, "memory") || !strings.Contains(got, strconv.Quote(h)) {
		t.Errorf("with none: %q", got)
	}
}

// TestTheCodingAgentsCompactionsAreBounded: an agent that keeps no memory is
// held to the same bounds: a busy turn refuses a compaction, the day's cap
// refuses one more, and a failed automatic compaction waits before it is
// tried again.
func TestTheCodingAgentsCompactionsAreBounded(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		home := shortHome(t)
		a := coderAgent(t)
		enableAgent(t, home, a)
		sv := startNoMemory(t, home, a, &starter{answer: aCoderHandoff}, nil)
		sv.turn.Lock()
		_, err := sv.Compact(context.Background())
		sv.turn.Unlock()
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("compact during a turn: %v, want ErrBusy", err)
		}
	})
	t.Run("the day's cap", func(t *testing.T) {
		home := shortHome(t)
		a := coderAgent(t)
		enableAgent(t, home, a)
		var today []time.Time
		for i := range maxUpkeepsPerDay {
			today = append(today, time.Now().Add(-time.Duration(i+1)*time.Minute).UTC())
		}
		if err := saveUpkeep(home, a.Name, upkeepState{Upkeeps: today}); err != nil {
			t.Fatal(err)
		}
		st := &starter{answer: aCoderHandoff}
		sv := startNoMemory(t, home, a, st, nil)
		out, err := sv.Compact(context.Background())
		if err != nil || out.Swapped || !strings.Contains(out.Reason, "upkeep turns in the last day") || len(st.segments) != 1 {
			t.Fatalf("compact past the cap = %+v, %v (segments %d)", out, err, len(st.segments))
		}
	})
	t.Run("a failed automatic one waits", func(t *testing.T) {
		home := shortHome(t)
		a := coderAgent(t)
		enableAgent(t, home, a)
		st := &starter{answer: func(string) string { return "" }, context: 800_000, limit: 1_000_000}
		sv := startNoMemory(t, home, a, st, nil)
		if err := sv.Send(context.Background(), "a long refactor"); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(2 * time.Second); !sv.inUseDue() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		for _, msg := range []string{"one", "two"} {
			if err := sv.Send(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
		}
		upkeeps := 0
		for _, r := range st.last.rawTurns() {
			if strings.HasSuffix(r, upkeepTextNoMemory) {
				upkeeps++
			}
		}
		sv.mu.Lock()
		retry := sv.upkeep.RetryAfter
		sv.mu.Unlock()
		if upkeeps != 1 || retry.Before(time.Now().Add(50*time.Minute)) || len(st.segments) != 1 {
			t.Fatalf("upkeep turns %d, retry after %v, segments %d: want one that failed, a wait of an hour, nothing swapped", upkeeps, retry, len(st.segments))
		}
	})
}
