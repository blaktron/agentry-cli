package engine

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/inference/inferencetest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// creditsFixture is the engine fixture compiled under a policy that pays
// with Agentry credits at a fake registry, and a client of that registry
// as the signed-in account.
func creditsFixture(t *testing.T) (Params, *dockerx.Fake, *inferencetest.Registry, *inference.Client) {
	t.Helper()
	reg := inferencetest.New(t)
	u, _ := url.Parse(reg.URL)
	p, f := fixtureWith(t, func(pol *policy.Policy) {
		pol.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: reg.URL + policy.AgentryInferencePath,
			ProxyAllow: []string{u.Host}}
	})
	c := inference.New(reg.URL, func() (string, error) { return inferencetest.SignIn, nil }, nil)
	return p, f, reg, c
}

// A run that pays with credits hands its agent container the session's
// token as its model key, under the named provider's variable, and the
// person's sign-in reaches no container, no argument and no receipt. The
// session is ended once the run's containers are gone, and its count is
// the receipt's inference block.
func TestACreditPaidRunHandsItsContainerTheSessionTokenAlone(t *testing.T) {
	p, f, reg, c := creditsFixture(t)
	happyFake(f)
	var agentEnv map[string]string
	var endedBeforeAgent bool
	f.Foreground["-agent"].Before = func(r dockerx.RunSpec) {
		agentEnv = r.Env
		endedBeforeAgent = len(reg.Ended()) > 0
	}
	reg.InputTokens, reg.OutputTokens, reg.Credits = 1200, 300, 1
	// The harness quotes the session token it was given, on its event stream
	// and on its stderr: neither receipt keeps it.
	quoted := inferencetest.TokenFor(1)
	f.Foreground["-agent"].Stdout = `{"type":"agent_choice","message":{"content":"my key is ` + quoted + `"}}` + "\n" + f.Foreground["-agent"].Stdout
	f.Foreground["-agent"].Stderr = "key in use: " + quoted + "\n"
	var stderr bytes.Buffer
	g, err := InferenceFor(context.Background(), c, p.Plan, false, inference.PurposeRun, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	p.Inference = g
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run: %v (stderr %s)", err, stderr.String())
	}
	if note := "agentry: paying for the model with Agentry credits at " + reg.URL + " (" + inferencetest.Model + ")\n"; !strings.Contains(stderr.String(), note) {
		t.Fatalf("the run did not say it pays with credits: %q", stderr.String())
	}
	if got := agentEnv[adapter.AgentryInferenceTokenEnv]; got != inferencetest.TokenFor(1) {
		t.Fatalf("the agent's %s = %q, want the session's token", adapter.AgentryInferenceTokenEnv, got)
	}
	if endedBeforeAgent {
		t.Fatal("the session was ended before the agent ran")
	}
	if got := reg.Opened(); len(got) != 1 || got[0] != inference.PurposeRun {
		t.Fatalf("opened %v, want one run session", got)
	}
	if got := reg.Ended(); len(got) != 1 || got[0] != "s-1" {
		t.Fatalf("ended %v, want s-1 once", got)
	}
	want := receipt.Inference{Session: "s-1", Model: inferencetest.Model, Requests: 2, InputTokens: 1200, OutputTokens: 300, Credits: 1}
	if env.Inference == nil || *env.Inference != want {
		t.Fatalf("envelope inference = %+v, want %+v", env.Inference, want)
	}
	if !strings.Contains(p.Plan.Enforcement["model.apiKey"], "Agentry credits session token") {
		t.Fatalf("model.apiKey line = %q", p.Plan.Enforcement["model.apiKey"])
	}
	// The sign-in is nowhere the run reached.
	var blob strings.Builder
	for _, call := range f.Calls {
		blob.WriteString(call.Arg + " " + strings.Join(call.Run.Argv, " "))
		for k, v := range call.Run.Env {
			blob.WriteString(" " + k + "=" + v)
		}
	}
	blob.WriteString(stderr.String())
	if strings.Contains(blob.String(), inferencetest.SignIn) {
		t.Fatal("a docker call or the run's output carries the registry sign-in")
	}
	for _, dir := range []string{p.OutDir, p.RunRoot} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's dirs
			if bytes.Contains(b, []byte(inferencetest.SignIn)) {
				t.Errorf("%s holds the registry sign-in", path)
			}
			if bytes.Contains(b, []byte(inferencetest.TokenFor(1))) {
				t.Errorf("%s holds the session token", path)
			}
			return nil
		})
	}
}

