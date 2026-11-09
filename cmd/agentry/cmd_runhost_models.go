package main

// A runhost's saved models, on the runhost: its own policy holds them as a
// computer's does (providers, saved models and a default), copied from a
// paired device's list one at a time. Each one's credential is kept here
// by the relay its kind takes, and each is checked here before it is kept,
// so a model the runhost cannot reach is refused there and then. The
// runhost's persistent agents and schedules name one of them or follow its
// default; one that something names is not taken away until it is moved.
//
// The store verb answers:
//
//	store models               the saved models, never a credential
//	store put-saved-model      one made available, a remote.PutModel as input
//	store remove-saved-model   one taken away, a remote.RemoveModel as input
//	store default-model        the default, a remote.RemoveModel's ref as input

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelcheck"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// runhostModelsVerb answers the store verb's saved model subcommands; ok is
// false for any other.
func runhostModelsVerb(ctx context.Context, sub string, in io.Reader, fw *runhost.FrameWriter) (code int, err error, ok bool) {
	switch sub {
	case "models":
		pol, _, err := runhostModelsPolicy()
		if err != nil {
			return engine.ExitPolicy, err, true
		}
		return 0, fw.Result(runhostListModels(pol)), true
	case "put-saved-model":
		code, err := runhostPutSavedModel(ctx, in, fw)
		return code, err, true
	case "remove-saved-model":
		code, err := runhostRemoveSavedModel(ctx, in, fw)
		return code, err, true
	case "default-model":
		code, err := runhostDefaultModel(in, fw)
		return code, err, true
	}
	return 0, nil, false
}

// runhostPolicyPath is the runhost's own policy file; a variable, so a test
// keeps the runhost's apart from the device's in one process.
var runhostPolicyPath = policy.DefaultPath

// runhostModelsPolicy is the runhost's own policy and its path.
func runhostModelsPolicy() (*policy.Policy, string, error) {
	path, err := runhostPolicyPath()
	if err != nil {
		return nil, "", err
	}
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		return nil, "", err
	}
	return pol, path, nil
}

// runhostHoldsModels says whether the runhost's policy holds saved models a
// device made available: its lists, or a version 1 policy's model block
// whose credential the runhost keeps (an older device's `runhost model
// --provider`). The model block setup writes, which nothing pays for, is
// no saved model of anyone's.
func runhostHoldsModels(pol *policy.Policy) bool {
	if pol.HasSavedModels() {
		return true
	}
	sm := pol.DefaultModel()
	if sm.ID == "" {
		return false
	}
	view, err := pol.ForRun(sm)
	return err == nil && runhostModelRefusal(view.Model) == ""
}

// runhostModelKind is what pays for m on the runhost.
func runhostModelKind(m policy.Model) string {
	switch {
	case m.Provider == policy.ProviderAgentry:
		return remote.ModelKindCredits
	case policy.IsModelSignIn(m.APIKeyKind):
		return remote.ModelKindSignIn
	case policy.TakesNoKey(policy.HarnessPolicy{}, m):
		return remote.ModelKindNoKey
	}
	return remote.ModelKindKey
}

// runhostModelRefusal says why a run on m would be refused on this runhost
// now: its credits token, its sign-in or its key is not here. "" when it can
// be used.
func runhostModelRefusal(m policy.Model) string {
	switch runhostModelKind(m) {
	case remote.ModelKindCredits:
		if _, err := runhostJobCredits(m); err != nil {
			return err.Error()
		}
	case remote.ModelKindSignIn:
		if err := runhostJobSignIn(m.APIKeyKind); err != nil {
			return err.Error()
		}
	case remote.ModelKindKey:
		if m.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(m.APIKeyEnv)) != "" {
			return ""
		}
		if m.APIKeySource != modelkey.SourceKeyring {
			return fmt.Sprintf("its key comes from $%s, which this runhost does not set", m.APIKeyEnv)
		}
		ok, err := modelkey.Present(m.KeyProvider())
		switch {
		case err != nil:
			return "its key could not be looked for in this runhost's store: " + err.Error()
		case !ok:
			return "its key is not in this runhost's store"
		}
	}
	return ""
}

