package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ModelKind is what pays for m on a runhost: ModelKindCredits,
// ModelKindSignIn, ModelKindNoKey or ModelKindKey.
func ModelKind(m policy.Model) string {
	switch {
	case m.Provider == policy.ProviderAgentry:
		return ModelKindCredits
	case policy.IsModelSignIn(m.APIKeyKind):
		return ModelKindSignIn
	case policy.TakesNoKey(policy.HarnessPolicy{}, m):
		return ModelKindNoKey
	}
	return ModelKindKey
}

// SubscriptionRefusal is the refusal of a saved model on a pasted Claude
// subscription token, which a runhost cannot run.
const SubscriptionRefusal = "a pasted Claude subscription token runs only the installed claude, which a runhost does not run: save a model on your Claude subscription through Claude Code's own sign-in (agentry providers add anthropic --sign-in) and make that one available"

// Prepared is one of a device's saved models as a runhost is sent it.
type Prepared struct {
	// Saved is the device's saved model.
	Saved policy.SavedModel
	// Doc is what PutSavedModel sends, without the key: a caller reads the
	// key on the device (modelkey.Personal on Model) once the person has
	// agreed, and sets Doc.Key, and sets Doc.Yes once they have agreed to a
	// Claude subscription's one message.
	Doc PutModel
	// Kind is the relay its credential takes: a key sent to the store, a
	// sign-in made on the runhost, a credits token minted for it, or an
	// address alone.
	Kind string
	// Model is the saved model as it reads: where its key is read from,
	// its sign-in's kind, its registry for Agentry credits.
	Model policy.Model
}

// Prepare is the device's saved model ref (a name or an id) in pol as a
// runhost is sent it. What a runhost cannot take is refused before anything
// is sent: a pasted Claude subscription token, a key variable a runhost does
// not take, and a connected account's token that renewedHere says only this
// device renews.
func Prepare(pol *policy.Policy, ref string, renewedHere func(vendor string) bool) (Prepared, error) {
	sm, ok := pol.FindModel(ref)
	if !ok {
		return Prepared{}, engine.Classify(engine.ExitUsage, fmt.Errorf("no saved model %q on this computer (agentry models list)", ref))
	}
	view, err := pol.ForRun(sm)
	if err != nil {
		return Prepared{}, engine.Classify(engine.ExitPolicy, err)
	}
	m := view.Model
	if m.APIKeyKind == policy.KeyKindSubscription {
		return Prepared{}, engine.Classify(engine.ExitPolicy, errors.New(SubscriptionRefusal))
	}
	kind := ModelKind(m)
	what := fmt.Sprintf("%q (%s/%s)", sm.Name, sm.Provider, sm.Model)
	if kind == ModelKindKey {
		if !job.ValidKeyEnv(m.APIKeyEnv) {
			return Prepared{}, engine.Classify(engine.ExitPolicy, fmt.Errorf("the saved model %s names its key's variable $%s, which a runhost does not take: name it as the provider does, ending in _API_KEY, _KEY or _TOKEN", what, m.APIKeyEnv))
		}
		if vendor := modelsignin.ConnectedVendor(sm.Provider); vendor != "" && renewedHere != nil && renewedHere(vendor) {
			return Prepared{}, engine.Classify(engine.ExitPolicy, fmt.Errorf("the %s token Agentry connected for %s is renewed on this computer at each run's start, and a runhost could not renew its copy: paste a %s token that does not expire (agentry providers add %s), then make it available", modelsignin.ConnectName(vendor), what, modelsignin.ConnectName(vendor), sm.Provider))
		}
	}
	providers, _, _ := pol.SavedModels()
	pr := providers[sm.Provider]
	doc := PutModel{ID: sm.ID, Name: sm.Name, ProviderID: sm.Provider, Model: sm.Model, ContextWindow: sm.ContextWindow,
		Provider: ModelProvider{Name: pr.Name, APIKeyEnv: m.APIKeyEnv, APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ProxyAllow: m.ProxyAllow}}
	if doc.Provider.ProxyAllow == nil {
		doc.Provider.ProxyAllow = []string{}
	}
	return Prepared{Saved: sm, Doc: doc, Kind: kind, Model: m}, nil
}

// KeepsSignIn says whether the runhost c reaches keeps a working sign-in to
// vendor, so a saved model on it needs no new one.
func KeepsSignIn(ctx context.Context, c *runhost.Client, vendor string) bool {
	list, err := ListStore(ctx, c)
	if err != nil {
		return false
	}
	for _, k := range list.SignIns {
		if k.Vendor == vendor && k.Broken == "" {
			return true
		}
	}
	return false
}

// KeepsCredits says whether the runhost c reaches keeps a credits token for
// the registry reg, so a saved model on Agentry credits needs no new one.
func KeepsCredits(ctx context.Context, c *runhost.Client, reg string) bool {
	list, err := ListStore(ctx, c)
	return err == nil && list.Credits != nil && strings.TrimRight(list.Credits.Registry, "/") == strings.TrimRight(reg, "/")
}
