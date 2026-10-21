package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// overloadedLine is docker-agent's error event for a provider at capacity.
const overloadedLine = `{"type":"error","error":"model failed: POST \"https://api.anthropic.com/v1/messages\": 529 Overloaded {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}","code":"model_error"}`

// shortCapacityWait makes the engine's capacity waits short for one test.
func shortCapacityWait(t *testing.T) {
	t.Helper()
	was := capacityWait
	capacityWait = 10 * time.Millisecond
	t.Cleanup(func() { capacityWait = was })
}

// countedAgent plays the agent container: attempt n (from 1) writes
// streams[n-1] (the last one again past the end) and exits 1 while it holds
// overloadedLine, 0 otherwise.
func countedAgent(streams ...string) (*dockerx.FakeForeground, func() int) {
	var mu sync.Mutex
	n := 0
	fg := &dockerx.FakeForeground{Handler: func(_ context.Context, _ dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
		mu.Lock()
		n++
		i := min(n, len(streams)) - 1
		mu.Unlock()
		_, _ = io.WriteString(stdout, streams[i])
		if strings.Contains(streams[i], "overloaded_error") {
			return 1, nil
		}
		return 0, nil
	}}
	return fg, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

const happyStream = `{"type":"tool_call","tool":"fs_read"}
{"type":"agent_choice","message":{"content":"final answer"}}
{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":40,"cost":0.05}}
`

// A harness that fails on a capacity answer before its first tool call is
// started again after the wait, the wait is announced, the run succeeds,
// and the receipts keep both attempts.
func TestAHarnessAtCapacityBeforeAnyToolCallStartsAgain(t *testing.T) {
	shortCapacityWait(t)
	p, f := fixture(t)
	happyFake(f)
	var stderr bytes.Buffer
	p.Stderr = &stderr
	fg, calls := countedAgent(`{"type":"stream_stopped","usage":{"input_tokens":7,"output_tokens":0}}`+"\n"+overloadedLine+"\n", happyStream)
	f.Foreground["-agent"] = fg
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if calls() != 2 {
		t.Fatalf("the harness started %d times, want 2", calls())
	}
	if string(env.Result) != `"final answer"` {
		t.Fatalf("result %s", env.Result)
	}
	// The failed attempt's tokens are the run's too.
	if env.Usage.InputTokens != 107 || env.Usage.Turns != 1 {
		t.Fatalf("usage %+v", env.Usage)
	}
	if !strings.Contains(stderr.String(), "at capacity; trying again in") {
		t.Fatalf("the wait was not announced: %q", stderr.String())
	}
	events, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "overloaded_error") || !strings.Contains(string(events), "final answer") {
		t.Fatalf("the events receipt lost an attempt:\n%s", events)
	}
}

// A harness that already called a tool is never started again: its tools'
// effects would be repeated. The run fails with the provider's words.
func TestAHarnessAtCapacityAfterAToolCallIsNotStartedAgain(t *testing.T) {
	shortCapacityWait(t)
	p, f := fixture(t)
	happyFake(f)
	fg, calls := countedAgent(`{"type":"tool_call","tool":"fs_read"}`+"\n"+overloadedLine+"\n", happyStream)
	f.Foreground["-agent"] = fg
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("err %v", err)
	}
	if calls() != 1 {
		t.Fatalf("the harness started %d times, want 1", calls())
	}
}

// The waits are bounded: once capacityTries are spent the run fails with
// the provider's words.
func TestCapacityWaitsEndAfterTheirTries(t *testing.T) {
	shortCapacityWait(t)
	p, f := fixture(t)
	happyFake(f)
	fg, calls := countedAgent(overloadedLine + "\n")
	f.Foreground["-agent"] = fg
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("err %v", err)
	}
	if calls() != capacityTries+1 {
		t.Fatalf("the harness started %d times, want %d", calls(), capacityTries+1)
	}
}

// A wait that would outlast the run's timeout is not taken: the run fails
// at once with the provider's words, not later as a timeout.
func TestNoCapacityWaitOutlastsTheTimeout(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.Budgets.TimeoutSeconds = 30 // under the 60-second wait
	fg, calls := countedAgent(overloadedLine+"\n", happyStream)
	f.Foreground["-agent"] = fg
	start := time.Now()
	_, err := Run(context.Background(), f, p)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the run waited %s", time.Since(start))
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness || calls() != 1 {
		t.Fatalf("err %v after %d starts", err, calls())
	}
}

// capacityDriver fails its first prompt as a provider at capacity does,
// and passes every later one to the session it wraps.
type capacityDriver struct {
	adapter.SessionDriver
	mu      sync.Mutex
	prompts int
}

func (d *capacityDriver) Prompt(ctx context.Context, text string) (*adapter.Turn, error) {
	d.mu.Lock()
	d.prompts++
	first := d.prompts == 1
	d.mu.Unlock()
	if first {
		return nil, errors.New("codex session: turn failed: Selected model is at capacity. Please try a different model.")
	}
	return d.SessionDriver.Prompt(ctx, text)
}

type capacityAdapter struct {
	adapter.Adapter
	drv *capacityDriver
}

func (a *capacityAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
	d, err := a.Adapter.Session(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	a.drv = &capacityDriver{SessionDriver: d}
	return a.drv, nil
}

// A session's turn that fails on a capacity answer is sent again on the
// same session after the wait, which the transcript announces.
func TestASessionTurnAtCapacityIsSentAgain(t *testing.T) {
	shortCapacityWait(t)
	p, f := sessionFixture(t)
	happyFake(f)
	ad := &capacityAdapter{Adapter: p.Adapter}
	p.Adapter = ad
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	ctx := context.Background()
	if err := s.Send(ctx, "hello"); err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if ad.drv.prompts != 2 {
		t.Fatalf("prompted %d times, want 2", ad.drv.prompts)
	}
	if _, err := s.End(ctx, true); err != nil {
		t.Fatal(err)
	}
	<-done
	var notice, answered bool
	for _, ev := range get() {
		switch ev.Kind {
		case session.KindNotice:
			notice = notice || strings.Contains(ev.Text, "at capacity")
		case session.KindAssistantTurn:
			answered = true
		}
	}
	if !notice || !answered {
		t.Fatalf("notice %v, answered %v: %+v", notice, answered, get())
	}
}
