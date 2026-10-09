package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// A provider out of credits (an HTTP 402, docker-agent's error event) ends
// the run with its own outcome and the provider's words: the budget class,
// reason out-of-credits, never a success (agentry-cli#742).
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
// puts them in the event's "error", which was read as no text before.
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