// runhostListModels is the runhost's saved models as a device is told them.
func runhostListModels(pol *policy.Policy) remote.Models {
	out := remote.Models{Models: []remote.SavedModel{}, Max: policy.MaxSavedModels}
	if !runhostHoldsModels(pol) {
		return out
	}
	_, models, def := pol.SavedModels()
	out.Default = def
	users := runhostModelUsers(models)
	for _, sm := range models {
		out.Models = append(out.Models, runhostSavedModelOut(pol, sm, def, users[sm.ID]))
	}
	return out
}

func runhostSavedModelOut(pol *policy.Policy, sm policy.SavedModel, def string, users []remote.ModelUser) remote.SavedModel {
	o := remote.SavedModel{ID: sm.ID, Name: sm.Name, Provider: sm.Provider, Model: sm.Model, ContextWindow: sm.ContextWindow, Default: sm.ID == def, Users: users}
	view, err := pol.ForRun(sm)
	if err != nil {
		o.Refusal = err.Error()
		return o
	}
	o.Kind = runhostModelKind(view.Model)
	o.Refusal = runhostModelRefusal(view.Model)
	return o
}

// runhostModelUsers are the runhost's persistent agents and schedules that
// name each saved model, by its id: never those that follow the default.
func runhostModelUsers(models []policy.SavedModel) map[string][]remote.ModelUser {
	out := map[string][]remote.ModelUser{}
	ids := map[string]bool{}
	for _, sm := range models {
		ids[sm.ID] = true
	}
	if home, err := runnerHome(); err == nil {
		for _, a := range persistent.AgentsIn(home) {
			if a.DesktopOnly {
				continue
			}
			cfg, err := persistent.LoadConfig(home, a.Name)
			if err != nil || !ids[cfg.Model] {
				continue
			}
			out[cfg.Model] = append(out[cfg.Model], remote.ModelUser{Kind: "agent", ID: a.Name, Name: a.DisplayName(cfg)})
		}
	}
	for _, sc := range runhostScheduleModels() {
		if ids[sc.model] {
			out[sc.model] = append(out[sc.model], remote.ModelUser{Kind: "schedule", ID: sc.id, Name: sc.name})
		}
	}
	return out
}

// runhostDefaultFollowers are the runhost's persistent agents and schedules
// that follow its default.
func runhostDefaultFollowers() []remote.ModelUser {
	var out []remote.ModelUser
	if home, err := runnerHome(); err == nil {
		for _, a := range persistent.AgentsIn(home) {
			if a.DesktopOnly {
				continue
			}
			if cfg, err := persistent.LoadConfig(home, a.Name); err == nil && cfg.Model == "" {
				out = append(out, remote.ModelUser{Kind: "agent", ID: a.Name, Name: a.DisplayName(cfg)})
			}
		}
	}
	for _, sc := range runhostScheduleModels() {
		if sc.model == job.RunhostDefault {
			out = append(out, remote.ModelUser{Kind: "schedule", ID: sc.id, Name: sc.name})
		}
	}
	return out
}

// runhostScheduleModel is a runhost schedule and the saved model it names
// (RunhostDefault for the default; "" for one that carries a device's).
type runhostScheduleModel struct{ id, name, model string }

func runhostScheduleModels() []runhostScheduleModel {
	list, _, err := runhostScheduleStore().List()
	if err != nil {
		return nil
	}
	var out []runhostScheduleModel
	for _, sc := range list {
		if sm, err := readScheduleMeta(sc.ID); err == nil && sm.Model != "" {
			out = append(out, runhostScheduleModel{id: sc.ID, name: sc.Name, model: sm.Model})
		}
	}
	return out
}

