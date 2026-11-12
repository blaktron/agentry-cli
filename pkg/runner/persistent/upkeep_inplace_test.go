package persistent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// saveTurns counts the save turns that follow a compaction in place, sent to
// every segment.
func saveTurns(st *starter) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, seg := range st.all {
		for _, r := range seg.rawTurns() {
			if strings.HasSuffix(r, saveText) {
				n++
			}
		}
	}
	return n
}

// TestEveryCompactionIsTheHarnesssSummaryWhereItCanCompact: on a harness that
// compacts in place, a compaction the person asks for, one a full turn
// brings and one an idle conversation brings are each the harness's own
// summary, with no upkeep turn over the large conversation and no new one;
// the Assistant, which keeps a memory, then saves the summary in a turn of
// the small conversation.
func TestEveryCompactionIsTheHarnesssSummaryWhereItCanCompact(t *testing.T) {
	inPlace := func(t *testing.T, st *starter, rec Compacted) {
		t.Helper()
		if st.compacting.compacted != 1 || upkeepTurns(st) != 0 || len(st.all) != 1 {
			t.Fatalf("compacted %d, upkeep turns %d, segments %d; want the harness's summary alone", st.compacting.compacted, upkeepTurns(st), len(st.all))
		}
		if saveTurns(st) != 1 {
			t.Fatalf("%d save turns, want 1", saveTurns(st))
		}
		if rec.Swap != SwapInPlace || rec.Swapped || rec.Reason != "" || rec.SaveFailed != "" || rec.FactsBefore < 0 || rec.FactsAfter < 0 {
			t.Errorf("record = %+v", rec)
		}
	}
	t.Run("manual", func(t *testing.T) {
		home := shortHome(t)
		enable(t, home)
		st := &starter{answer: aHandoff, compactor: true}
		sv := startUpkeeper(t, home, st, nil)
		if err := sv.Send(context.Background(), "the car budget is 72,000 CAD"); err != nil {
			t.Fatal(err)
		}
		out, err := sv.Compact(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inPlace(t, st, out)
		raw := st.last.rawTurns()
		save := raw[len(raw)-1]
		if !strings.Contains(save, "summary above is what is left of it") || !strings.Contains(save, "under to save") ||
			!strings.Contains(save, "not a message from the person") || strings.Contains(save, "answer with the handoff") {
			t.Errorf("the save turn = %q", save)
		}
		u := loadUpkeep(home, "assistant")
		if u.LastCompaction.IsZero() || len(u.Upkeeps) != 1 || u.MemoryWrites < 2 {
			t.Errorf("upkeep.json = %+v", u)
		}
	})
	t.Run("in use", func(t *testing.T) {
		home := shortHome(t)
		enable(t, home)
		st := &starter{answer: aHandoff, compactor: true, context: 800_000, limit: 1_000_000}
		sv := startUpkeeper(t, home, st, nil)
		if err := sv.Send(context.Background(), "a long question"); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(2 * time.Second); !sv.inUseDue() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		st.mu.Lock()
		st.last.context = 1_000
		st.mu.Unlock()
		if err := sv.Send(context.Background(), "the next one"); err != nil {
			t.Fatal(err)
		}
		c, _ := LastUpkeep(home, "assistant")
		if c == nil || c.Trigger != UpkeepInUse {
			t.Fatalf("record = %+v", c)
		}
		inPlace(t, st, Compacted{Swap: c.Swap, Swapped: c.Swapped, Reason: c.Reason, FactsBefore: c.FactsBefore, FactsAfter: c.FactsAfter})
		raw := st.last.rawTurns()
		if last := raw[len(raw)-1]; !strings.HasSuffix(last, "the next one") || strings.Contains(last, "compacted your conversation") {
			t.Errorf("the next message = %q", last)
		}
	})
	t.Run("idle", func(t *testing.T) {
		home := shortHome(t)
		enableHeartbeat(t, home, false)
		c := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
		if err := saveUpkeep(home, "assistant", upkeepState{LastPersonTurn: c.now().Add(-3 * time.Hour), ContextTokens: 150_000,
			ContextLimit: 1_000_000, TurnsSince: 4}); err != nil {
			t.Fatal(err)
		}
		st := &starter{answer: aHandoff, resumed: true, compactor: true}
		startConsolidating(t, home, st, c, &memoryStub{facts: 20, digest: "sha256:c"})
		if !waitUntil(func() bool { return saveTurns(st) == 1 }) {
			t.Fatal("no compaction at idle")
		}
		// The tidy, due too since the save wrote the memory, may follow.
		if !waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) >= 1 }) {
			t.Fatal("no record of the compaction")
		}
		r := upkeepLines(t, st.segments[0].OutDir)[0]
		if r.Trigger != UpkeepIdle {
			t.Fatalf("record = %+v", r)
		}
		inPlace(t, st, Compacted{Swap: r.Swap, Swapped: r.Swapped, Reason: r.Reason, FactsBefore: r.FactsBefore, FactsAfter: r.FactsAfter})
	})
}

