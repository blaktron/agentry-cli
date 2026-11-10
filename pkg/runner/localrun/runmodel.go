package localrun

import (
	"errors"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// RunModel is the saved model a run is on, the policy it compiles from
// and the harness it runs on.
type RunModel struct {
	// Policy is the policy as the saved model reads (policy.ForRun): its
	// provider's credential, endpoint and allowlist, its model id and
	// context window.
	Policy *policy.Policy
	// Saved is the saved model; empty for a policy with none.
	Saved policy.SavedModel
	// Adapter is the harness: the one the run named, else the harness
	// block's the saved model was read from, else the one its credential
	// takes (adapter.DefaultFor).
	Adapter string
}

// ChooseRunModel is the saved model a run naming ref (by name or id, empty
// for the default) on the harness adapterName (empty for the one the
// saved model takes) runs on. A ref that names no saved model is refused
// (exit 11): a run never falls back to the default.
func ChooseRunModel(pol *policy.Policy, ref, adapterName string) (RunModel, error) {
	sm, err := pol.RunModel(ref, adapterName)
	if err != nil {
		return RunModel{}, engine.Classify(engine.ExitPolicy, err)
	}
	view, err := pol.ForRun(sm)
	if err != nil {
		return RunModel{}, engine.Classify(engine.ExitPolicy, err)
	}
	name := adapterName
	if name == "" {
		name = pol.RunHarness(sm)
	}
	if name == "" {
		name = adapter.DefaultFor(view)
	}
	// An organization's managed settings hold the model, what pays for it,
	// the member's own subscriptions and endpoints, and the coding tools.
	provider, model := sm.Provider, sm.Model
	if provider == "" {
		provider, model = view.Model.Provider, view.Model.Name
	}
	if r := managed.ModelRefusal(managed.RunModel{Provider: provider, Model: model, KeyKind: view.Model.APIKeyKind,
		BaseURL: view.Model.BaseURL, Adapter: name}); r != "" {
		return RunModel{}, engine.Classify(engine.ExitPolicy, errors.New(r))
	}
	return RunModel{Policy: view, Saved: sm, Adapter: name}, nil
}

// IsMissingModel reports whether err is a run naming a saved model this
// computer does not have.
func IsMissingModel(err error) bool {
	var m *policy.MissingModelError
	return errors.As(err, &m)
}
