// Saved models and providers: `agentry models` and `agentry providers`. A
// provider is a credential (a key, a model sign-in, a connected account or
// Agentry credits, and for an endpoint of your own its address); a saved
// model is a provider plus a model id, under a name you give it. Up to ten
// are kept on a computer, and the default is what a run uses unless it names
// another. Every save is checked first (pkg/runner/modelcheck), and a model
// or provider something still names is not removed until it is moved.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/modelcheck"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// listModelsToCheck is the save check's models listing; a seam, so tests
// reach no provider.
var listModelsToCheck = setup.ListModelsToCheck

// modelsPolicy is the policy the commands change: --policy, else the runner
// home's, written from defaults when there is none yet.
func modelsPolicy(flag string) (*policy.Policy, string, error) {
	pf, err := policy.Resolve(flag)
	if err != nil {
		return nil, "", err
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitPolicy, err)
	}
	return pol, pf.Path, nil
}

// modelCheckers are the save check's real askers on this computer.
func modelCheckers(cmd *cobra.Command, home string, pol *policy.Policy, policyPath string) modelcheck.Checkers {
	return modelcheck.Checkers{
		Key: func(m policy.Model) (string, error) {
			return modelkey.Personal(m.APIKeyEnv, m.APIKeySource, m.KeyProvider())
		},
		ListModels:         listModelsToCheck,
		ProbeContextWindow: probeContextWindow,
		ChatGPTModels: func(context.Context) ([]string, error) {
			_, models, err := chatgptModels(cmd, home)
			return models, err
		},
		CopilotModels: func(_ context.Context, plan string) ([]string, error) {
			return copilotModels(cmd, home, pol, true)(plan)
		},
		Claude: func(ctx context.Context, model string) (modelcheck.ClaudeAnswer, error) {
			s := claudeSignIn(home)
			s.Policy, s.PolicyPath = pol, policyPath
			return s.CheckModel(ctx, model)
		},
		Credits: func(ctx context.Context, m policy.Model) error {
			c := creditsClient(&policy.Policy{Model: m})
			if c == nil {
				return errors.New("this computer is not signed in to the registry the credits are paid through (agentry login)")
			}
			o, err := c.Open(ctx, inference.PurposeRun)
			if err != nil {
				return err
			}
			_, _ = c.End(context.WithoutCancel(ctx), o.Session.ID)
			return nil
		},
	}
}

// checkSavedModel checks sm before it is saved, saying first what a Claude
// subscription's check spends and taking the person's yes; the window an
// endpoint of the person's own reports fills sm's unless window overrides it.
func checkSavedModel(cmd *cobra.Command, pol *policy.Policy, policyPath string, sm *policy.SavedModel, window int64, yes bool) (modelcheck.Result, error) {
	home, err := runnerHome()
	if err != nil {
		return modelcheck.Result{}, err
	}
	view, err := pol.ModelView(*sm)
	if err != nil {
		return modelcheck.Result{}, engine.Classify(engine.ExitUsage, err)
	}
	errOut := cmd.ErrOrStderr()
	if view.APIKeyKind == policy.KeyKindClaudeSignIn {
		_, _ = fmt.Fprintln(errOut, modelcheck.ClaudeNotice)
		if !yes {
			if !stdinIsTTY(cmd.InOrStdin()) {
				return modelcheck.Result{}, engine.Classify(engine.ExitUsage, errors.New("checking a Claude subscription's model sends it one message: pass --yes to check and save it"))
			}
			p := &setup.TTYPrompter{In: bufio.NewReader(cmd.InOrStdin()), Err: errOut}
			ok, err := p.Confirm("Check and save?", false)
			if err != nil {
				return modelcheck.Result{}, err
			}
			if !ok {
				return modelcheck.Result{}, engine.Classify(engine.ExitUsage, errors.New("nothing saved"))
			}
		}
	}
	_, _ = fmt.Fprintf(narration(cmd), "checking %s on %s…\n", sm.Model, sm.Provider)
	res, err := modelcheck.Check(cmd.Context(), modelCheckers(cmd, home, pol, policyPath), view)
	if err != nil {
		return res, engine.Classify(engine.ExitPolicy, err)
	}
	if res.Warning != "" {
		_, _ = fmt.Fprintln(errOut, "warning: "+res.Warning)
	}
	switch {
	case window != 0:
		if err := policy.CheckContextWindow(window); err != nil {
			return res, engine.Classify(engine.ExitUsage, fmt.Errorf("--context-window: %w", err))
		}
		sm.ContextWindow = window
	case res.ContextWindow > 0:
		sm.ContextWindow = res.ContextWindow
		_, _ = fmt.Fprintf(errOut, "context window: %d tokens, as %s reports it\n", res.ContextWindow, res.Source)
	case view.OwnEndpoint() && sm.ContextWindow == 0:
		_, _ = fmt.Fprintln(errOut, "warning: "+setup.UnknownWindowSentence(sm.Model))
	}
	return res, nil
}

