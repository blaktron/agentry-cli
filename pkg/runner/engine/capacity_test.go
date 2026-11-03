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
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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

	// A harness that fails for another reason before any tool call, and one
	// that answers without calling any, are started once.
	for name, stream := range map[string]string{
		"another failure": `{"type":"error","error":"model failed: 400 invalid request","code":"model_error"}` + "\n",
		"no tool call":    `{"type":"agent_choice","message":{"content":"final answer"}}` + "\n",
	} {
		p, f := fixture(t)
		happyFake(f)
		fg, calls := countedAgent(stream)
		f.Foreground["-agent"] = fg
		_, _ = Run(context.Background(), f, p)
		if calls() != 1 {
			t.Errorf("%s: the harness started %d times, want 1", name, calls())
		}
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
	t.Run("what bounds the sending again", turnsAtCapacityAreBounded)
}

// turnsAtCapacityAreBounded sends a turn through promptAtCapacity to a
// driver that answers with the errors given: the turn is sent again at most
// capacityTries times, only after a capacity answer, only while the caller
// allows it and only while the wait can end before the context does.
func turnsAtCapacityAreBounded(t *testing.T) {
	shortCapacityWait(t)
	const atCapacity = "codex session: turn failed: Selected model is at capacity. Please try a different model."
	fails := func(n int, msg string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = msg
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		within  time.Duration // the run's time left, none when zero
		wait    time.Duration // the capacity wait, the short test one when zero
		inError bool          // asking at all is the error: the context ends then, so no wait is waited
		fails   []string
		allow   bool
		prompts int // prompts sent
		asked   int // times the caller was asked before a wait
		ok      bool
	}{
		{"as many capacity answers as the tries", 0, 0, false, fails(capacityTries, atCapacity), true, capacityTries + 1, capacityTries, true},
		{"one more than the tries", 0, 0, false, fails(capacityTries+1, atCapacity), true, capacityTries + 1, capacityTries, false},
		{"an answer that is not a capacity one", 0, 0, false, fails(1, "codex session: turn failed: invalid request"), true, 1, 0, false},
		{"a caller that allows no more", 0, 0, false, fails(2, atCapacity), false, 1, 1, false},
		{"a run whose end is inside the wait", 30 * time.Minute, time.Hour, true, fails(1, atCapacity), true, 1, 0, false},
		{"a run whose end is well past the wait", time.Hour, 0, false, fails(1, atCapacity), true, 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wait > 0 {
				was := capacityWait
				capacityWait = tc.wait
				t.Cleanup(func() { capacityWait = was })
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.within > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, tc.within)
				defer stop()
			}
			drv := &scriptedDriver{fails: tc.fails, events: make(chan adapter.Event, 16)}
			asked := 0
			// Asked before a wait that the run's end should have ruled out, the
			// caller ends the context, so a wait taken in error is not waited.
			_, err := promptAtCapacity(ctx, drv, "hello", func(int) bool {
				asked++
				if tc.inError {
					cancel()
				}
				return tc.allow
			})
			if (err == nil) != tc.ok || drv.prompts != tc.prompts || asked != tc.asked {
				t.Fatalf("err %v after %d prompts and %d asks, want ok %v after %d and %d", err, drv.prompts, asked, tc.ok, tc.prompts, tc.asked)
			}
		})
	}
	// The wait is not taken inside a context that has ended.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if canWaitAtCapacity(ended) {
		t.Error("a wait was allowed in a context that has ended")
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

// docker-agent spends its own quick retries and then prints the provider's
// words only as its last stderr line: that too is a capacity answer.
func TestAHarnessAtCapacityOnItsStderrStartsAgain(t *testing.T) {
	shortCapacityWait(t)
	p, f := fixture(t)
	happyFake(f)
	var mu sync.Mutex
	n := 0
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(_ context.Context, _ dockerx.RunSpec, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			_, _ = io.WriteString(stderr, "Welcome to docker agent!\n\nError: model failed: error receiving from stream: HTTP 503: Selected model is at capacity. Please try a different model.\n")
			return 1, nil
		}
		_, _ = io.WriteString(stdout, happyStream)
		return 0, nil
	}}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("the harness started %d times, want 2", n)
	}
	t.Run("what a restart carries", harnessRestartBookkeeping)
}

