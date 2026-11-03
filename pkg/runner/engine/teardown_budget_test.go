package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// liveCtxClient records which removals ran on a context that was still
// live, and holds a stop until its context ends.
type liveCtxClient struct {
	*dockerx.Fake
	mu      sync.Mutex
	removed map[string]bool
	order   []string
	swept   bool
	// firstStop is when the first stop began; lastDeadline the latest
	// deadline any stop was given.
	firstStop, lastDeadline time.Time
}

func (c *liveCtxClient) Stop(ctx context.Context, name string, _ int) error {
	c.mu.Lock()
	if c.firstStop.IsZero() {
		c.firstStop = time.Now()
	}
	if dl, ok := ctx.Deadline(); ok && dl.After(c.lastDeadline) {
		c.lastDeadline = dl
	}
	c.order = append(c.order, "stop "+name)
	c.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (c *liveCtxClient) RemoveForce(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() == nil {
		c.removed[name] = true
	}
	c.order = append(c.order, "remove "+name)
	return nil
}

func (c *liveCtxClient) PSByLabel(ctx context.Context, _ string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.swept = ctx.Err() == nil
	return []string{"agentry-cafe0000-leftover"}, nil
}

func shrinkTeardownBudgets(t *testing.T, step, steps, sweep time.Duration) {
	t.Helper()
	s, ss, sw := teardownStepBudget, teardownStepsBudget, teardownSweepBudget
	teardownStepBudget, teardownStepsBudget, teardownSweepBudget = step, steps, sweep
	t.Cleanup(func() { teardownStepBudget, teardownStepsBudget, teardownSweepBudget = s, ss, sw })
}

// A step that spends its whole budget (a gateway's stop on a busy machine)
// does not take the steps registered before it, which run after it, nor
// the sweep of the run's label: each runs on a live context.
func TestASlowTeardownStepLeavesTheLaterStepsAndTheSweepTheirOwnBudget(t *testing.T) {
	shrinkTeardownBudgets(t, 50*time.Millisecond, time.Second, time.Second)
	c := &liveCtxClient{Fake: &dockerx.Fake{}, removed: map[string]bool{}}
	td := &teardown{d: c, runID: "cafe0000"}
	td.add(func(ctx context.Context) { _ = c.RemoveForce(ctx, "agentry-cafe0000-proxy") })
	td.add(func(ctx context.Context) { _ = c.RemoveForce(ctx, "agentry-cafe0000-browse-proxy") })
	td.add(func(ctx context.Context) {
		_ = c.Stop(ctx, "agentry-cafe0000-gateway", 10)
		_ = c.RemoveForce(ctx, "agentry-cafe0000-gateway")
	})
	td.run()
	for _, name := range []string{"agentry-cafe0000-proxy", "agentry-cafe0000-browse-proxy"} {
		if !c.removed[name] {
			t.Errorf("%s was not removed on a live context after a slow step", name)
		}
	}
	if !c.swept {
		t.Error("the sweep of the run's label ran on an expired context")
	}
	if !c.removed["agentry-cafe0000-leftover"] {
		t.Error("what the sweep found carrying the run's label was not removed on a live context")
	}
	// The steps run in the reverse of the order they were registered: what
	// was started last is stopped first, and the sweep comes after them all.
	want := []string{"stop agentry-cafe0000-gateway", "remove agentry-cafe0000-gateway", "remove agentry-cafe0000-browse-proxy", "remove agentry-cafe0000-proxy", "remove agentry-cafe0000-leftover"}
	if !slices.Equal(c.order, want) {
		t.Errorf("the steps ran %q, want %q", c.order, want)
	}
}

// When every step spends its budget the steps stop at the ceiling, and the
// sweep still has a live context of its own.
func TestTheSweepRunsAfterTheTeardownStepsHitTheirCeiling(t *testing.T) {
	shrinkTeardownBudgets(t, 40*time.Millisecond, 100*time.Millisecond, time.Second)
	c := &liveCtxClient{Fake: &dockerx.Fake{}, removed: map[string]bool{}}
	td := &teardown{d: c, runID: "cafe0000"}
	start := time.Now()
	for range 20 {
		td.add(func(ctx context.Context) { _ = c.Stop(ctx, "x", 10) })
	}
	td.run()
	if !c.swept {
		t.Error("the sweep of the run's label ran on an expired context")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("teardown took %v: the steps' ceiling did not hold", took)
	}
	// No step was given a context that outlives the ceiling, which counts
	// from the start of the first: 20 steps of 40ms would otherwise run on
	// to 800ms.
	if over := c.lastDeadline.Sub(c.firstStop); over > 100*time.Millisecond {
		t.Errorf("a step was given a deadline %v after the first began, past the %v ceiling", over, teardownStepsBudget)
	}
}
