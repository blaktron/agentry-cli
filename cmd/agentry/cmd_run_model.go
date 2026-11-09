package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// refuseModelElsewhere refuses --model where this computer's saved models
// do not apply: a hosted run pays with Agentry's own model access, and a
// remote runhost runs on the model it keeps.
func refuseModelElsewhere(opts runOpts) error {
	switch {
	case opts.on != "" || opts.installed:
		return engine.Classify(engine.ExitUsage, errors.New("--model chooses among this computer's saved models, and a run on a remote runhost uses the model the runhost keeps (agentry runhost model)"))
	case hostedFor(opts):
		return engine.Classify(engine.ExitUsage, errors.New("--model chooses among this computer's saved models, and a hosted run uses Agentry's own model access"))
	}
	return nil
}

// signInKept reports whether a model sign-in of kind is kept on this
// computer; a variable so a test can answer for the store.
var signInKept = func(home, kind string) bool {
	_, err := modelsignin.Open(home, connectionKeys()).Get(modelsignin.VendorOf(kind))
	return !errors.Is(err, modelsignin.ErrNone)
}

// keyKept is modelkey.Present; a variable so a test can answer for the
// keychain.
var keyKept = modelkey.Present

// savedModelUsable refuses a run whose saved model's credential is not on
// this computer (exit 11), naming the saved model, its provider and the
// fix, before anything is laid down for it; other is the fix of running on
// another model instead. What it cannot tell from here (a sign-in that no
// longer renews, a key the provider revoked) the run refuses when it meets
// it.
func savedModelUsable(home string, rm localrun.RunModel, getenv func(string) string, other string) error {
	m := rm.Policy.Model
	if m.Provider == "" {
		return nil
	}
	hp := rm.Policy.Harness[rm.Adapter]
	what := fmt.Sprintf("the saved model %q (provider %s)", savedModelName(rm), m.KeyProvider())
	if rm.Saved.ID == "" {
		what = fmt.Sprintf("the model (provider %s)", m.KeyProvider())
	}
	kind := policy.KeyKind(hp, m)
	if policy.IsModelSignIn(kind) {
		if !signInKept(home, kind) {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s cannot be used: it spends %s, and none is kept on this computer: sign in with agentry model-signins add %s, or %s",
				what, policy.SignInSentence(kind), modelsignin.VendorOf(kind), other))
		}
		return nil
	}
	env := policy.KeyEnv(hp, m)
	if env == "" || strings.TrimSpace(getenv(env)) != "" {
		return nil
	}
	if policy.KeySource(hp, m) != modelkey.SourceKeyring {
		fix := "set it, or " + other
		if m.Provider == policy.ProviderCustom && hp.APIKeyEnv == "" {
			fix += "; if the endpoint takes no key, delete the provider's apiKeyEnv from the policy"
		}
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s cannot be used: its key comes from $%s, which is not set: %s", what, env, fix))
	}
	ok, err := keyKept(m.KeyProvider())
	if err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s: the keychain: %w", what, err))
	}
	if !ok {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s cannot be used: its key is neither in $%s nor in the keychain (account %s): set $%s, keep the key again (agentry providers remove %s, then agentry providers add), or %s",
			what, env, modelkey.Account(m.KeyProvider()), env, m.KeyProvider(), other))
	}
	return nil
}

// runOnAnother is a run's fix of running on another saved model.
const runOnAnother = "run on another saved model (--model; agentry models list)"

// savedModelName is the saved model's name as a person knows it.
func savedModelName(rm localrun.RunModel) string {
	if rm.Saved.Name != "" {
		return rm.Saved.Name
	}
	return rm.Saved.ID
}
