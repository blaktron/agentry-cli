package inference_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/inference/inferencetest"
)

// The client opens, renews and ends a session with the sign-in as its
// bearer, and maps the registry's refusals: 401 signed out, 402 no
// credits, 410 ended.
func TestTheClientOpensRenewsAndEndsASession(t *testing.T) {
	reg := inferencetest.New(t)
	c := inference.New(reg.URL+"/", func() (string, error) { return inferencetest.SignIn, nil }, nil)
	o, err := c.Open(context.Background(), inference.PurposeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if o.Token != inferencetest.TokenFor(1) || o.Model != inferencetest.Model || o.RenewEvery != 15*time.Minute || o.Session.ID != "s-1" {
		t.Fatalf("opened %+v", o)
	}
	if _, err := c.Renew(context.Background(), o.Session.ID); err != nil {
		t.Fatal(err)
	}
	reg.Credits = 3
	v, err := c.End(context.Background(), o.Session.ID)
	if err != nil || !v.Ended || v.Credits != 3 {
		t.Fatalf("end: %+v %v", v, err)
	}
	if _, err := c.Renew(context.Background(), o.Session.ID); !errors.Is(err, inference.ErrEnded) {
		t.Fatalf("renew after end: %v", err)
	}
	reg.NoCredits = true
	if _, err := c.Open(context.Background(), inference.PurposeRun); !errors.Is(err, inference.ErrNoCredits) {
		t.Fatalf("no credits: %v", err)
	}
	other := inference.New(reg.URL, func() (string, error) { return "agy_someoneelse", nil }, nil)
	if _, err := other.Open(context.Background(), inference.PurposeRun); !errors.Is(err, inference.ErrSignedOut) {
		t.Fatalf("a refused sign-in: %v", err)
	}
	none := inference.New(reg.URL, func() (string, error) { return "", nil }, nil)
	if _, err := none.Open(context.Background(), inference.PurposeRun); !errors.Is(err, inference.ErrSignedOut) {
		t.Fatalf("no sign-in: %v", err)
	}
}

// A refused sign-in says what to do: sign in again by default, or the advice
// the client was given for a sign-in that is not this computer's.
func TestARefusedSignInSaysWhatToDo(t *testing.T) {
	reg := inferencetest.New(t)
	def := inference.New(reg.URL, func() (string, error) { return "agy_someoneelse", nil }, nil)
	if _, err := def.Open(context.Background(), inference.PurposeRun); !errors.Is(err, inference.ErrSignedOut) || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("default advice: %v", err)
	}
	set := inference.New(reg.URL, func() (string, error) { return "agy_someoneelse", nil }, nil).WithSignInAdvice("give the runhost a new token")
	_, err := set.Open(context.Background(), inference.PurposeRun)
	if !errors.Is(err, inference.ErrSignedOut) || !strings.Contains(err.Error(), "(give the runhost a new token)") || strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("set advice: %v", err)
	}
}
