package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// ModelSignInFor is the model sign-in a plan spends, from the person's
// store, for Params.ModelSignIn and Params.RenewModelSignIn: nil when the
// plan spends a key. A hosted run is refused before the store is read
// (I5): a sign-in is the person's own, local only, permanently (D2). No
// sign-in of the plan's kind is the receipt reason signin-missing.
//
// Every local client calls it: `agentry run`, a built-in's run
// (localrun), and the desktop's run path (agentry-cli#845).
func ModelSignInFor(ctx context.Context, store *modelsignin.Store, plan *spec.Plan, hosted bool) (*ModelSignInGrant, func(context.Context) (ConnectionToken, error), error) {
	msi := plan.Harness.ModelSignIn
	if msi == nil {
		return nil, nil, nil
	}
	if hosted {
		return nil, nil, HostedSignInRefusal(msi.Kind)
	}
	g, err := store.ForRun(ctx, msi.Kind, msi.Delivery, plan.Harness.Host != nil)
	if errors.Is(err, modelsignin.ErrNoSignIn) {
		return nil, nil, Classify(ExitPolicy, receipt.WithReason(err, receipt.ReasonSignInMissing, ""))
	}
	if err != nil {
		return nil, nil, Classify(ExitPolicy, err)
	}
	grant := &ModelSignInGrant{Kind: g.Kind, ConfigDir: g.ConfigDir,
		Token: ConnectionToken{AccessToken: g.Access.Token, ExpiresAt: g.Access.ExpiresAt}}
	var renew func(context.Context) (ConnectionToken, error)
	if g.Renew != nil {
		renew = func(ctx context.Context) (ConnectionToken, error) {
			a, err := g.Renew(ctx)
			return ConnectionToken{AccessToken: a.Token, ExpiresAt: a.ExpiresAt}, err
		}
	}
	return grant, renew, nil
}

// HostedSignInRefusal is the refusal of a model sign-in for a hosted run.
func HostedSignInRefusal(kind string) error {
	return Classify(ExitPolicy, fmt.Errorf("a hosted run never spends a model sign-in (%s, policy apiKeyKind %s): hosted runs use Agentry's own model access", policy.SignInSentence(kind), kind))
}
