package localrun

import (
	"errors"
	"fmt"

	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"

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
	provider, model := runsOn(sm, view)
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

// runsOn is the provider and the model id a run on sm asks for, where view
// is the policy as sm reads it: a saved model of a policy that holds none
// as a list is read from the policy's model block.
func runsOn(sm policy.SavedModel, view *policy.Policy) (string, string) {
	if sm.Provider == "" {
		return view.Model.Provider, view.Model.Name
	}
	return sm.Provider, sm.Model
}

// RunsOn is the provider, by the managed settings' name for it, and the
// model id a run on sm asks for.
func RunsOn(pol *policy.Policy, sm policy.SavedModel) (string, string, error) {
	view, err := pol.ForRun(sm)
	if err != nil {
		return "", "", err
	}
	provider, model := runsOn(sm, view)
	return managed.RunModel{Provider: provider}.Vendor(), model, nil
}

// SavedModelFor is the saved model of pol that runs provider's model
// (provider by the managed settings' name for it), the default first when
// it is one of them, or false when none is.
func SavedModelFor(pol *policy.Policy, provider, model string) (policy.SavedModel, bool) {
	_, models, def := pol.SavedModels()
	var found []policy.SavedModel
	for _, sm := range models {
		if p, m, err := RunsOn(pol, sm); err == nil && p == provider && m == model {
			found = append(found, sm)
		}
	}
	for _, sm := range found {
		if sm.ID == def {
			return sm, true
		}
	}
	if len(found) == 0 {
		return policy.SavedModel{}, false
	}
	return found[0], true
}

// PresetModelConflict is a run naming a saved model other than the one a
// locked preset fixes for the QuickAgent. Each client says it in its own
// words for the choice the person made.
type PresetModelConflict struct {
	// Organization is the handle of the organization that set the preset.
	Organization string
	// Ref is the QuickAgent the preset is for (ns/name).
	Ref string
	// Want is the preset's model.
	Want managedsettings.Model
}

func (e *PresetModelConflict) Error() string {
	return fmt.Sprintf("%s's managed settings fix the model of %s at %s's %s (%s)", e.Organization, e.Ref, e.Want.Provider, e.Want.Model, managedsettings.QuickAgents)
}

// PresetModelMissing is a run of a QuickAgent whose locked preset fixes a
// model no saved model on this computer is. Each client adds its own way
// to save one.
type PresetModelMissing struct {
	Organization, Ref string
	Want              managedsettings.Model
}

func (e *PresetModelMissing) Error() string {
	return fmt.Sprintf("%s's managed settings run %s on %s's %s (%s), and no saved model on this computer is that model", e.Organization, e.Ref, e.Want.Provider, e.Want.Model, managedsettings.QuickAgents)
}

// PresetModel is the saved model a run of the QuickAgent ref (ns/name, any
// version) runs on when held gives it a preset model: named, the saved
// model the person chose (an id or a name, empty for none), is returned as
// it is unless a locked preset fixes another (a *PresetModelConflict);
// with none named, it is the saved model of pol that is the preset's
// model, the default first. A preset never lifts the managed model rows: a
// preset model they refuse is refused with their sentence (exit 11). With
// no saved model of the preset's model on this computer, a locked preset's
// run is refused (a *PresetModelMissing), and a default's stays on the person's
// own choice, with the warning returned to say so. Without a preset model
// the run's choice is named, unchanged.
func PresetModel(pol *policy.Policy, held managed.Effective, ref, named, adapterName string) (string, string, error) {
	r, err := manifest.ParseRef(ref)
	if err != nil {
		return named, "", nil
	}
	a, ok := held.AssignmentOf(r.Namespace + "/" + r.Name)
	if !ok || a.Preset == nil || a.Preset.Model == nil {
		return named, "", nil
	}
	want := *a.Preset.Model
	if named != "" {
		if !a.PresetLocked {
			return named, "", nil
		}
		sm, err := pol.RunModel(named, adapterName)
		if err != nil {
			// The run's own choice refuses a saved model it does not have.
			return named, "", nil
		}
		if provider, model, err := RunsOn(pol, sm); err == nil && provider == want.Provider && model == want.Model {
			return named, "", nil
		}
		return named, "", engine.Classify(engine.ExitPolicy, &PresetModelConflict{Organization: a.Organization, Ref: a.Ref, Want: want})
	}
	if reason := held.ModelRefusal(managed.RunModel{Provider: want.Provider, Model: want.Model}); reason != "" {
		return "", "", engine.Classify(engine.ExitPolicy, errors.New(reason))
	}
	sm, ok := SavedModelFor(pol, want.Provider, want.Model)
	if !ok {
		if a.PresetLocked {
			return "", "", engine.Classify(engine.ExitPolicy, &PresetModelMissing{Organization: a.Organization, Ref: a.Ref, Want: want})
		}
		return "", fmt.Sprintf("%s's managed settings suggest %s's %s for %s, and no saved model on this computer is that model; the run is on your own choice of model",
			a.Organization, want.Provider, want.Model, a.Ref), nil
	}
	if sm.ID != "" {
		return sm.ID, "", nil
	}
	return sm.Name, "", nil
}
