package engine

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// lineAdapter turns each line into one event: "tool" is a tool call,
// "cost=<usd>" a usage event carrying the harness's running total.
type lineAdapter struct{ adapter.Adapter }

func (lineAdapter) Events(line []byte) []adapter.Event {
	s := strings.TrimSpace(string(line))
	switch {
	case s == "tool":
		return []adapter.Event{{Kind: adapter.EventToolCall}}
	case strings.HasPrefix(s, "cost="):
		c, _ := strconv.ParseFloat(s[5:], 64)
		return []adapter.Event{{Kind: adapter.EventUsage, CostUSD: c}}
	}
	return nil
}

func TestCollectorCostBudgetIsAHardStop(t *testing.T) {
	var tripped int
	c := &eventCollector{
		adapter: lineAdapter{}, events: io.Discard,
		maxCostUSD: 0.50, overCost: func() { tripped++ },
		overBudget: func() { t.Fatal("turn budget must not trip") },
	}
	c.consume(strings.NewReader("cost=0.10\ntool\ncost=0.40\ncost=0.51\ncost=0.60\ncost=0.30\n"))
	if tripped != 1 {
		t.Fatalf("cost budget must trip exactly once, got %d", tripped)
	}
	if u := c.usage(); u.CostUSD != 0.60 || u.Turns != 1 {
		t.Fatalf("high-water mark cost + turn count expected, got %+v", u)
	}
	// Unlimited: never trips.
	c = &eventCollector{adapter: lineAdapter{}, events: io.Discard, overCost: func() { t.Fatal("unlimited must not trip") }}
	c.consume(strings.NewReader("cost=999\n"))
}
