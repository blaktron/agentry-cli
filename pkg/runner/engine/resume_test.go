package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// shortResumeWaits makes the engine's resume waits short for one test.
func shortResumeWaits(t *testing.T) {
	t.Helper()
	was := resumeWaits
	resumeWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { resumeWaits = was })
}

// pagedHarness plays a docker-agent that reads pages pages of an input, one
// tool call each, keeping its progress in its session store under the
// --session id it is started with, as docker-agent keeps its conversation,
// and reporting the conversation's running cost, as docker-agent does.
// A start given the resume message carries on its stored conversation; a
// start given anything else takes it as the task and begins it from page 1. Each start fails, as the model
// server does, with failure (a line on stderr) after failAfter[n] pages of
// that start, while failAfter has entries. executed counts each page's tool
// runs, so a page read twice shows.
type pagedHarness struct {
	pages     int
	failure   string
	failAfter []int
	cost      map[string]float64

	mu        sync.Mutex
	store     map[string]int // conversation → pages read
	starts    []harnessStart
	executed  map[int]int
	finalText string
}

type harnessStart struct {
	conversation, message string
}

func (h *pagedHarness) handler(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	msg, _ := io.ReadAll(stdin)
	conversation := ""
	for i, a := range r.Argv {
		if a == "--session" && i+1 < len(r.Argv) {
			conversation = r.Argv[i+1]
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.store == nil {
		h.store, h.executed = map[string]int{}, map[int]int{}
	}
	n := len(h.starts)
	h.starts = append(h.starts, harnessStart{conversation: conversation, message: string(msg)})
	read, ok := h.store[conversation]
	switch {
	case string(msg) != resumeMessage:
		read = 0 // the task is begun from its start
	case !ok:
		_, _ = fmt.Fprintln(stderr, "Error: no conversation to carry on")
		return 1, nil
	}
	budget := h.pages
	if n < len(h.failAfter) {
		budget = min(h.pages, read+h.failAfter[n])
	}
	for read < budget {
		read++
		h.executed[read]++
		h.store[conversation] = read
		_, _ = fmt.Fprintf(stdout, `{"type":"tool_call","tool":"read_input","page":%d}`+"\n", read)
	}
	// The conversation's running cost, as docker-agent reports it.
	if h.cost == nil {
		h.cost = map[string]float64{}
	}
	h.cost[conversation] += 0.01
	cost := h.cost[conversation]
	_, _ = fmt.Fprintf(stdout, `{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":10,"cost":%g}}`+"\n", cost)
	if read < h.pages {
		_, _ = fmt.Fprintln(stderr, h.failure)
		return 1, nil
	}
	h.finalText = fmt.Sprintf("read %d pages", read)
	_, _ = fmt.Fprintf(stdout, `{"type":"agent_choice","message":{"content":%q}}`+"\n", h.finalText)
	return 0, nil
}

func (h *pagedHarness) snapshot() ([]harnessStart, map[int]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.starts), h.executed
}

// pagedTask is the task a pagedFixture run is given.
const pagedTask = "read big.txt page by page"

// pagedFixture is a run whose agent container is h, given pagedTask.
func pagedFixture(t *testing.T, h *pagedHarness) (Params, *dockerx.Fake) {
	t.Helper()
	shortResumeWaits(t)
	shortCapacityWait(t)
	p, f := fixture(t)
	happyFake(f)
	p.Task = strings.NewReader(pagedTask)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	return p, f
}

// A harness that ends on a passing model failure after its first tool call
// is started again on the same conversation with the resume message, not the
// task: no page is read twice, the wait is announced, every attempt is in the
// receipts, the usage is the sum of the attempts, and the run succeeds.
func TestAHarnessThatFailsAfterAToolCallResumesItsConversation(t *testing.T) {
	for name, failure := range map[string]string{
		"a server error":       `Error: model failed: POST "http://192.168.1.20:8080/v1/chat/completions": 502 Bad Gateway`,
		"a dropped connection": `Error: model failed: error receiving from stream: read tcp 172.18.0.5:41234->192.168.1.20:8080: read: connection reset by peer`,
		"a stalled stream":     `Error: model stream stalled: no data received from upstream`,
		"an overflow":          `Error: The conversation has exceeded the model's context window. Compact the conversation to reduce its size, or start a new session.`,
		"a rate limit":         `Error: model failed: POST "https://api.example/v1/chat/completions": 429 Too Many Requests`,
	} {
		t.Run(name, func(t *testing.T) {
			h := &pagedHarness{pages: 6, failure: failure, failAfter: []int{2, 3}}
			p, f := pagedFixture(t, h)
			var stderr bytes.Buffer
			p.Stderr = &stderr
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr.String())
			}
			starts, executed := h.snapshot()
			if len(starts) != 3 {
				t.Fatalf("the harness started %d times, want 3: %+v", len(starts), starts)
			}
			if !strings.Contains(starts[0].message, pagedTask) {
				t.Errorf("the first start was given %q, not the task", starts[0].message)
			}
			for i, s := range starts[1:] {
				if s.conversation != starts[0].conversation || s.conversation == "" {
					t.Errorf("start %d kept conversation %q, want %q", i+2, s.conversation, starts[0].conversation)
				}
				if s.message != resumeMessage {
					t.Errorf("start %d was given %q, not the resume message", i+2, s.message)
				}
			}
			for page := 1; page <= 6; page++ {
				if executed[page] != 1 {
					t.Errorf("page %d was read %d times, want once: %v", page, executed[page], executed)
				}
			}
			if string(env.Result) != `"read 6 pages"` {
				t.Errorf("result %s", env.Result)
			}
			if env.Usage.InputTokens != 300 || env.Usage.Turns != 6 || env.Usage.CostUSD < 0.029 {
				t.Errorf("usage %+v, want the three attempts' sum", env.Usage)
			}
			if got := strings.Count(stderr.String(), "starting the harness again on its conversation in"); got != 2 {
				t.Errorf("%d resumes announced, want 2:\n%s", got, stderr.String())
			}
			receipt, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "agent-stderr.log"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(receipt), "agentry: the harness is started again on its conversation"); got != 2 {
				t.Errorf("the stderr receipt marks %d resumes, want 2:\n%s", got, receipt)
			}
			events, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(events), `"tool":"read_input"`); got != 6 {
				t.Errorf("the events receipt holds %d tool calls, want 6 (every attempt's)", got)
			}
		})
	}
}