// savedModelOut is one saved model in --json: names, never a credential.
type savedModelOut struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	Default       bool   `json:"default,omitempty"`
	// Resolved is the id a Claude alias resolved to at the check.
	Resolved string `json:"resolved,omitempty"`
}

func modelOut(sm policy.SavedModel, def string) savedModelOut {
	return savedModelOut{ID: sm.ID, Name: sm.Name, Provider: sm.Provider, Model: sm.Model, ContextWindow: sm.ContextWindow, Default: sm.ID == def}
}

// sayModel prints one saved model's line.
func sayModel(w io.Writer, sm policy.SavedModel, def, resolved string) {
	mark := " "
	if sm.ID == def {
		mark = "*"
	}
	extra := ""
	if resolved != "" && resolved != sm.Model {
		extra += " (" + resolved + " at the check)"
	}
	if sm.ContextWindow > 0 {
		extra += fmt.Sprintf(" · context %d", sm.ContextWindow)
	}
	_, _ = fmt.Fprintf(w, "%s %s · %s · %s%s\n", mark, sm.Name, sm.Provider, sm.Model, extra)
}

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List, add, change and remove the saved models on this computer, and choose the default",
		Long: "A saved model is a provider (agentry providers) plus a model id, under a name you give it.\n" +
			"This computer keeps up to 10. The first one saved is the default, which a run uses unless it\n" +
			"names another. Every save is checked first: the provider's own model list must offer the id\n" +
			"(a Claude subscription is sent one short message instead, which counts toward its usage), and\n" +
			"a provider that cannot be reached refuses the save with the reason. A model a schedule names\n" +
			"is not removed until the schedule is moved to another (--move-to).",
	}
	cmd.AddCommand(newModelsListCmd(), newModelsAddCmd(), newModelsEditCmd(), newModelsRemoveCmd(), newModelsDefaultCmd(), newModelsCheckCmd())
	return cmd
}

func newModelsListCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the saved models (* marks the default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, _, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			_, models, def := pol.SavedModels()
			if flags.jsonOut {
				rows := make([]savedModelOut, 0, len(models))
				for _, sm := range models {
					rows = append(rows, modelOut(sm, def))
				}
				return emitJSON(cmd.OutOrStdout(), map[string]any{"default": def, "max": policy.MaxSavedModels, "models": rows})
			}
			for _, sm := range models {
				sayModel(cmd.OutOrStdout(), sm, def, "")
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%d of %d saved models\n", len(models), policy.MaxSavedModels)
			return nil
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// modelFlags are add's and edit's.
type modelFlags struct {
	provider, model, name, policyPath string
	window                            int64
	makeDefault, yes                  bool
}

func newModelsAddCmd() *cobra.Command {
	var f modelFlags
	cmd := &cobra.Command{
		Use:   "add <name> --provider <id> --model <model id>",
		Short: "Save a model on a provider this computer has, checked first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.provider == "" || f.model == "" {
				return engine.Classify(engine.ExitUsage, errors.New("--provider and --model are required (agentry providers list names the providers)"))
			}
			pol, path, err := modelsPolicy(f.policyPath)
			if err != nil {
				return err
			}
			sm := policy.SavedModel{Name: args[0], Provider: f.provider, Model: f.model}
			// The cap, the name and the provider are refused before the
			// provider is asked, so a refused save spends nothing (a Claude
			// subscription's check is a message on the plan).
			if err := pol.CanAddModel(sm); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			res, err := checkSavedModel(cmd, pol, path, &sm, f.window, f.yes)
			if err != nil {
				return err
			}
			saved, err := pol.AddModel(sm)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if f.makeDefault {
				if _, err := pol.SetDefault(saved.ID); err != nil {
					return err
				}
			}
			return saveModels(cmd, pol, path, saved, res.Resolved, "saved")
		},
	}
	modelFlagSet(cmd, &f, true)
	return cmd
}

// modelFlagSet adds add's and edit's flags.
func modelFlagSet(cmd *cobra.Command, f *modelFlags, add bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.provider, "provider", "", "the provider's id (agentry providers list)")
	fl.StringVar(&f.model, "model", "", "the model id the provider is asked for (a Claude subscription also takes an alias, such as sonnet)")
	fl.Int64Var(&f.window, "context-window", 0, "the model's context window in tokens, for an endpoint of your own (default: what the endpoint reports)")
	fl.BoolVar(&f.yes, "yes", false, "check a Claude subscription's model without asking first (the check sends it one short message)")
	fl.StringVar(&f.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	if add {
		fl.BoolVar(&f.makeDefault, "default", false, "make the saved model the default")
	} else {
		fl.StringVar(&f.name, "name", "", "a new name for the saved model")
	}
}

// saveModels writes the policy and says what was saved.
func saveModels(cmd *cobra.Command, pol *policy.Policy, path string, sm policy.SavedModel, resolved, did string) error {
	if err := policy.Save(path, pol); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	// A schedule that runs signed out holds its own sealed copy of the
	// keys: a changed default is sealed again now.
	resealSignedOut(cmd.ErrOrStderr())
	if flags.jsonOut {
		out := modelOut(sm, pol.DefaultModel().ID)
		out.Resolved = resolved
		return emitJSON(cmd.OutOrStdout(), out)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: ", did)
	sayModel(cmd.OutOrStdout(), sm, pol.DefaultModel().ID, resolved)
	return nil
}

func newModelsEditCmd() *cobra.Command {
	var f modelFlags
	cmd := &cobra.Command{
		Use:   "edit <name> [--name <new name>] [--provider <id>] [--model <model id>] [--context-window <tokens>]",
		Short: "Change a saved model; a new provider or model id is checked first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(f.policyPath)
			if err != nil {
				return err
			}
			old, ok := pol.FindModel(args[0])
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no saved model %q on this computer (agentry models list)", args[0]))
			}
			sm := old
			if f.name != "" {
				sm.Name = f.name
			}
			if f.provider != "" {
				sm.Provider = f.provider
			}
			if f.model != "" {
				sm.Model = f.model
			}
			resolved := ""
			if sm.Provider != old.Provider || sm.Model != old.Model {
				sm.ContextWindow = 0
				res, err := checkSavedModel(cmd, pol, path, &sm, f.window, f.yes)
				if err != nil {
					return err
				}
				resolved = res.Resolved
			} else if f.window != 0 {
				if err := policy.CheckContextWindow(f.window); err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--context-window: %w", err))
				}
				sm.ContextWindow = f.window
			}
			saved, err := pol.EditModel(old.ID, sm)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			return saveModels(cmd, pol, path, saved, resolved, "changed")
		},
	}
	modelFlagSet(cmd, &f, false)
	return cmd
}

func newModelsDefaultCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "default <name>",
		Short: "Make a saved model the default, which runs use unless they name another",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			sm, err := pol.SetDefault(args[0])
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			return saveModels(cmd, pol, path, sm, "", "default")
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

func newModelsCheckCmd() *cobra.Command {
	var policyPath string
	var yes bool
	cmd := &cobra.Command{
		Use:   "check <name>",
		Short: "Check a saved model again, as a save does, without changing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			sm, ok := pol.FindModel(args[0])
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no saved model %q on this computer (agentry models list)", args[0]))
			}
			checked := sm
			res, err := checkSavedModel(cmd, pol, path, &checked, 0, yes)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				out := modelOut(sm, pol.DefaultModel().ID)
				out.Resolved = res.Resolved
				return emitJSON(cmd.OutOrStdout(), map[string]any{"model": out, "ok": true, "contextWindow": res.ContextWindow, "warning": res.Warning})
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), "ok: ")
			sayModel(cmd.OutOrStdout(), sm, pol.DefaultModel().ID, res.Resolved)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "check a Claude subscription's model without asking first (the check sends it one short message)")
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// modelUser is something on this computer that names a saved model.
type modelUser struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// modelUsers are the schedules on this computer that name one of the saved
// models sms, by id or by name, or that run on the harness whose block in
// pol one of them was read from and name no saved model, which run on it;
// then the persistent agents that name one of them.
func modelUsers(home string, pol *policy.Policy, sms []policy.SavedModel) ([]modelUser, []schedule.Schedule, []persistent.Agent, error) {
	list, _, err := schedule.StoreIn(home).List()
	if err != nil {
		return nil, nil, nil, err
	}
	var users []modelUser
	var named []schedule.Schedule
	for _, sc := range list {
		ref := strings.TrimSpace(sc.Run.Model)
		if ref == "" {
			if sm, ok := pol.HarnessModel(sc.Run.Adapter); ok {
				ref = sm.ID
			}
		}
		if ref == "" {
			continue
		}
		if slices.ContainsFunc(sms, func(sm policy.SavedModel) bool { return sm.ID == ref || strings.EqualFold(sm.Name, ref) }) {
			users = append(users, modelUser{Kind: "schedule", ID: sc.ID, Name: sc.Name})
			named = append(named, sc)
		}
	}
	ids := map[string]bool{}
	for _, sm := range sms {
		ids[sm.ID] = true
	}
	agentUsers, agents := modelAgents(home, ids)
	return append(users, agentUsers...), named, agents, nil
}

// usedRefusal is the refusal of a removal while something names the model:
// what is the sentence's subject and verb ("the saved model "x" is").
func usedRefusal(what string, users []modelUser) error {
	lines := make([]string, 0, len(users))
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("%s %s (%s)", u.Kind, u.Name, u.ID))
	}
	return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s named by %s: move them first with --move-to <saved model>", what, strings.Join(lines, ", ")))
}

// moveUsers points each schedule at the saved model to.
func moveUsers(home string, named []schedule.Schedule, to policy.SavedModel) error {
	st := schedule.StoreIn(home)
	for _, sc := range named {
		sc.Run.Model = to.ID
		if err := st.Save(sc); err != nil {
			return fmt.Errorf("move schedule %s to %s: %w", sc.Name, to.Name, err)
		}
	}
	return nil
}

// moveTarget is the saved model --move-to names, which must not be among
// the ones removed.
func moveTarget(pol *policy.Policy, ref string, removed []policy.SavedModel) (policy.SavedModel, error) {
	to, ok := pol.FindModel(ref)
	if !ok {
		return to, engine.Classify(engine.ExitUsage, fmt.Errorf("--move-to: no saved model %q on this computer", ref))
	}
	if slices.ContainsFunc(removed, func(sm policy.SavedModel) bool { return sm.ID == to.ID }) {
		return to, engine.Classify(engine.ExitUsage, fmt.Errorf("--move-to: %q is removed too", to.Name))
	}
	return to, nil
}