// The session is renewed at its interval while the run lives, and never
// after the run has stopped renewing and ended it.
func TestACreditsSessionIsRenewedAtItsIntervalAndNotAfterItsEnd(t *testing.T) {
	reg := inferencetest.New(t)
	c := inference.New(reg.URL, func() (string, error) { return inferencetest.SignIn, nil }, nil)
	o, err := c.Open(context.Background(), inference.PurposeRun)
	if err != nil {
		t.Fatal(err)
	}
	g := &InferenceGrant{Session: o.Session.ID, Token: o.Token, RenewEvery: 40 * time.Millisecond,
		Renew: func(ctx context.Context) error { _, err := c.Renew(ctx, o.Session.ID); return err },
		End: func(ctx context.Context) (receipt.Inference, error) {
			v, err := c.End(ctx, o.Session.ID)
			return InferenceReceipt(v, o.Model), err
		}}
	p := Params{Inference: g}
	stop := renewInference(context.Background(), p, nil)
	time.Sleep(230 * time.Millisecond)
	stop()
	env := &receipt.Envelope{}
	endInference(p, env, nil)
	renewals := reg.RenewedAt()
	if len(renewals) < 3 || len(renewals) > 6 {
		t.Fatalf("%d renewals in 230ms at 40ms, want about 5", len(renewals))
	}
	for i := 1; i < len(renewals); i++ {
		if gap := renewals[i].Sub(renewals[i-1]); gap < 30*time.Millisecond {
			t.Fatalf("renewals %v apart, sooner than the interval", gap)
		}
	}
	if env.Inference == nil || env.Inference.Session != o.Session.ID {
		t.Fatalf("envelope inference = %+v", env.Inference)
	}
	ended := reg.EndedAt(o.Session.ID)
	time.Sleep(120 * time.Millisecond)
	for _, at := range reg.RenewedAt() {
		if at.After(ended) {
			t.Fatal("a renewal arrived after the session was ended")
		}
	}
	if got := len(reg.Ended()); got != 1 {
		t.Fatalf("ended %d times, want once", got)
	}

	// A session whose end fails is said not to have ended and leaves the
	// envelope without an inference block; a grant with no End, or no
	// envelope to record in, is left alone.
	failing := Params{Inference: &InferenceGrant{End: func(context.Context) (receipt.Inference, error) {
		return receipt.Inference{Session: "s-1"}, errors.New("registry unreachable")
	}}}
	failed := &receipt.Envelope{}
	var said bytes.Buffer
	endInference(failing, failed, &said)
	if failed.Inference != nil || !strings.Contains(said.String(), "could not end the Agentry credits session (it lapses within the hour): registry unreachable") {
		t.Fatalf("a failed end: inference %+v, said %q", failed.Inference, said.String())
	}
	good := Params{Inference: &InferenceGrant{End: func(context.Context) (receipt.Inference, error) {
		return receipt.Inference{Session: "s-2", Credits: 3}, nil
	}}}
	endInference(good, nil, nil)
	kept := &receipt.Envelope{}
	endInference(good, kept, nil)
	if kept.Inference == nil || kept.Inference.Session != "s-2" || kept.Inference.Credits != 3 {
		t.Fatalf("a good end: %+v", kept.Inference)
	}
	none := &receipt.Envelope{}
	endInference(Params{Inference: &InferenceGrant{}}, none, nil)
	endInference(Params{}, none, nil)
	if none.Inference != nil {
		t.Fatalf("a grant with no End recorded %+v", none.Inference)
	}
}

