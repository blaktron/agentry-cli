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

// toolFirstDriver fails its first prompt at capacity after the turn called a
// tool, and passes every later one to the session it wraps.
type toolFirstDriver struct {
	*capacityDriver
	out chan adapter.Event
}

func (d *toolFirstDriver) Events() <-chan adapter.Event { return d.out }

func (d *toolFirstDriver) Prompt(ctx context.Context, text string) (*adapter.Turn, error) {
	d.mu.Lock()
	first := d.prompts == 0
	d.mu.Unlock()
	if first {
		d.out <- adapter.Event{Kind: adapter.EventToolCall, Text: "fs_write", Tool: "fs_write"}
	}
	return d.capacityDriver.Prompt(ctx, text)
}

type toolFirstAdapter struct {
	adapter.Adapter
	drv *toolFirstDriver
}

func (a *toolFirstAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
	d, err := a.Adapter.Session(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	out := make(chan adapter.Event, 64)
	go func() {
		for ev := range d.Events() {
			out <- ev
		}
		close(out)
	}()
	a.drv = &toolFirstDriver{capacityDriver: &capacityDriver{SessionDriver: d}, out: out}
	return a.drv, nil
}

// A session's turn that called a tool before the capacity answer is not
// sent again: its tools could act twice. The turn fails with the
// provider's words.
func TestASessionTurnAtCapacityAfterAToolCallIsNotSentAgain(t *testing.T) {
	shortCapacityWait(t)
	p, f := sessionFixture(t)
	happyFake(f)
	ad := &toolFirstAdapter{Adapter: p.Adapter}
	p.Adapter = ad
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	ctx := context.Background()
	if err := s.Send(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Fatalf("Send = %v, want the provider's words", err)
	}
	if ad.drv.prompts != 1 {
		t.Fatalf("prompted %d times, want 1", ad.drv.prompts)
	}
	_, _ = s.End(ctx, true)
	<-done
	for _, ev := range get() {
		if ev.Kind == session.KindNotice && strings.Contains(ev.Text, "at capacity") {
			t.Fatalf("a wait was announced: %+v", ev)
		}
	}
}

// scriptedDriver is a session whose prompts fail with the errors given, in
// order, then answer; toolOnFirst records a tool call in the first turn.
type scriptedDriver struct {
	fails       []string
	toolOnFirst bool
	mu          sync.Mutex
	prompts     int
	events      chan adapter.Event
}

func (d *scriptedDriver) Start(context.Context) error  { return nil }
func (d *scriptedDriver) Events() <-chan adapter.Event { return d.events }
func (d *scriptedDriver) Close() error                 { close(d.events); return nil }
func (d *scriptedDriver) Prompt(context.Context, string) (*adapter.Turn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prompts++
	if d.prompts == 1 && d.toolOnFirst {
		d.events <- adapter.Event{Kind: adapter.EventToolCall, Text: "fs_write", Tool: "fs_write"}
	}
	if d.prompts <= len(d.fails) {
		return nil, errors.New(d.fails[d.prompts-1])
	}
	d.events <- adapter.Event{Kind: adapter.EventAssistantText, Text: "done"}
	return &adapter.Turn{StopReason: "completed", Text: "done"}, nil
}

type scriptedAdapter struct {
	adapter.Adapter
	drv *scriptedDriver
}

func (a scriptedAdapter) Session(context.Context, io.ReadWriteCloser, adapter.SessionOptions) (adapter.SessionDriver, error) {
	return a.drv, nil
}

// A run driven as one turn sends its turn again after a capacity answer
// while it has called no tool, and not after one.
func TestAOneTurnRunAtCapacityIsSentAgainBeforeAnyToolCall(t *testing.T) {
	shortCapacityWait(t)
	const atCapacity = "codex session: turn failed: Selected model is at capacity. Please try a different model."
	for _, tc := range []struct {
		name    string
		tool    bool
		prompts int
		ok      bool
	}{
		{"before a tool call", false, 2, true},
		{"after a tool call", true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drv := &scriptedDriver{fails: []string{atCapacity}, toolOnFirst: tc.tool, events: make(chan adapter.Event, 16)}
			var stderr bytes.Buffer
			p := Params{Adapter: scriptedAdapter{drv: drv}, Stderr: &stderr}
			c := &eventCollector{adapter: p.Adapter, events: io.Discard}
			rw := rwPipe{Reader: strings.NewReader(""), WriteCloser: nopWriteCloser{io.Discard}}
			err := oneTurn(context.Background(), p, rw, []byte("task"), adapter.SessionOptions{}, c, func(error) {})
			if (err == nil) != tc.ok || drv.prompts != tc.prompts {
				t.Fatalf("err %v after %d prompts", err, drv.prompts)
			}
			if announced := strings.Contains(stderr.String(), "at capacity"); announced != tc.ok {
				t.Fatalf("stderr %q", stderr.String())
			}
		})
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
