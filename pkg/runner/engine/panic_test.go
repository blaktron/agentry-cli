package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// panicAdapter's event mapper panics on every line, which is what an adapter
// bug — or a harness event the mapper did not expect — would do.
type panicAdapter struct{ adapter.Adapter }

func (panicAdapter) Events([]byte) []adapter.Event {
	panic("adapter mapper exploded")
}

// TestRunPanicInTheEventStreamTearsDownAndNamesIt (agentry-cli#161): the
// goroutine that consumes the harness's event stream calls the adapter's
// mapper and the --events tee writer with nothing guarding them, and a panic
// in a goroutine takes the whole process down — running none of the defers in
// Run's own goroutine, which is where teardown lives. That left the agent
// container holding a valid gateway token, the gateway holding the docker
// socket, and the run network and secrets.env on disk, bounded by nothing
// because the turn, cost and wall budgets are all enforced by the process
// that died.
//
// It is a full Run, not a test of consume in isolation: the claim is about
// what the process does next, and only the real path can show the teardown
// running and the panic reaching the envelope.
func TestRunPanicInTheEventStreamTearsDownAndNamesIt(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.Adapter = panicAdapter{}
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: "an event line the mapper chokes on\n", ExitCode: 0},
	}

	env, err := Run(context.Background(), f, p)
	if err == nil {
		t.Fatal("a run whose event stream panicked must fail")
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI {
		t.Fatalf("want exit class %d, got %v", ExitCLI, err)
	}
	if !strings.Contains(err.Error(), "adapter mapper exploded") {
		t.Fatalf("the run's error must name the panic, got: %v", err)
	}
	if env == nil || env.Status != "error" || !strings.Contains(env.Error, "adapter mapper exploded") {
		t.Fatalf("the envelope must name the panic: %+v", env)
	}
	// Teardown ran because the run failed normally instead of dying: removing
	// the run network is one of the steps teardown registers and the fake
	// records it, so its presence is the proof the deferred teardown executed
	// rather than the process being killed out from under it.
	joined := strings.Join(f.Ops(), "\n")
	if !strings.Contains(joined, "NetworkRemove:agentry-cafe0000-net") {
		t.Fatalf("teardown did not run: %v", f.Ops())
	}
}