// decodeRunhostDoc reads one JSON document of the device's, refusing a
// field it does not know.
func decodeRunhostDoc(in io.Reader, into any) error {
	dec := json.NewDecoder(io.LimitReader(in, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// singleLine says whether v is one line with no NUL and no space at either end.
func singleLine(v string) bool {
	return !strings.ContainsAny(v, "\r\n\x00") && strings.TrimSpace(v) == v
}

// runhostPutSavedModel answers put-saved-model: the saved model is kept in
// the runhost's policy, its key (when it has one) in the runhost's store,
// once the check on the runhost passes. Its sign-in or credits token must
// already be here: the device relays it first.
func runhostPutSavedModel(ctx context.Context, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	var doc remote.PutModel
	if err := decodeRunhostDoc(in, &doc); err != nil {
		return engine.ExitUsage, fmt.Errorf("the saved model: %w", err)
	}
	pr := policy.Provider{Name: doc.Provider.Name, APIKeyEnv: doc.Provider.APIKeyEnv, APIKeyKind: doc.Provider.APIKeyKind,
		BaseURL: doc.Provider.BaseURL, ProxyAllow: doc.Provider.ProxyAllow}
	view := policy.Model{Provider: policy.ProviderVendor(doc.ProviderID), ProviderID: doc.ProviderID, Name: doc.Model,
		APIKeyEnv: pr.APIKeyEnv, APIKeyKind: pr.APIKeyKind, BaseURL: pr.BaseURL, ProxyAllow: pr.ProxyAllow}
	kind := runhostModelKind(view)
	switch {
	case !singleLine(doc.Name) || !singleLine(doc.Model) || !singleLine(doc.Provider.Name) || !singleLine(doc.Provider.BaseURL):
		return engine.ExitUsage, errors.New("the saved model's name, model id and address are each one line")
	case doc.ID != "" && !policy.IsModelID(doc.ID):
		return engine.ExitUsage, fmt.Errorf("%q is not a saved model's id", doc.ID)
	case kind == remote.ModelKindKey && (!job.ValidKeyEnv(pr.APIKeyEnv) || doc.Key == "" || !job.SecretValueOK(doc.Key) || strings.ContainsAny(doc.Key, " \t")):
		return engine.ExitUsage, errors.New("a saved model paid by a key comes with one key, one line, and a variable named as a provider names its key")
	case kind != remote.ModelKindKey && doc.Key != "":
		return engine.ExitUsage, errors.New("only a saved model paid by a key is sent one")
	case kind == remote.ModelKindSignIn && policy.SignInProvider(pr.APIKeyKind) != view.Provider:
		return engine.ExitUsage, fmt.Errorf("a %s model's provider is %s, not %q", pr.APIKeyKind, policy.SignInProvider(pr.APIKeyKind), doc.ProviderID)
	case pr.APIKeyKind == policy.KeyKindSubscription:
		return engine.ExitUsage, errors.New("a pasted Claude subscription token runs only the installed claude, which a runhost does not run: make a saved model on your Claude subscription through Claude Code's own sign-in available instead")
	}
	if kind == remote.ModelKindKey {
		pr.APIKeySource = modelkey.SourceKeyring
	}
	pol, path, err := runhostModelsPolicy()
	if err != nil {
		return engine.ExitPolicy, err
	}
	if !runhostHoldsModels(pol) {
		// The model block setup wrote is no saved model of anyone's: the
		// first one made available takes its place, as the default.
		pol.ResetModels(policy.Model{})
	}
	before, _, _ := pol.SavedModels()
	sm, err := pol.PutModel(doc.ProviderID, pr, policy.SavedModel{ID: doc.ID, Name: doc.Name, Model: doc.Model, ContextWindow: doc.ContextWindow})
	if err != nil {
		return engine.ExitUsage, fmt.Errorf("this runhost: %w", err)
	}
	// The key goes into the store before the check, which reads it there
	// as a run will; a check that refuses puts back what was there.
	restore := func() {}
	if kind == remote.ModelKindKey {
		old, had := "", false
		if ok, _ := modelkey.Present(doc.ProviderID); ok {
			if v, err := modelkey.Lookup(doc.ProviderID); err == nil {
				old, had = v, true
			}
		}
		if err := modelkey.Store(doc.ProviderID, doc.Key); err != nil {
			return engine.ExitCLI, fmt.Errorf("keep the key in this runhost's store: %w", err)
		}
		restore = func() {
			if had {
				_ = modelkey.Store(doc.ProviderID, old)
			} else {
				_ = modelkey.Delete(doc.ProviderID)
			}
		}
	}
	checked, err := runhostCheckModel(ctx, pol, path, sm, doc.Yes, fw)
	if err != nil {
		restore()
		return engine.ExitPolicy, fmt.Errorf("the saved model %q was not kept on this runhost: %w", sm.Name, err)
	}
	if sm.ContextWindow == 0 && checked.ContextWindow > 0 {
		sm.ContextWindow = checked.ContextWindow
		if sm, err = pol.EditModel(sm.ID, sm); err != nil {
			restore()
			return engine.ExitUsage, err
		}
	}
	// A provider no saved model names any more (the one a model replaced
	// in place was on) goes, with its credential.
	for _, id := range pol.UnusedProviders() {
		_ = pol.DropProvider(id)
		if old, ok := before[id]; ok {
			_ = fw.Out("%s", forgetRunhostCredential(ctx, id, old, fw))
		}
	}
	if err := policy.Save(path, pol); err != nil {
		restore()
		return engine.ExitPolicy, err
	}
	_ = fw.Out("the saved model %q (%s/%s) is kept on this runhost", sm.Name, sm.Provider, sm.Model)
	list := runhostListModels(pol)
	kept, _ := list.Find(sm.ID)
	return 0, fw.Result(remote.PutAnswer{Model: kept, Checked: checked, Models: list})
}

// runhostModelCheckers are the save check's askers on the runhost: a
// computer's, except that Agentry credits are asked with the runhost's own
// token. A variable, so the tests ask no provider.
var runhostModelCheckers = func(cmd *cobra.Command, home string, pol *policy.Policy, path string) modelcheck.Checkers {
	c := modelCheckers(cmd, home, pol, path)
	c.Credits = func(ctx context.Context, m policy.Model) error {
		cl := runhostCreditsClient(m)
		if cl == nil {
			_, err := runhostJobCredits(m)
			return err
		}
		o, err := cl.Open(ctx, inference.PurposeRun)
		if err != nil {
			return err
		}
		_, _ = cl.End(context.WithoutCancel(ctx), o.Session.ID)
		return nil
	}
	return c
}

// runhostCheckModel checks sm, in pol at path, on this runhost, as a
// computer checks a save: its provider's own list, its plan's, Claude
// Code's one message (which yes agrees to), or a credits session.
func runhostCheckModel(ctx context.Context, pol *policy.Policy, path string, sm policy.SavedModel, yes bool, fw *runhost.FrameWriter) (remote.Checked, error) {
	view, err := pol.ModelView(sm)
	if err != nil {
		return remote.Checked{}, err
	}
	if view.APIKeyKind == policy.KeyKindClaudeSignIn && !yes {
		return remote.Checked{}, engine.Classify(engine.ExitUsage, fmt.Errorf("checking a Claude subscription's model sends it one short message, which counts toward the plan's usage: agree to it on the device (--yes) to check the model here"))
	}
	if refusal := runhostModelRefusal(view); refusal != "" && runhostModelKind(view) != remote.ModelKindKey {
		return remote.Checked{}, errors.New(refusal)
	}
	home, err := runnerHome()
	if err != nil {
		return remote.Checked{}, err
	}
	say := &lineWriter{say: func(t string) { _ = fw.Out("%s", t) }}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(say)
	cmd.SetErr(say)
	_ = fw.Out("checking %s on %s from this runhost…", sm.Model, sm.Provider)
	res, err := modelcheck.Check(ctx, runhostModelCheckers(cmd, home, pol, path), view)
	if err != nil {
		return remote.Checked{}, err
	}
	return remote.Checked{Resolved: res.Resolved, ContextWindow: res.ContextWindow, Warning: res.Warning}, nil
}

// forgetRunhostCredential removes provider id's credential from the
// runhost, as pr says what it is: its key from the store, its sign-in ended
// at the vendor, its credits token revoked. It says what it did.
func forgetRunhostCredential(ctx context.Context, id string, pr policy.Provider, fw *runhost.FrameWriter) string {
	m := policy.Model{Provider: policy.ProviderVendor(id), ProviderID: id, APIKeyEnv: pr.APIKeyEnv, APIKeySource: pr.APIKeySource, APIKeyKind: pr.APIKeyKind, BaseURL: pr.BaseURL}
	switch runhostModelKind(m) {
	case remote.ModelKindKey:
		if err := modelkey.Delete(id); err != nil {
			return fmt.Sprintf("the %s key was not removed from this runhost's store: %v", id, err)
		}
		return fmt.Sprintf("the %s key is removed from this runhost's store", id)
	case remote.ModelKindSignIn:
		s, err := runhostSignIns()
		if err != nil {
			return fmt.Sprintf("the %s sign-in was not ended: %v", id, err)
		}
		out, err := s.Remove(ctx, modelsignin.VendorOf(pr.APIKeyKind), &lineWriter{say: func(t string) { _ = fw.Out("%s", t) }})
		if err != nil {
			return fmt.Sprintf("the %s sign-in was not ended: %v", id, err)
		}
		if !out.Removed {
			return fmt.Sprintf("this runhost kept no %s sign-in", out.Vendor)
		}
		return fmt.Sprintf("the %s sign-in is ended: %s", out.Vendor, out.Outcome)
	case remote.ModelKindCredits:
		st, err := openRunhostStore()
		if err != nil {
			return fmt.Sprintf("the credits token was not revoked: %v", err)
		}
		out, err := revokeStoredCredits(ctx, st, "")
		switch {
		case err != nil:
			return fmt.Sprintf("the credits token was not removed: %v", err)
		case out.Credits == nil:
			return "this runhost kept no credits token"
		case !out.Revoked:
			return fmt.Sprintf("the credits token %q is removed from this runhost's store, but %s did not revoke it (%s): revoke it among your personal access tokens there", out.Credits.Name, out.Credits.Registry, out.RevokeError)
		}
		return fmt.Sprintf("the credits token %q is revoked and removed", out.Credits.Name)
	}
	return fmt.Sprintf("the %s endpoint took no key", id)
}

// runhostRemoveSavedModel answers remove-saved-model: refused while one of
// the runhost's agents or schedules names it, unless they move to another;
// the default's removal needs the new default; the last one's leaves the
// runhost holding none, refused while anything follows the default. Its
// provider's credential goes too when no other saved model uses it.
func runhostRemoveSavedModel(ctx context.Context, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	var req remote.RemoveModel
	if err := decodeRunhostDoc(in, &req); err != nil {
		return engine.ExitUsage, fmt.Errorf("the removal: %w", err)
	}
	pol, path, err := runhostModelsPolicy()
	if err != nil {
		return engine.ExitPolicy, err
	}
	list := runhostListModels(pol)
	held, ok := list.Find(req.Ref)
	if !ok {
		return engine.ExitUsage, fmt.Errorf("this runhost holds no saved model %q", req.Ref)
	}
	sm, _ := pol.FindModel(held.ID)
	providers, _, _ := pol.SavedModels()
	var to policy.SavedModel
	if req.MoveTo != "" {
		t, ok := list.Find(req.MoveTo)
		if !ok {
			return engine.ExitUsage, fmt.Errorf("--move-to: this runhost holds no saved model %q", req.MoveTo)
		}
		if t.ID == held.ID {
			return engine.ExitUsage, fmt.Errorf("--move-to: %q is the one removed", t.Name)
		}
		to, _ = pol.FindModel(t.ID)
	}
	users := held.Users
	if to.ID == "" && len(users) > 0 {
		return engine.ExitPolicy, fmt.Errorf("the saved model %q is named by %s on this runhost: move them first with --move-to <saved model>", held.Name, usersSaid(users))
	}
	if len(list.Models) == 1 {
		if followers := runhostDefaultFollowers(); len(followers) > 0 {
			return engine.ExitPolicy, fmt.Errorf("the saved model %q is this runhost's only one, and %s follow its default: make another available first (agentry runhost model %s --add <saved model>), or take them off", held.Name, usersSaid(followers), runhostOwnName())
		}
		pol.ResetModels(policy.Default().Model)
	} else if _, err := pol.RemoveModel(sm.ID, to.ID); err != nil {
		return engine.ExitUsage, fmt.Errorf("this runhost: %w", err)
	}
	moved, err := moveRunhostModelUsers(ctx, users, to)
	if err != nil {
		return engine.ExitCLI, err
	}
	out := remote.Removed{Removed: held, Moved: moved, MovedTo: to.Name, Credential: "kept"}
	if out.Moved == nil {
		out.Moved = []remote.ModelUser{}
	}
	if pol.HasSavedModels() {
		for _, id := range pol.UnusedProviders() {
			_ = pol.DropProvider(id)
		}
	}
	stillUsed := pol.HasSavedModels() && slices.ContainsFunc(pol.Models, func(m policy.SavedModel) bool { return m.Provider == sm.Provider })
	if err := policy.Save(path, pol); err != nil {
		return engine.ExitPolicy, err
	}
	if !stillUsed && !req.KeepCredential {
		out.Credential = forgetRunhostCredential(ctx, sm.Provider, providers[sm.Provider], fw)
	}
	_ = fw.Out("the saved model %q is off this runhost", held.Name)
	out.Models = runhostListModels(pol)
	return 0, fw.Result(out)
}

// usersSaid is a list of agents and schedules as a refusal names them.
func usersSaid(users []remote.ModelUser) string {
	lines := make([]string, 0, len(users))
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("%s %s (%s)", u.Kind, u.Name, u.ID))
	}
	return strings.Join(lines, ", ")
}

// moveRunhostModelUsers points each agent and schedule in users at the
// saved model to: an agent's configuration as `agentry agent configure
// --model` writes it, a schedule's kept job and its record.
func moveRunhostModelUsers(ctx context.Context, users []remote.ModelUser, to policy.SavedModel) ([]remote.ModelUser, error) {
	if to.ID == "" || len(users) == 0 {
		return nil, nil
	}
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	var agents []persistent.Agent
	var moved []remote.ModelUser
	for _, u := range users {
		switch u.Kind {
		case "agent":
			a, err := cliAgent(u.ID)
			if err != nil {
				return moved, err
			}
			agents = append(agents, a)
		case "schedule":
			if err := moveRunhostSchedule(u.ID, to.ID); err != nil {
				return moved, fmt.Errorf("move schedule %s to %s: %w", u.Name, to.Name, err)
			}
		}
		moved = append(moved, u)
	}
	return moved, moveAgents(ctx, home, agents, to)
}

// moveRunhostSchedule has schedule id run on the runhost's saved model to
// from its next occurrence.
func moveRunhostSchedule(id, to string) error {
	st, err := openRunhostStore()
	if err != nil {
		return err
	}
	b, err := st.Entry(scheduleEntryName(id))
	if err != nil {
		return err
	}
	var tmpl runhostScheduleTemplate
	if err := json.Unmarshal(b, &tmpl); err != nil {
		return err
	}
	tmpl.Job.Run.RunhostModel = to
	if b, err = json.Marshal(tmpl); err != nil {
		return err
	}
	if err := st.PutEntry(scheduleEntryName(id), b); err != nil {
		return err
	}
	meta, err := readScheduleMeta(id)
	if err != nil {
		return err
	}
	meta.Model = to
	return writeScheduleMeta(id, meta)
}

// runhostDefaultModel answers default-model.
func runhostDefaultModel(in io.Reader, fw *runhost.FrameWriter) (int, error) {
	var req remote.RemoveModel
	if err := decodeRunhostDoc(in, &req); err != nil {
		return engine.ExitUsage, fmt.Errorf("the default: %w", err)
	}
	pol, path, err := runhostModelsPolicy()
	if err != nil {
		return engine.ExitPolicy, err
	}
	held, ok := runhostListModels(pol).Find(req.Ref)
	if !ok {
		return engine.ExitUsage, fmt.Errorf("this runhost holds no saved model %q", req.Ref)
	}
	if _, err := pol.SetDefault(held.ID); err != nil {
		return engine.ExitUsage, err
	}
	if err := policy.Save(path, pol); err != nil {
		return engine.ExitPolicy, err
	}
	_ = fw.Out("the saved model %q is this runhost's default", held.Name)
	return 0, fw.Result(runhostListModels(pol))
}

// runhostJobModel puts the runhost's own saved model in place of the one a
// job names (job.Run.RunhostModel): the model as the runhost's policy reads
// it, and its key read from the runhost's store into the job, which keeps it
// in memory and the run's environment alone. A saved model that is gone, or
// that cannot be used here now, is refused by name; nothing falls back to
// the default.
func runhostJobModel(j *job.Job) (int, error) {
	ref := j.Run.RunhostModel
	if ref == "" {
		return 0, nil
	}
	pol, _, err := runhostModelsPolicy()
	if err != nil {
		return engine.ExitPolicy, err
	}
	list := runhostListModels(pol)
	var held remote.SavedModel
	var ok bool
	if ref == job.RunhostDefault {
		held, ok = list.DefaultModel()
		if !ok {
			return engine.ExitPolicy, fmt.Errorf("this runhost holds no saved model, and the run follows its default: make one available from a device (agentry runhost model %s --add <saved model>)", runhostOwnName())
		}
	} else if held, ok = list.Find(ref); !ok || held.ID != ref {
		return engine.ExitPolicy, fmt.Errorf("the run names the saved model %s, which this runhost no longer holds, and a run never takes the default in its place: make it available again (agentry runhost model %s --add <saved model>), or move the run to one it holds", ref, runhostOwnName())
	}
	if held.Refusal != "" {
		return engine.ExitPolicy, fmt.Errorf("the saved model %q cannot be used on this runhost: %s: make it available again from a device (agentry runhost model %s --add %s)", held.Name, held.Refusal, runhostOwnName(), held.Name)
	}
	sm, _ := pol.FindModel(held.ID)
	rm, err := localrun.ChooseRunModel(pol, sm.ID, "")
	if err != nil {
		return engine.ExitPolicy, err
	}
	key := ""
	if held.Kind == remote.ModelKindKey {
		m := rm.Policy.Model
		if key, err = modelkey.Personal(m.APIKeyEnv, m.APIKeySource, m.KeyProvider()); err != nil || key == "" {
			return engine.ExitPolicy, errors.Join(fmt.Errorf("the saved model %q's key could not be read from this runhost's store", held.Name), err)
		}
	}
	j.Run.Model, j.Run.ModelKey, j.Run.RunhostModel = job.ModelOfSaved(rm.Policy.Model, sm), key, ""
	if err := j.Validate(); err != nil {
		return engine.ExitUsage, err
	}
	return 0, nil
}

// runhostModelUsable is a persistent agent's segment's check on a runhost:
// the saved model's key, sign-in or credits token is here.
func runhostModelUsable(agent string) func(localrun.RunModel) error {
	return func(rm localrun.RunModel) error {
		if rm.Policy.Model.Provider == "" {
			return nil
		}
		if refusal := runhostModelRefusal(rm.Policy.Model); refusal != "" {
			name := rm.Saved.Name
			if name == "" {
				name = rm.Policy.Model.KeyProvider()
			}
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("the saved model %q cannot be used on this runhost: %s: make it available again from a device (agentry runhost model %s --add %s), or move the agent to another (agentry agent configure %s --on %s --model <name>)",
				name, refusal, runhostOwnName(), name, agent, runhostOwnName()))
		}
		return nil
	}
}