// A renewal the registry answers 410 (the session ended) stops the renewer:
// it says so once and asks no more.
func TestARenewalOfAnEndedSessionStopsTheRenewer(t *testing.T) {
	reg := inferencetest.New(t)
	reg.EndRenewals = true
	c := inference.New(reg.URL, func() (string, error) { return inferencetest.SignIn, nil }, nil)
	calls := 0
	g := &InferenceGrant{Session: "s-x", Token: "t", RenewEvery: 20 * time.Millisecond,
		Renew: func(ctx context.Context) error { calls++; _, err := c.Renew(ctx, "s-x"); return err }}
	var stderr bytes.Buffer
	stop := renewInference(context.Background(), Params{Inference: g}, &stderr)
	time.Sleep(150 * time.Millisecond)
	stop()
	if calls != 1 {
		t.Fatalf("%d renewals after the registry said the session ended, want 1", calls)
	}
	if !strings.Contains(stderr.String(), "the Agentry credits session ended") {
		t.Fatalf("stderr %q", stderr.String())
	}

	// A renewal that fails is said once and asked again at the interval until
	// it works; a failure after a good renewal is said again.
	var asks atomic.Int32
	reached := make(chan struct{})
	flaky := &InferenceGrant{Session: "s-y", Token: "t", RenewEvery: time.Millisecond,
		Renew: func(context.Context) error {
			n := asks.Add(1)
			if n == 8 {
				close(reached)
			}
			// Two failures, a good renewal, three failures, a good renewal.
			if n == 3 || n >= 7 {
				return nil
			}
			return errors.New("registry unreachable " + strconv.Itoa(int(n)))
		}}
	stderr.Reset()
	stop = renewInference(context.Background(), Params{Inference: flaky}, &stderr)
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-reached:
	case <-wait.Done():
		t.Fatalf("the renewer asked %d times: a failed renewal was not asked again at the interval", asks.Load())
	}
	stop()
	said := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(said) != 2 ||
		!strings.Contains(said[0], "could not renew the Agentry credits session (trying again each minute): registry unreachable 1") ||
		!strings.Contains(said[1], "could not renew the Agentry credits session (trying again each minute): registry unreachable 4") {
		t.Fatalf("a failure is said once until a renewal works, got:\n%s", stderr.String())
	}

	// A grant with no Renew starts no renewer; one with no interval waits the
	// default one rather than asking at once.
	for range 50 {
		renewInference(context.Background(), Params{Inference: &InferenceGrant{RenewEvery: time.Nanosecond}}, nil)()
	}
	var early atomic.Int32
	for range 50 {
		renewInference(context.Background(), Params{Inference: &InferenceGrant{Renew: func(context.Context) error { early.Add(1); return nil }}}, nil)()
	}
	if n := early.Load(); n != 0 {
		t.Fatalf("a grant with no interval asked for a renewal at once, %d times", n)
	}
}

// A plan that pays with credits does not run without a session, and a
// session is never handed to a plan that spends a key.
func TestTheRunHoldsItsCreditsSessionAgainstItsPlan(t *testing.T) {
	p, f, _, _ := creditsFixture(t)
	happyFake(f)
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "was given no credits session") {
		t.Fatalf("want a policy refusal naming the missing session, got %v", err)
	}
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonSignInMissing {
		t.Fatalf("reason %v, want %s", r, receipt.ReasonSignInMissing)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "RunDetached") {
			t.Fatalf("a run with no session started a container: %v", f.Ops())
		}
	}

	// A session with no token is no session.
	p, f, _, _ = creditsFixture(t)
	happyFake(f)
	p.Inference = &InferenceGrant{Session: "s-1"}
	_, err = Run(context.Background(), f, p)
	if r := receipt.ReasonOf(err); !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "was given no credits session") || r == nil || r.Code != receipt.ReasonSignInMissing || len(f.Calls) != 0 {
		t.Fatalf("a session with no token: %v (%d docker calls)", err, len(f.Calls))
	}
	// A hosted run uses the platform's model, whatever session it is handed.
	p, f, _, _ = creditsFixture(t)
	happyFake(f)
	p.Inference = &InferenceGrant{Session: "s-1", Token: inferencetest.TokenFor(1)}
	p.Hosted = true
	_, err = Run(context.Background(), f, p)
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "a hosted run uses the platform's model and takes no Agentry credits session") || len(f.Calls) != 0 {
		t.Fatalf("a hosted run handed a session: %v (%d docker calls)", err, len(f.Calls))
	}

	kp, kf := fixture(t)
	happyFake(kf)
	kp.Inference = &InferenceGrant{Session: "s-1", Token: inferencetest.TokenFor(1)}
	if _, err := Run(context.Background(), kf, kp); err == nil || !strings.Contains(err.Error(), "compiled for a model key") {
		t.Fatalf("a key-paid plan handed a session: %v", err)
	}
}