// A harness that fails on a capacity answer before its first tool call is
// started again on a fresh conversation with the task, as before: nothing
// acted, so nothing is carried on.
func TestACapacityAnswerBeforeAToolCallStartsAFreshConversation(t *testing.T) {
	h := &pagedHarness{pages: 2, failure: "Error: model failed: 529 Overloaded {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}", failAfter: []int{0}}
	p, f := pagedFixture(t, h)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	starts, _ := h.snapshot()
	if len(starts) != 2 || !strings.Contains(starts[1].message, pagedTask) || starts[1].conversation == starts[0].conversation {
		t.Fatalf("starts %+v, want a second start on a fresh conversation with the task", starts)
	}
}

// Failures that are not passing ones, and a passing one before any tool
// call that is not a capacity answer, end the run at its first start.
func TestOtherFailuresAreNotResumed(t *testing.T) {
	for name, tc := range map[string]struct {
		failure   string
		failAfter int
	}{
		"a refused request after a tool call": {`Error: model failed: 400 Bad Request {"error":{"message":"invalid tool schema"}}`, 2},
		"a dropped connection before any":     {`Error: model failed: connection refused`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := &pagedHarness{pages: 4, failure: tc.failure, failAfter: []int{tc.failAfter}}
			p, f := pagedFixture(t, h)
			_, err := Run(context.Background(), f, p)
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitHarness {
				t.Fatalf("err %v", err)
			}
			if starts, _ := h.snapshot(); len(starts) != 1 {
				t.Fatalf("the harness started %d times, want 1", len(starts))
			}
		})
	}
}

// The resumes are bounded: after len(resumeWaits) the run fails with the
// harness's words.
func TestResumesEndAfterTheirTries(t *testing.T) {
	h := &pagedHarness{pages: 100, failure: "Error: model stream stalled: no data received from upstream", failAfter: []int{1, 1, 1, 1, 1, 1, 1, 1}}
	p, f := pagedFixture(t, h)
	_, err := Run(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "model stream stalled") {
		t.Fatalf("err %v, want the harness's words", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 1+len(resumeWaits) {
		t.Fatalf("the harness started %d times, want %d", len(starts), 1+len(resumeWaits))
	}
}

// No resume waits past the run's timeout, which runs from the first start.
func TestNoResumeOutlastsTheTimeout(t *testing.T) {
	h := &pagedHarness{pages: 4, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{1}}
	p, f := pagedFixture(t, h)
	resumeWaits = []time.Duration{time.Hour}
	p.Budgets.TimeoutSeconds = 30
	start := time.Now()
	_, err := Run(context.Background(), f, p)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the run waited %s", time.Since(start))
	}
	if err == nil {
		t.Fatal("the run succeeded")
	}
	if starts, _ := h.snapshot(); len(starts) != 1 {
		t.Fatalf("the harness started %d times, want 1", len(starts))
	}
}

// The turn budget counts every attempt's tool calls.
func TestTheTurnBudgetHoldsAcrossResumes(t *testing.T) {
	h := &pagedHarness{pages: 6, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{3}}
	p, f := pagedFixture(t, h)
	p.Budgets.MaxTurns = 4
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("err %v, want the turn budget", err)
	}
}

