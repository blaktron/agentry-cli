package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// A provider out of credits (an HTTP 402, docker-agent's error event) ends
// the run with its own outcome and the provider's words: the budget class,
// reason out-of-credits, never a success.
func TestAProviderOutOfCreditsEndsTheRunWithItsReason(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.Foreground["-agent"].Stdout = strings.Join([]string{
		`{"type":"agent_choice","message":{"content":"working"}}`,
		`{"type":"error","error":"POST \"https://openrouter.ai/api/v1/chat/completions\": 402 Payment Required {\"error\":{\"message\":\"Insufficient credits. Add more using https://openrouter.ai/settings/credits\",\"code\":402}}","code":"model_error"}`,
	}, "\n") + "\n"
	f.Foreground["-agent"].ExitCode = 1
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("want exit %d, got %v", ExitBudget, err)
	}
	for _, s := range []string{"out of credits", "Insufficient credits", "402 Payment Required"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the error does not say %q: %v", s, err)
		}
	}
	if env.Status != "error" || env.Reason == nil || env.Reason.Code != receipt.ReasonOutOfCredits {
		t.Fatalf("envelope %s, reason %+v", env.Status, env.Reason)
	}
}

// Any other harness error keeps its words in the run's failure: docker-agent
// puts them in the event's "error" field.
func TestADockerAgentErrorEventsWordsReachTheFailure(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.Foreground["-agent"].Stdout = `{"type":"error","error":"model failed: the provider is down","code":"model_error"}` + "\n"
	f.Foreground["-agent"].ExitCode = 1
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "the provider is down") {
		t.Fatalf("err %v", err)
	}
}

// A harness that keeps running after its provider's 402 is ended by it,
// as by a plan's limit: the run does not wait for its timeout.
func TestAProviderOutOfCreditsEndsAHarnessThatKeepsRunning(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.Foreground["-agent"].Stdout = `{"type":"error","error":"POST \"https://api.poe.com/v1/chat/completions\": 402 Payment Required","code":"model_error"}` + "\n"
	f.Foreground["-agent"].Block = true
	start := time.Now()
	env, err := Run(context.Background(), f, p)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the run waited %s for its timeout", time.Since(start))
	}
	if env.Reason == nil || env.Reason.Code != receipt.ReasonOutOfCredits || !strings.Contains(err.Error(), "402 Payment Required") {
		t.Fatalf("err %v, reason %+v", err, env.Reason)
	}
}

// outOfCreditsAdapter is the session fixture's adapter whose driver
// reports a provider out of credits after its first turn.
type outOfCreditsAdapter struct{ adapter.Adapter }

func (a outOfCreditsAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
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
	return &outOfCreditsDriver{SessionDriver: d, out: out}, nil
}

type outOfCreditsDriver struct {
	adapter.SessionDriver
	out  chan adapter.Event
	once sync.Once
}

func (d *outOfCreditsDriver) Events() <-chan adapter.Event { return d.out }

func (d *outOfCreditsDriver) Prompt(ctx context.Context, text string) (*adapter.Turn, error) {
	turn, err := d.SessionDriver.Prompt(ctx, text)
	d.once.Do(func() {
		d.out <- adapter.Event{Kind: adapter.EventOutOfCredits, Text: "402 Payment Required: insufficient_credits"}
	})
	return turn, err
}

// A provider out of credits ends a session with its own reason and outcome:
// the out_of_credits end, exit 16 with reason out-of-credits and the
// provider's words, and no further turn.
func TestAProviderOutOfCreditsEndsASession(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	p.Adapter = outOfCreditsAdapter{p.Adapter}
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	ctx := context.Background()
	_ = s.Send(ctx, "first turn")
	deadline := time.After(10 * time.Second)
	for {
		if ended, reason := s.Ended(); ended {
			if reason != session.EndOutOfCredits {
				t.Fatalf("ended for %s", reason)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("running out of credits never ended the session")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := s.Send(ctx, "another"); err != session.ErrEnded {
		t.Fatalf("a turn after it: %v", err)
	}
	env, err := s.End(ctx, true)
	<-done
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || env.Reason == nil || env.Reason.Code != receipt.ReasonOutOfCredits ||
		!strings.Contains(err.Error(), "insufficient_credits") {
		t.Fatalf("end = %v (%+v)", err, env.Reason)
	}
	if last := get()[len(get())-1]; last.Kind != session.KindSessionEnd || last.Reason != session.EndOutOfCredits {
		t.Fatalf("last event %+v", last)
	}
}
