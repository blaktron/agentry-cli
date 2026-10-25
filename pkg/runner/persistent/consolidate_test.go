package persistent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// memoryStub is a memory's fact count and digest as a test sets them.
type memoryStub struct {
	mu     sync.Mutex
	facts  int
	digest string
}

func (m *memoryStub) set(facts int, digest string) {
	m.mu.Lock()
	m.facts, m.digest = facts, digest
	m.mu.Unlock()
}

func (m *memoryStub) count(context.Context) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.facts
}

func (m *memoryStub) sum(context.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.digest
}

// startConsolidating starts the Assistant on the test's clock and memory,
// with a fast heartbeat.
func startConsolidating(t *testing.T, home string, st *starter, c *clock, m *memoryStub) *Supervisor {
	t.Helper()
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start, Now: c.now, Tick: 5 * time.Millisecond,
		CountFacts: m.count, MemoryDigest: m.sum,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

// consolidations counts the consolidation turns sent to the starter's
// segments.
func consolidations(st *starter) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, seg := range st.all {
		for _, raw := range seg.rawTurns() {
			if strings.HasSuffix(raw, consolidateText) {
				n++
			}
		}
	}
	return n
}

// waitUntil polls cond for up to two seconds.
func waitUntil(cond func() bool) bool {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// TestTheMemoryIsConsolidatedOnceADayAtIdle: two hours after the person's
// last turn the memory is consolidated in an upkeep turn of the conversation
// as it is, with no swap and a record; not again within the day, and not a
// day later when nothing was written since.
func TestTheMemoryIsConsolidatedOnceADayAtIdle(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	m := &memoryStub{facts: 40, digest: "sha256:a"}
	st := &starter{resumed: true}
	sv := startConsolidating(t, home, st, c, m)
	if err := sv.Send(context.Background(), "the car budget is 72,000 CAD"); err != nil {
		t.Fatal(err)
	}
	c.set(c.now().Add(90 * time.Minute))
	time.Sleep(50 * time.Millisecond)
	if n := consolidations(st); n != 0 {
		t.Fatalf("%d consolidations before two hours idle", n)
	}
	m.set(38, "sha256:b")
	c.set(c.now().Add(31 * time.Minute))
	if !waitUntil(func() bool { return consolidations(st) == 1 }) {
		t.Fatal("no consolidation at two hours idle")
	}
	raw := st.lastSegment().rawTurns()
	if last := raw[len(raw)-1]; !strings.Contains(last, "memory-consolidate skill") || !strings.Contains(last, "not a message from the person") {
		t.Errorf("the consolidation turn = %q", last)
	}
	if len(st.all) != 1 {
		t.Errorf("a consolidation swapped the segment: %d segments", len(st.all))
	}
	if !waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 1 }) {
		t.Fatal("no record of the consolidation")
	}
	recs := upkeepLines(t, st.segments[0].OutDir)
	if r := recs[0]; r.Trigger != UpkeepConsolidate || r.Swapped || r.Reason != "" || r.FactsBefore != 38 || r.FactsAfter != 38 {
		t.Errorf("the record = %+v", r)
	}
	if u := loadUpkeep(home, "assistant"); u.LastConsolidation.IsZero() || u.MemoryWrites != 0 || u.MemoryDigest != "sha256:b" || len(u.Upkeeps) != 1 {
		t.Errorf("upkeep.json = %+v", u)
	}

	// The person comes back, and goes again: the day is not over.
	if err := sv.Send(context.Background(), "and the colour?"); err != nil {
		t.Fatal(err)
	}
	c.set(c.now().Add(3 * time.Hour))
	time.Sleep(50 * time.Millisecond)
	if n := consolidations(st); n != 1 {
		t.Fatalf("%d consolidations within a day", n)
	}
	// A day later, with the memory as the last run left it: nothing to do.
	c.set(c.now().Add(24 * time.Hour))
	if !waitUntil(func() bool { return loadUpkeep(home, "assistant").MemoryWrites == 0 }) {
		t.Fatal("an unchanged memory was not looked at")
	}
	if n := consolidations(st); n != 1 {
		t.Errorf("an unchanged memory was consolidated: %d", n)
	}
	waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 2 })
	if recs := upkeepLines(t, st.segments[0].OutDir); len(recs) != 2 || !strings.Contains(recs[1].Reason, "has not changed") || recs[1].Turn != 0 {
		t.Errorf("the skipped run's record = %+v", recs)
	}
}