// The conversation store is a volume of the run's own, given to the harness
// user before the first start and removed at teardown.
func TestTheConversationStoreIsTheRunsAndGoesWithIt(t *testing.T) {
	h := &pagedHarness{pages: 2, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{1}}
	p, f := pagedFixture(t, h)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	ops := f.Ops()
	volume := sessionVolumeName("cafe0000")
	created := slices.Index(ops, "VolumeCreate:"+volume)
	removed := slices.Index(ops, "VolumeRemove:"+volume)
	if created < 0 || removed < created {
		t.Fatalf("the store was not made and removed: %q", ops)
	}
	init, ok := f.FindRun("-session-init")
	if !ok || !slices.Equal(init.CapAdd, []string{"CHOWN"}) || init.Network != "none" {
		t.Fatalf("the store's one-shot: %+v (found %v)", init, ok)
	}
	if agent, ok := f.FindRun("-agent"); !ok || !onlyPlanAndConversation(agent.Mounts) {
		t.Fatalf("agent mounts %+v", agent.Mounts)
	}
}

// The store goes with a run that fails, after its resumes are spent.
func TestTheConversationStoreGoesWithAFailedRun(t *testing.T) {
	h := &pagedHarness{pages: 100, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{1, 1, 1, 1, 1, 1}}
	p, f := pagedFixture(t, h)
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("the run succeeded")
	}
	if !slices.Contains(f.Ops(), "VolumeRemove:"+sessionVolumeName("cafe0000")) {
		t.Fatalf("the store was not removed: %q", f.Ops())
	}
}

// The timeout runs from the first start: resumes do not renew it, and the
// last one that would end past it is not begun.
func TestTheTimeoutRunsFromTheFirstStartAcrossResumes(t *testing.T) {
	h := &pagedHarness{pages: 100, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{1, 1, 1, 1, 1, 1}}
	p, f := pagedFixture(t, h)
	resumeWaits = []time.Duration{400 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	p.Budgets.TimeoutSeconds = 2
	start := time.Now()
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("the run succeeded")
	}
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("the run took %s, past its 2s timeout", took)
	}
	if starts, _ := h.snapshot(); len(starts) >= 1+len(resumeWaits) || len(starts) < 2 {
		t.Fatalf("the harness started %d times, want fewer than %d and more than one", len(starts), 1+len(resumeWaits))
	}
}

// The cost budget counts every attempt's spend.
func TestTheCostBudgetHoldsAcrossResumes(t *testing.T) {
	h := &pagedHarness{pages: 6, failure: "Error: model failed: 502 Bad Gateway", failAfter: []int{1, 1, 1, 1, 1}}
	p, f := pagedFixture(t, h)
	p.Budgets.MaxCostUSD = 0.025 // each start costs 0.01
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || !strings.Contains(err.Error(), "cost budget") {
		t.Fatalf("err %v, want the cost budget", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 3 {
		t.Fatalf("the harness started %d times, want 3", len(starts))
	}
}

// A resume continues the stored conversation, whose running cost the harness
// reports from where it left off: the run's cost is what the three starts
// added, not their running totals summed.
func TestAResumedConversationsCostIsCountedOnce(t *testing.T) {
	h := &pagedHarness{pages: 6, failure: `Error: model failed: POST "http://192.168.1.20:8080/v1/chat/completions": 502 Bad Gateway`, failAfter: []int{2, 3}}
	p, f := pagedFixture(t, h)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 3 {
		t.Fatalf("the harness started %d times, want 3", len(starts))
	}
	if env.Usage.CostUSD < 0.0299 || env.Usage.CostUSD > 0.0301 {
		t.Fatalf("cost %v, want 0.03", env.Usage.CostUSD)
	}
}