// TestASummaryThatFailsIsNotSavedAndWaits: a harness whose summary fails
// leaves the conversation as it was, sends no save turn and no upkeep turn
// over the large conversation, and an automatic compaction then waits an
// hour.
func TestASummaryThatFailsIsNotSavedAndWaits(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{answer: aHandoff, compactor: true, compactErr: errors.New("compact: context deadline exceeded"), context: 800_000, limit: 1_000_000}
	sv := startUpkeeper(t, home, st, nil)
	if err := sv.Send(context.Background(), "a long question"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); !sv.inUseDue() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if err := sv.Send(context.Background(), "the next one"); err != nil {
		t.Fatal(err)
	}
	if saveTurns(st) != 0 || upkeepTurns(st) != 0 || len(st.all) != 1 {
		t.Fatalf("save turns %d, upkeep turns %d, segments %d; want none and the conversation as it was", saveTurns(st), upkeepTurns(st), len(st.all))
	}
	u := loadUpkeep(home, "assistant")
	if u.RetryAfter.Before(time.Now().Add(50*time.Minute)) || !u.LastCompaction.IsZero() {
		t.Errorf("upkeep.json = %+v", u)
	}
	if c, _ := LastUpkeep(home, "assistant"); c == nil || c.Trigger != UpkeepInUse || !strings.Contains(c.Reason, "context deadline exceeded") {
		t.Errorf("record = %+v", c)
	}
}

// TestASaveThatFailsLeavesTheCompactionStanding: the summary replaced the
// conversation, so a save turn that fails after it is recorded as such and
// the compaction stands; the memory is marked as maybe written, so its tidy
// still comes.
func TestASaveThatFailsLeavesTheCompactionStanding(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{answer: aHandoff, compactor: true, failSends: 1}
	sv := startUpkeeper(t, home, st, nil)
	out, err := sv.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Reason != "" || out.SaveFailed == "" || out.Swap != SwapInPlace || st.compacting.compacted != 1 {
		t.Fatalf("out = %+v, compacted %d", out, st.compacting.compacted)
	}
	if u := loadUpkeep(home, "assistant"); u.LastCompaction.IsZero() || u.MemoryWrites == 0 {
		t.Errorf("upkeep.json = %+v", u)
	}
	if c, _ := LastUpkeep(home, "assistant"); c == nil || c.SaveFailed == "" {
		t.Errorf("record = %+v", c)
	}
}

