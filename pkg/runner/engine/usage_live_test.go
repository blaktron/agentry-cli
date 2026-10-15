package engine

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// tokenAdapter turns "in=<n> out=<n>" into a usage event with that split and
// "total=<n>" into one that reports context tokens without a split, the two
// shapes a harness can take.
type tokenAdapter struct{ adapter.Adapter }

func (tokenAdapter) Events(line []byte) []adapter.Event {
	s := strings.TrimSpace(string(line))
	switch {
	case s == "tool":
		return []adapter.Event{{Kind: adapter.EventToolCall}}
	case strings.HasPrefix(s, "total="):
		n, _ := strconv.ParseInt(s[6:], 10, 64)
		return []adapter.Event{{Kind: adapter.EventUsage, TotalTokens: n}}
	case strings.HasPrefix(s, "in="):
		var in, out int64
		for _, f := range strings.Fields(s) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(v, 10, 64)
			if k == "in" {
				in = n
			} else {
				out = n
			}
		}
		return []adapter.Event{{Kind: adapter.EventUsage, InputTokens: in, OutputTokens: out}}
	}
	return nil
}

// The whole point of the callback: a driver learns the run's usage WHILE the
// run is going, and what it learns last is what the envelope ends up
// carrying. Without it usage would reach nobody until the envelope, so nothing
// could meter a live run.
func TestUsageIsReportedWhileTheRunIsLive(t *testing.T) {
	var seen []receipt.Usage
	c := &eventCollector{
		adapter: tokenAdapter{}, events: io.Discard,
		onUsage: func(u receipt.Usage) { seen = append(seen, u) },
	}
	c.consume(strings.NewReader("in=10 out=5\ntool\nin=20 out=7\ntool\nin=1 out=2\n"))

	if len(seen) != 3 {
		t.Fatalf("got %d usage reports, want one per usage event: %+v", len(seen), seen)
	}
	// Each report carries the RUNNING total, not the event's own delta —
	// a cap is a threshold on the run, not on one message.
	want := []receipt.Usage{
		{InputTokens: 10, OutputTokens: 5, Turns: 0},
		{InputTokens: 30, OutputTokens: 12, Turns: 1},
		{InputTokens: 31, OutputTokens: 14, Turns: 2},
	}
	for i, w := range want {
		got := seen[i]
		if got.InputTokens != w.InputTokens || got.OutputTokens != w.OutputTokens || got.Turns != w.Turns {
			t.Errorf("report %d = in %d out %d turns %d, want in %d out %d turns %d",
				i, got.InputTokens, got.OutputTokens, got.Turns, w.InputTokens, w.OutputTokens, w.Turns)
		}
	}
	// The last thing a driver saw is what the envelope says.
	if final := c.usage(); seen[len(seen)-1] != final {
		t.Errorf("last live report %+v is not the envelope's usage %+v", seen[len(seen)-1], final)
	}
	if got := c.usage().Total(); got != 45 {
		t.Errorf("Total() = %d, want input+output = 45", got)
	}
}

// A harness that reports context tokens without a split is metered on that
// total; the split stays zero rather than being invented.
func TestUsageTotalFollowsTheHarnessThatReportsNoSplit(t *testing.T) {
	var last receipt.Usage
	c := &eventCollector{
		adapter: tokenAdapter{}, events: io.Discard,
		onUsage: func(u receipt.Usage) { last = u },
	}
	c.consume(strings.NewReader("total=100\ntotal=250\n"))
	if last.TotalTokens != 350 || last.InputTokens != 0 || last.OutputTokens != 0 {
		t.Fatalf("got %+v, want the totals accumulated and no invented split", last)
	}
	if got := last.Total(); got != 350 {
		t.Errorf("Total() = %d, want 350", got)
	}
	if final := c.usage(); final.TotalTokens != 350 {
		t.Errorf("the envelope lost the harness's total: %+v", final)
	}
}

// --events hands a driver the same lines the receipt gets, as they arrive,
// so it can tail the stream instead of parsing the narration.
func TestEventsAreTeedLiveToTheDriver(t *testing.T) {
	var tee, receiptCopy bytes.Buffer
	c := &eventCollector{
		adapter: tokenAdapter{}, events: &receiptCopy, tee: &tee,
	}
	c.consume(strings.NewReader("in=1 out=2\ntool\n"))
	if tee.String() != receiptCopy.String() {
		t.Fatalf("the tee and the receipt disagree:\ntee:     %q\nreceipt: %q", tee.String(), receiptCopy.String())
	}
	if lines := strings.Count(tee.String(), "\n"); lines != 2 {
		t.Errorf("got %d tee lines, want one per harness line", lines)
	}
}

// No callback and no tee is the ordinary run: nothing added, nothing broken.
func TestUsageReportingIsOptional(t *testing.T) {
	c := &eventCollector{adapter: tokenAdapter{}, events: io.Discard}
	c.consume(strings.NewReader("in=3 out=4\n"))
	if u := c.usage(); u.InputTokens != 3 || u.OutputTokens != 4 {
		t.Fatalf("usage lost without a callback: %+v", u)
	}
}

// The consume goroutine enforces --max-turns and --max-cost, and its end is
// what releases teardown. A driver's callback that panics must not take
// either down with it: the run goes on, the meter is switched off, and the
// failure is remembered rather than read as "no usage".
func TestAPanickingUsageCallbackDoesNotEndTheRun(t *testing.T) {
	calls := 0
	c := &eventCollector{
		adapter: tokenAdapter{}, events: io.Discard,
		onUsage: func(receipt.Usage) { calls++; panic("the driver blew up") },
	}
	c.consume(strings.NewReader("in=1 out=1\nin=2 out=2\ntool\n"))

	if calls != 1 {
		t.Errorf("callback ran %d times; a panicking meter must be switched off after the first", calls)
	}
	if c.usageErr == nil {
		t.Error("the panic was swallowed without trace; a dead meter must be distinguishable from a quiet run")
	}
	// The stream was still consumed to the end: usage accumulated and the
	// turn was still counted, so the budgets still see the run.
	if u := c.usage(); u.InputTokens != 3 || u.OutputTokens != 3 || u.Turns != 1 {
		t.Errorf("the run stopped being metered after the panic: %+v", u)
	}
}