// A hosted run is refused a credits session before the registry is asked.
func TestAHostedRunOpensNoCreditsSession(t *testing.T) {
	p, _, reg, c := creditsFixture(t)
	if _, err := InferenceFor(context.Background(), c, p.Plan, true, inference.PurposeRun, nil); err == nil || !strings.Contains(err.Error(), "hosted run") {
		t.Fatalf("hosted: %v", err)
	}
	if len(reg.Opened()) != 0 {
		t.Fatal("a hosted run asked the registry for a session")
	}
	if _, err := InferenceFor(context.Background(), nil, p.Plan, false, inference.PurposeRun, nil); err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("signed out: %v", err)
	}
	// A plan that spends a key asks the registry for nothing, hosted or not,
	// signed in or not.
	kp, _ := fixture(t)
	for _, hosted := range []bool{false, true} {
		if g, err := InferenceFor(context.Background(), nil, kp.Plan, hosted, inference.PurposeRun, nil); g != nil || err != nil {
			t.Fatalf("a key plan (hosted %v) was handed a credits session: %+v %v", hosted, g, err)
		}
	}
}

// A payer whose credits are used up is refused the session before the run
// starts, as out of credits; and the registry's refusal of a model call,
// as the harness quotes it, is out of credits too, named as Agentry's.
func TestUsedUpCreditsAreOutOfCredits(t *testing.T) {
	p, _, reg, c := creditsFixture(t)
	reg.NoCredits = true
	_, err := InferenceFor(context.Background(), c, p.Plan, false, inference.PurposeRun, nil)
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonOutOfCredits {
		t.Fatalf("open with no credits: %v (reason %v)", err, r)
	}
	quoted := `error: POST "http://127.0.0.1/api/v1/x/inference/v1/chat/completions": 402 {"error":{"message":"Your Agentry credits are used up: the balance is 0.","type":"insufficient_quota","code":"credits_exhausted"}}`
	if !adapter.OutOfCredits(quoted) {
		t.Fatal("the registry's refusal is not recognized as out of credits")
	}
	err = outOfCreditsError(Params{Inference: &InferenceGrant{}}, "")
	if !strings.Contains(err.Error(), "your Agentry credits are used up") || strings.Contains(err.Error(), "the provider said") {
		t.Fatalf("message %q", err)
	}
	// A run on the person's own account words the provider's refusal, and the
	// provider's words only when it gave some.
	plain := outOfCreditsError(Params{}, "")
	if !strings.Contains(plain.Error(), "the account that pays for them has no credits left") || strings.Contains(plain.Error(), "Agentry credits") ||
		strings.Contains(plain.Error(), "the provider said") {
		t.Fatalf("message %q", plain)
	}
	if said := outOfCreditsError(Params{}, "402 from upstream"); !strings.HasSuffix(said.Error(), "; the provider said: 402 from upstream") {
		t.Fatalf("message %q", said)
	}

	// The open's refusal names the fix; a registry that no longer knows the
	// sign-in is a missing sign-in, not a lack of credits.
	if err := creditsError(inference.ErrNoCredits); !strings.Contains(err.Error(), "add credits at the registry, or choose another model provider (agentry setup)") {
		t.Fatalf("no credits: %v", err)
	}
	stale := inference.New(reg.URL, func() (string, error) { return "agy_staleSignIn", nil }, nil)
	reg.NoCredits = false
	_, err = InferenceFor(context.Background(), stale, p.Plan, false, inference.PurposeRun, nil)
	var ce *ClassifiedError
	if r := receipt.ReasonOf(err); !errors.As(err, &ce) || ce.Code != ExitPolicy || r == nil || r.Code != receipt.ReasonSignInMissing {
		t.Fatalf("a signed-out registry: %v (reason %v)", err, r)
	}
}
