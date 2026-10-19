package engine

import (
	"context"
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
	swept   bool
}

func (c *liveCtxClient) Stop(ctx context.Context, _ string, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *liveCtxClient) RemoveForce(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() == nil {
		c.removed[name] = true
	}
	return nil
}

func (c *liveCtxClient) PSByLabel(ctx context.Context, _ string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.swept = ctx.Err() == nil
	return nil, nil
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
}