// TestATidyDueInALargeConversationCompactsItFirst: the memory's tidy never
// runs in a conversation of half the window or more. When it is due there,
// the conversation is compacted first and the tidy follows in the small one;
// when that compaction fails, the tidy waits and says why.
func TestATidyDueInALargeConversationCompactsItFirst(t *testing.T) {
	start := func(t *testing.T, st *starter) (string, *clock) {
		t.Helper()
		home := shortHome(t)
		enableHeartbeat(t, home, false)
		c := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
		// Below the idle compaction's floor, but over half this window.
		if err := saveUpkeep(home, "assistant", upkeepState{LastPersonTurn: c.now().Add(-3 * time.Hour), ContextTokens: 70_000,
			ContextLimit: 131_072, TurnsSince: 4, MemoryWrites: 3}); err != nil {
			t.Fatal(err)
		}
		st.answer, st.resumed, st.compactor = aHandoff, true, true
		startConsolidating(t, home, st, c, &memoryStub{facts: 20, digest: "sha256:d"})
		return home, c
	}
	t.Run("compacted first", func(t *testing.T) {
		st := &starter{}
		start(t, st)
		if !waitUntil(func() bool { return consolidations(st) == 1 }) {
			t.Fatal("no tidy after the compaction")
		}
		raw := st.lastSegment().rawTurns()
		save, tidy := -1, -1
		for i, r := range raw {
			switch {
			case strings.HasSuffix(r, saveText):
				save = i
			case strings.HasSuffix(r, consolidateText):
				tidy = i
			}
		}
		if st.compacting.compacted != 1 || save < 0 || tidy != save+1 {
			t.Fatalf("compacted %d, the turns = %q", st.compacting.compacted, raw)
		}
		waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 2 })
		recs := upkeepLines(t, st.segments[0].OutDir)
		if len(recs) != 2 || recs[0].Trigger != UpkeepIdle || recs[0].Swap != SwapInPlace || recs[1].Trigger != UpkeepConsolidate || recs[1].Reason != "" {
			t.Errorf("the records = %+v", recs)
		}
	})
	t.Run("the compaction fails", func(t *testing.T) {
		st := &starter{compactErr: errors.New("compact: context deadline exceeded")}
		home, c := start(t, st)
		if !waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 2 }) {
			t.Fatal("no records of the compaction and the tidy")
		}
		recs := upkeepLines(t, st.segments[0].OutDir)
		if recs[0].Trigger != UpkeepIdle || recs[0].Reason == "" || recs[1].Trigger != UpkeepConsolidate || !strings.Contains(recs[1].Reason, "too large to tidy") {
			t.Errorf("the records = %+v", recs)
		}
		if consolidations(st) != 0 || saveTurns(st) != 0 {
			t.Errorf("%d tidies and %d saves were sent in the large conversation", consolidations(st), saveTurns(st))
		}
		u := loadUpkeep(home, "assistant")
		if u.ConsolidateAfter.Before(c.now().Add(50*time.Minute)) || u.RetryAfter.Before(c.now().Add(50*time.Minute)) {
			t.Errorf("upkeep.json = %+v", u)
		}
		// Neither is tried again inside the hour.
		time.Sleep(50 * time.Millisecond)
		if st.compacting.compacted != 0 || len(upkeepLines(t, st.segments[0].OutDir)) != 2 {
			t.Errorf("tried again: %+v", upkeepLines(t, st.segments[0].OutDir))
		}
	})
}

// TestTooLargeToTidy: the rule alone.
func TestTooLargeToTidy(t *testing.T) {
	for _, tc := range []struct {
		tokens, limit int64
		want          bool
	}{
		{60_000, 131_072, false},
		{65_536, 131_072, true},
		{99_999, 1_000_000, false},
		{100_000, 1_000_000, true},
		{99_999, 0, false},
		{100_000, 0, true},
	} {
		if got := (upkeepState{ContextTokens: tc.tokens, ContextLimit: tc.limit}).tooLargeToTidy(); got != tc.want {
			t.Errorf("%d of %d: %v, want %v", tc.tokens, tc.limit, got, tc.want)
		}
	}
}

// TestUpkeepWaitsLongerOnAServerOfThePersonsOwn: an upkeep on a model server
// of the person's own may wait as long as the harness waits for a reply
// there; on a hosted model, five minutes.
func TestUpkeepWaitsLongerOnAServerOfThePersonsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model policy.Model
		want  time.Duration
	}{
		{"hosted", policy.Default().Model, upkeepTurnCap},
		{"custom", policy.Model{Provider: "custom", Name: "local", BaseURL: "http://localhost:8000/v1"}, upkeepTurnCapOwn},
		{"openai at a base URL", policy.Model{Provider: policy.ProviderOpenAI, Name: "local", BaseURL: "http://localhost:8000/v1"}, upkeepTurnCapOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := shortHome(t)
			enable(t, home)
			pol := policy.Default()
			pol.Model = tc.model
			st := &starter{answer: aHandoff}
			sv, err := Start(context.Background(), Options{
				Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: pol,
				Docker: &dockerx.Fake{}, StartSegment: st.start,
			})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
			if got := sv.upkeepCap(); got != tc.want {
				t.Errorf("cap = %v, want %v", got, tc.want)
			}
		})
	}
}
