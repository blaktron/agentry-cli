package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// InferenceGrant is the Agentry credits session a run pays for its model
// with (model provider agentry): the session's token, which the harness
// presents to the registry's model endpoint as its key, and how the
// session is kept alive and ended. The person's registry sign-in is never
// part of it.
type InferenceGrant struct {
	Session string
	Token   string
	// Model is the model the registry forwards to, for the receipt.
	Model      string
	RenewEvery time.Duration
	// Renew, when set, is called every RenewEvery for as long as the run
	// lives. Nil leaves renewing to the caller (a persistent agent's
	// supervisor, whose session outlives each start of its harness).
	Renew func(ctx context.Context) error
	// End, when set, ends the session once the run's containers are gone
	// and answers its final view for the receipt. Nil leaves ending to the
	// caller.
	End func(ctx context.Context) (receipt.Inference, error)
}

// InferenceFor opens the credits session a plan that pays with Agentry
// credits needs, for Params.Inference: nil when the plan spends a key or a
// sign-in. A hosted run is refused before anything is asked: its model is
// the platform's. Every local client calls it: `agentry run`, a built-in's
// run (localrun) and the desktop's run path.
func InferenceFor(ctx context.Context, c *inference.Client, plan *spec.Plan, hosted bool, purpose string, stderr io.Writer) (*InferenceGrant, error) {
	if !plan.PaysWithCredits() {
		return nil, nil
	}
	if hosted {
		return nil, Classify(ExitPolicy, errors.New("model provider agentry pays for a run on this computer with your credits; a hosted run uses the platform's model and takes no such provider"))
	}
	if c == nil {
		return nil, Classify(ExitPolicy, receipt.WithReason(errors.New("model provider agentry pays with the credits of a SecureAgentry sign-in, and this computer is not signed in to the registry its policy names: agentry login"),
			receipt.ReasonSignInMissing, ""))
	}
	o, err := c.Open(ctx, purpose)
	if err != nil {
		return nil, creditsError(err)
	}
	g := &InferenceGrant{Session: o.Session.ID, Token: o.Token, Model: o.Model, RenewEvery: o.RenewEvery}
	g.Renew = func(ctx context.Context) error {
		_, err := c.Renew(ctx, o.Session.ID)
		return err
	}
	g.End = func(ctx context.Context) (receipt.Inference, error) {
		v, err := c.End(ctx, o.Session.ID)
		if err != nil {
			return receipt.Inference{}, err
		}
		return InferenceReceipt(v, o.Model), nil
	}
	if stderr != nil {
		_, _ = fmt.Fprintf(stderr, "agentry: paying for the model with Agentry credits at %s (%s)\n", c.Registry(), o.Model)
	}
	return g, nil
}

// InferenceReceipt is a session's final view as the receipt records it.
func InferenceReceipt(v inference.View, model string) receipt.Inference {
	return receipt.Inference{Session: v.ID, Model: model, Requests: v.Requests,
		InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, Credits: v.Credits}
}

// creditsError classifies a registry's refusal to open or renew a session.
func creditsError(err error) error {
	switch {
	case errors.Is(err, inference.ErrNoCredits):
		return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("%w: add credits at the registry, or choose another model provider (agentry setup)", err), receipt.ReasonOutOfCredits, ""))
	case errors.Is(err, inference.ErrSignedOut):
		return Classify(ExitPolicy, receipt.WithReason(err, receipt.ReasonSignInMissing, ""))
	}
	return Classify(ExitPolicy, err)
}

// checkInference holds the run's credits session against its plan: a plan
// that pays with credits runs only with a session's token, and a session
// handed to a plan that spends a key is refused, as a sign-in is.
func checkInference(p Params) error {
	pays, g := p.Plan.PaysWithCredits(), p.Inference
	switch {
	case !pays && g == nil:
		return nil
	case !pays:
		return Classify(ExitPolicy, errors.New("this run was compiled for a model key and was handed an Agentry credits session; recompile under a policy that names model provider agentry"))
	case g == nil || g.Token == "":
		return Classify(ExitPolicy, receipt.WithReason(errors.New("this run pays for its model with Agentry credits (model provider agentry) and was given no credits session: sign in (agentry login) and run it again"),
			receipt.ReasonSignInMissing, ""))
	case p.Hosted:
		return Classify(ExitPolicy, errors.New("a hosted run uses the platform's model and takes no Agentry credits session"))
	}
	return nil
}

// inferenceToken is the value of an EnvVar.FromInference: the session's
// token, empty when the run has none (checkInference refuses that first).
func inferenceToken(p Params) string {
	if p.Inference == nil {
		return ""
	}
	return p.Inference.Token
}

// inferenceSecrets are the values the run's redactor masks for its credits
// session.
func inferenceSecrets(p Params) []string {
	if t := inferenceToken(p); t != "" {
		return []string{t}
	}
	return nil
}

// renewInference renews the run's credits session every RenewEvery for as
// long as the run lives; stop ends the loop. A failed renewal is said once
// and tried again a minute later, until the registry says the session has
// ended. The token's value never changes, so nothing in the run restarts
// for it.
func renewInference(ctx context.Context, p Params, stderr io.Writer) (stop func()) {
	g := p.Inference
	if g == nil || g.Renew == nil {
		return func() {}
	}
	every := g.RenewEvery
	if every <= 0 {
		every = inference.DefaultRenewEvery
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		said := false
		wait := every
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			err := g.Renew(ctx)
			switch {
			case err == nil:
				said, wait = false, every
			case ctx.Err() != nil:
				return
			case errors.Is(err, inference.ErrEnded):
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr, "agentry: the Agentry credits session ended, so the model's next calls are refused: %v\n", err)
				}
				return
			default:
				if !said && stderr != nil {
					_, _ = fmt.Fprintf(stderr, "agentry: could not renew the Agentry credits session (trying again each minute): %v\n", err)
				}
				said, wait = true, min(time.Minute, every)
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// endInference ends the run's credits session and records its final view
// on the envelope. Called once the run's containers are gone, so every
// token the harness spent is in it; a session whose end is not answered
// lapses at the registry within the hour, and the receipt carries no
// inference block.
func endInference(p Params, env *receipt.Envelope, stderr io.Writer) {
	g := p.Inference
	if g == nil || g.End == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := g.End(ctx)
	if err != nil {
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "agentry: could not end the Agentry credits session (it lapses within the hour): %v\n", err)
		}
		return
	}
	if env != nil {
		env.Inference = &v
	}
}