// removedOut is a removal in --json.
type removedOut struct {
	Removed []savedModelOut `json:"removed"`
	Moved   []modelUser     `json:"moved"`
	MovedTo string          `json:"movedTo,omitempty"`
	Default string          `json:"default"`
	// Provider is the provider removed, with what became of its credential.
	Provider   string `json:"provider,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// removeModels removes sms (with provider, when it is a provider's removal),
// moving what names them to moveTo, and writes the policy.
func removeModels(cmd *cobra.Command, pol *policy.Policy, path, what string, sms []policy.SavedModel, moveTo string, remove func(newDefault string) error) (removedOut, error) {
	home, err := runnerHome()
	if err != nil {
		return removedOut{}, err
	}
	users, named, agents, err := modelUsers(home, pol, sms)
	if err != nil {
		return removedOut{}, err
	}
	var to policy.SavedModel
	if moveTo != "" {
		if to, err = moveTarget(pol, moveTo, sms); err != nil {
			return removedOut{}, err
		}
	} else if len(users) > 0 {
		return removedOut{}, usedRefusal(what, users)
	}
	if err := remove(to.ID); err != nil {
		return removedOut{}, engine.Classify(engine.ExitUsage, err)
	}
	// What named the model is moved first: the model it moves to is in the
	// policy before and after, so no schedule ever names one that is gone.
	if err := moveUsers(home, named, to); err != nil {
		return removedOut{}, err
	}
	if err := moveAgents(cmd.Context(), home, agents, to); err != nil {
		return removedOut{}, engine.Classify(engine.ExitCLI, err)
	}
	if err := policy.Save(path, pol); err != nil {
		return removedOut{}, engine.Classify(engine.ExitPolicy, err)
	}
	resealSignedOut(cmd.ErrOrStderr())
	out := removedOut{Moved: users, MovedTo: to.Name, Default: pol.DefaultModel().ID}
	if out.Moved == nil {
		out.Moved = []modelUser{}
	}
	for _, sm := range sms {
		out.Removed = append(out.Removed, modelOut(sm, ""))
	}
	return out, nil
}

func sayRemoved(w io.Writer, pol *policy.Policy, out removedOut) {
	if out.Provider != "" {
		_, _ = fmt.Fprintf(w, "removed the provider %s (%s)\n", out.Provider, out.Credential)
	}
	for _, sm := range out.Removed {
		_, _ = fmt.Fprintf(w, "removed the saved model %s\n", sm.Name)
	}
	for _, u := range out.Moved {
		_, _ = fmt.Fprintf(w, "moved %s %s to %s\n", u.Kind, u.Name, out.MovedTo)
	}
	_, _ = fmt.Fprintf(w, "default: %s\n", pol.DefaultModel().Name)
}

func newModelsRemoveCmd() *cobra.Command {
	var policyPath, moveTo string
	cmd := &cobra.Command{
		Use:   "remove <name> [--move-to <name>]",
		Short: "Remove a saved model; what names it, and the default, move to --move-to",
		Long: "Removing a saved model that a schedule or a persistent agent names is refused, with them listed,\n" +
			"unless --move-to names the saved model they move to; a running agent takes it at its next turn.\n" +
			"Removing the default needs the new default, which --move-to names too. Nothing moves to the\n" +
			"default on its own.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			sm, ok := pol.FindModel(args[0])
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no saved model %q on this computer (agentry models list)", args[0]))
			}
			out, err := removeModels(cmd, pol, path, fmt.Sprintf("the saved model %q is", sm.Name), []policy.SavedModel{sm}, moveTo, func(newDefault string) error {
				_, err := pol.RemoveModel(sm.ID, newDefault)
				return err
			})
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			sayRemoved(cmd.OutOrStdout(), pol, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&moveTo, "move-to", "", "the saved model the schedules and agents naming this one move to, and the new default when this is the default")
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}
