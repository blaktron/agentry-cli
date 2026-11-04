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

	// A harness that reports exactly the budget is within it.
	c = &eventCollector{adapter: lineAdapter{}, events: io.Discard, maxCostUSD: 0.50, overCost: func() { t.Fatal("the budget itself must not trip") }}
	c.consume(strings.NewReader("cost=0.25\ncost=0.50\n"))
	if u := c.usage(); u.CostUSD != 0.50 {
		t.Fatalf("cost at the budget: %+v", u)
	}

	// A run paid by a sign-in reports tokens, not the harness's dollars: its
	// cost is not recorded and never trips the cost budget.
	c = &eventCollector{adapter: lineAdapter{}, events: io.Discard, signIn: true, maxCostUSD: 0.50, overCost: func() { t.Fatal("a sign-in run has no dollars to stop on") }}
	c.consume(strings.NewReader("cost=3.00\n"))
	if u := c.usage(); u.CostUSD != 0 {
		t.Fatalf("a sign-in run records the harness's cost: %+v", u)
	}

	// The turn budget stops on the tool call past the limit, once, and an
	// unset one never does.
	turns := 0
	c = &eventCollector{adapter: lineAdapter{}, events: io.Discard, maxTurns: 2,
		overBudget: func() { turns++ }, overCost: func() { t.Fatal("cost budget must not trip") }}
	c.consume(strings.NewReader("tool\ntool\n"))
	if turns != 0 {
		t.Fatalf("the turn budget tripped at its limit")
	}
	c.consume(strings.NewReader("tool\ntool\n"))
	if turns != 1 {
		t.Fatalf("the turn budget tripped %d times, want once", turns)
	}
	if u := c.usage(); u.Turns != 4 {
		t.Fatalf("turns after the stop: %+v", u)
	}
}