// harnessRestartBookkeeping holds what a harness started again carries from
// its earlier starts: one deadline for all of them, receipts made new for the
// first start and added to for the others, the usage of every start, and the
// last stderr lines read for the provider's words.
func harnessRestartBookkeeping(t *testing.T) {
	t.Run("one deadline for every start", func(t *testing.T) {
		rd := &runDirs{}
		p := Params{}
		p.Budgets.TimeoutSeconds = 3600
		first, cancel := rd.harnessContext(context.Background(), p)
		defer cancel()
		again, cancelAgain := rd.harnessContext(context.Background(), p)
		defer cancelAgain()
		d1, ok1 := first.Deadline()
		d2, ok2 := again.Deadline()
		if !ok1 || !ok2 || !d1.Equal(d2) {
			t.Errorf("deadlines %v (%v) and %v (%v), want the same one", d1, ok1, d2, ok2)
		}
		p.Budgets.TimeoutSeconds = 0
		none, cancelNone := (&runDirs{}).harnessContext(context.Background(), p)
		defer cancelNone()
		if _, ok := none.Deadline(); ok {
			t.Error("a run with no timeout got a deadline")
		}
	})
	t.Run("receipts", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.ndjson")
		if err := os.WriteFile(path, []byte("an earlier run\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rd := &runDirs{}
		for i, line := range []string{"first\n", "second\n"} {
			rd.harnessStarts = i
			f, err := rd.harnessReceipt(path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(f, line)
			_ = f.Close()
		}
		if b, _ := os.ReadFile(path); string(b) != "first\nsecond\n" {
			t.Errorf("receipt %q, want an earlier run's lines gone and both starts kept", b)
		}
	})
	t.Run("usage", func(t *testing.T) {
		got := withBase(receipt.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CostUSD: 0.5, Turns: 4},
			receipt.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CostUSD: 5, Turns: 40})
		if got != (receipt.Usage{InputTokens: 11, OutputTokens: 22, TotalTokens: 33, CostUSD: 5.5, Turns: 44}) {
			t.Errorf("usage %+v", got)
		}
	})
	t.Run("stderr", func(t *testing.T) {
		const words = "Error: model failed: HTTP 503: Selected model is at capacity. Please try a different model."
		p := Params{Plan: &spec.Plan{}}
		for _, tc := range []struct {
			name, stderr string
			want         bool
		}{
			{"the last line", "a\nb\n" + words + "\n", true},
			{"the third from last", words + "\nb\nc\n", true},
			{"blank lines are no lines", words + "\nb\n\nc\n\n\n", true},
			{"the fourth from last", words + "\nb\nc\nd\n", false},
			{"no capacity words", "a\nb\nc\n", false},
		} {
			path := filepath.Join(t.TempDir(), "agent-stderr.log")
			if err := os.WriteFile(path, []byte(tc.stderr), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := exitedAtCapacity(p, "", path); got != tc.want {
				t.Errorf("%s: exitedAtCapacity = %v, want %v", tc.name, got, tc.want)
			}
		}
		if !exitedAtCapacity(p, words, filepath.Join(t.TempDir(), "absent.log")) {
			t.Error("the error the harness reported on its events was not read")
		}
		oneTurn := Params{Plan: &spec.Plan{Harness: spec.HarnessSpec{OneTurn: &spec.SessionSpec{}}}}
		if exitedAtCapacity(oneTurn, words, filepath.Join(t.TempDir(), "absent.log")) {
			t.Error("a run driven as one turn was taken for a harness to start again")
		}
	})
}