// TestAMessageDuringAConsolidationWaitsForIt: the person's message sent
// while the consolidation's turn runs waits for it, told why, and is
// answered after it.
func TestAMessageDuringAConsolidationWaitsForIt(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	m := &memoryStub{facts: 10, digest: "sha256:e"}
	st := &starter{resumed: true}
	sv := startConsolidating(t, home, st, c, m)
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := sv.Subscribe()
	defer unsubscribe()
	hold := make(chan struct{})
	seg := st.lastSegment()
	seg.mu.Lock()
	seg.hold = hold
	seg.mu.Unlock()
	c.set(c.now().Add(2*time.Hour + time.Minute))
	if !waitUntil(func() bool { return consolidations(st) == 1 }) {
		t.Fatal("no consolidation")
	}
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(context.Background(), "I'm back") }()
	told := false
	for deadline := time.After(2 * time.Second); !told; {
		select {
		case ev := <-events:
			told = ev.Text == "Tidying the memory first; your message follows"
		case <-deadline:
			t.Fatal("the waiting message was not told why")
		}
	}
	select {
	case err := <-sent:
		t.Fatalf("the message did not wait: %v", err)
	default:
	}
	close(hold)
	if err := <-sent; err != nil {
		t.Fatalf("the message after the consolidation: %v", err)
	}
	raw := seg.rawTurns()
	if last := raw[len(raw)-1]; !strings.HasSuffix(last, "I'm back") || !strings.HasSuffix(raw[len(raw)-2], consolidateText) {
		t.Errorf("the turns = %q", raw[len(raw)-2:])
	}
}

// TestAConsolidationFollowsAnIdleCompaction: when a two-hour idle finds a
// large conversation and a consolidation due, the compaction's upkeep turn
// comes first, and the consolidation runs in the new conversation.
func TestAConsolidationFollowsAnIdleCompaction(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	if err := saveUpkeep(home, "assistant", upkeepState{LastPersonTurn: c.now().Add(-3 * time.Hour), ContextTokens: 150_000,
		ContextLimit: 1_000_000, TurnsSince: 4, MemoryWrites: 4}); err != nil {
		t.Fatal(err)
	}
	m := &memoryStub{facts: 20, digest: "sha256:c"}
	st := &starter{answer: aHandoff, resumed: true, renew: true}
	startConsolidating(t, home, st, c, m)
	if !waitUntil(func() bool { return consolidations(st) == 1 }) {
		t.Fatal("no consolidation after the idle compaction")
	}
	raw := st.lastSegment().rawTurns()
	compact, consolidate := -1, -1
	for i, r := range raw {
		switch {
		case strings.HasSuffix(r, upkeepText):
			compact = i
		case strings.HasSuffix(r, consolidateText):
			consolidate = i
		}
	}
	if compact < 0 || consolidate != compact+1 || !strings.Contains(raw[consolidate], "Agentry compacted your conversation") {
		t.Fatalf("the turns = %q", raw)
	}
	waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 2 })
	recs := upkeepLines(t, st.segments[0].OutDir)
	if len(recs) != 2 || recs[0].Trigger != UpkeepIdle || !recs[0].Swapped || recs[1].Trigger != UpkeepConsolidate || recs[1].Swapped {
		t.Errorf("the records = %+v", recs)
	}
}

// TestAConsolidationKeepsTheDailyCap: with the day's upkeep turns spent, no
// consolidation is sent, the record says why, and it waits before trying
// again.
func TestAConsolidationKeepsTheDailyCap(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	var spent []time.Time
	for i := range maxUpkeepsPerDay {
		spent = append(spent, c.now().Add(-time.Duration(i+1)*time.Hour))
	}
	if err := saveUpkeep(home, "assistant", upkeepState{LastPersonTurn: c.now().Add(-3 * time.Hour), MemoryWrites: 1, Upkeeps: spent}); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	startConsolidating(t, home, st, c, &memoryStub{facts: 5, digest: "sha256:d"})
	if !waitUntil(func() bool { return len(upkeepLines(t, st.segments[0].OutDir)) == 1 }) {
		t.Fatal("no record of the held-back consolidation")
	}
	if r := upkeepLines(t, st.segments[0].OutDir)[0]; r.Trigger != UpkeepConsolidate || !strings.Contains(r.Reason, "kept for compactions") {
		t.Errorf("the record = %+v", r)
	}
	if n := consolidations(st); n != 0 {
		t.Errorf("%d consolidations past the cap", n)
	}
	if u := loadUpkeep(home, "assistant"); !u.ConsolidateAfter.After(c.now()) {
		t.Errorf("the consolidation does not wait: %+v", u.ConsolidateAfter)
	}
}
