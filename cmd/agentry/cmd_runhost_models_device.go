package main

// A runhost's saved models, from the device: `agentry runhost model <name>`
// lists them, --add makes one of this computer's saved models available
// there through the relay its credential takes, --remove takes one away and
// --default chooses the runhost's default. The runhost checks each one
// before it keeps it (cmd_runhost_models.go).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelcheck"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// runhostModelTimeout bounds one call that makes a saved model available:
// its check may build a harness's image on the runhost the first time.
const runhostModelTimeout = 15 * time.Minute

// rememberRunhostModels keeps what the runhost name said of its saved
// models in this computer's record of it, for runhost list.
func rememberRunhostModels(name string, list remote.Models) {
	home, err := runnerHome()
	if err != nil {
		return
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return
	}
	rec, ok := recs.Find(name)
	if !ok {
		return
	}
	rec.Models = []runhost.ModelRecord{}
	for _, sm := range list.Models {
		rec.Models = append(rec.Models, runhost.ModelRecord{ID: sm.ID, Name: sm.Name, Provider: sm.Provider, Model: sm.Model, Default: sm.Default})
	}
	_ = recs.Save(home)
}

// runhostModelsSaid is a record's saved models as runhost list says them:
// their names, the default marked.
func runhostModelsSaid(rec runhost.Record) string {
	if rec.Models == nil {
		return "(not read yet)"
	}
	if len(rec.Models) == 0 {
		return "none"
	}
	names := make([]string, 0, len(rec.Models))
	for _, m := range rec.Models {
		n := safeTerminalText(m.Name, 64)
		if m.Default {
			n = "*" + n
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// printRunhostModels prints a runhost's saved models, one a line, the
// default marked *, with what names each and why one cannot be used.
func printRunhostModels(w io.Writer, name string, list remote.Models) {
	if len(list.Models) == 0 {
		_, _ = fmt.Fprintf(w, "runhost %s holds no saved model: agentry runhost model %s --add <saved model> makes one of this computer's available there\n", name, name)
		return
	}
	_, _ = fmt.Fprintf(w, "runhost %s holds %d of at most %d saved models (* the default):\n", name, len(list.Models), list.Max)
	for _, sm := range list.Models {
		mark := " "
		if sm.Default {
			mark = "*"
		}
		line := fmt.Sprintf("%s %s · %s · %s", mark, safeTerminalText(sm.Name, 64), sm.Provider, safeTerminalText(sm.Model, 128))
		if sm.ContextWindow > 0 {
			line += fmt.Sprintf(" · context %d", sm.ContextWindow)
		}
		_, _ = fmt.Fprintln(w, line)
		if sm.Refusal != "" {
			_, _ = fmt.Fprintf(w, "    cannot be used there now: %s\n", safeTerminalText(sm.Refusal, 400))
		}
		if len(sm.Users) > 0 {
			_, _ = fmt.Fprintf(w, "    named by %s\n", safeTerminalText(usersSaid(sm.Users), 400))
		}
	}
}

// runhostSays prints a runhost's narration line on this computer's stderr.
func runhostSays(cmd *cobra.Command, name string) func(string) {
	errOut := narration(cmd)
	return func(s string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", name, safeTerminalText(s, 4096)) }
}

func runRunhostModelsList(cmd *cobra.Command, name string) error {
	c, _, err := dialAgentRunhost(cmd, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	list, err := remote.ListModels(cmd.Context(), c)
	if err != nil {
		return err
	}
	rememberRunhostModels(name, list)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "models": list})
	}
	printRunhostModels(cmd.OutOrStdout(), name, list)
	return nil
}

// runhostModelSubscriptionRefusal is the refusal of a pasted Claude
// subscription token on a runhost.
const runhostModelSubscriptionRefusal = "a pasted Claude subscription token runs only the installed claude, which a runhost does not run: save a model on your Claude subscription through Claude Code's own sign-in (agentry providers add anthropic --sign-in) and make that one available"

// runRunhostModelAdd makes this computer's saved model ref available on the
// runhost name: its key read here and sent to the runhost's store, its
// sign-in made on the runhost from the browser here, or its credits token
// minted for the runhost, unless the runhost keeps one already; then the
// runhost checks it and keeps it. Nothing of a sign-in stays here.
func runRunhostModelAdd(cmd *cobra.Command, name, ref string, oauthApp, yes bool) error {
	pol, err := devicePolicy()
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	sm, ok := pol.FindModel(ref)
	if !ok {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("no saved model %q on this computer (agentry models list)", ref))
	}
	view, err := pol.ForRun(sm)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	m := view.Model
	providers, _, _ := pol.SavedModels()
	pr := providers[sm.Provider]
	if m.APIKeyKind == policy.KeyKindSubscription {
		return engine.Classify(engine.ExitPolicy, errors.New(runhostModelSubscriptionRefusal))
	}
	kind := runhostModelKind(m)
	what := fmt.Sprintf("%q (%s/%s)", sm.Name, sm.Provider, sm.Model)
	home, err := runnerHome()
	if err != nil {
		return err
	}
	if kind == remote.ModelKindKey {
		if !job.ValidKeyEnv(m.APIKeyEnv) {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("the saved model %s names its key's variable $%s, which a runhost does not take: name it as the provider does, ending in _API_KEY, _KEY or _TOKEN", what, m.APIKeyEnv))
		}
		if vendor := modelsignin.ConnectedVendor(sm.Provider); vendor != "" {
			if r, err := modelsignin.Open(home, connectionKeys()).Get(vendor); err == nil && r.SecretIn != "" {
				return engine.Classify(engine.ExitPolicy, fmt.Errorf("the %s token Agentry connected for %s is renewed on this computer at each run's start, and a runhost could not renew its copy: paste a %s token that does not expire (agentry providers add %s), then make it available", modelsignin.ConnectName(vendor), what, modelsignin.ConnectName(vendor), sm.Provider))
			}
		}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), runhostModelTimeout)
	defer cancel()
	cmd.SetContext(ctx)
	c, rec, err := dialAgentRunhost(cmd, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	agreed := yes
	ask := func(q string) error {
		if agreed {
			return nil
		}
		a := terminalAsker(cmd)
		if a == nil {
			return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
		}
		ok, err := a.Confirm(q, false)
		if err != nil {
			return err
		}
		if !ok {
			return engine.Classify(engine.ExitPolicy, errors.New("nothing was made available"))
		}
		return nil
	}
	key := ""
	switch kind {
	case remote.ModelKindKey:
		if err := ask(fmt.Sprintf("Keep your %s key on runhost %s (%s), encrypted in its store, so its persistent agents and schedules can run on %s?", sm.Provider, name, rec.Host, what)); err != nil {
			return err
		}
		if key, err = modelkey.Personal(m.APIKeyEnv, m.APIKeySource, m.KeyProvider()); err != nil {
			return engine.Classify(engine.ExitPolicy, err)
		}
		if key == "" {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("no key for the saved model %s here: set $%s, or keep it in the keychain (agentry providers add %s)", what, m.APIKeyEnv, sm.Provider))
		}
	case remote.ModelKindSignIn:
		vendor := modelsignin.VendorOf(m.APIKeyKind)
		if !runhostKeepsSignIn(ctx, c, vendor) {
			_, _ = fmt.Fprintf(narration(cmd), "runhost %s keeps no working %s sign-in: it signs in now, from your browser here\n", name, vendor)
			if _, err := signInRunhost(cmd, name, vendor, oauthApp, yes); err != nil {
				return err
			}
		}
		if m.APIKeyKind == policy.KeyKindClaudeSignIn {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), modelcheck.ClaudeNotice)
			if err := ask(fmt.Sprintf("Check %s on runhost %s, and keep it there?", what, name)); err != nil {
				return err
			}
		}
	case remote.ModelKindCredits:
		reg, err := policy.AgentryRegistry(m)
		if err != nil {
			return engine.Classify(engine.ExitPolicy, err)
		}
		if !runhostKeepsCredits(ctx, c, reg) {
			_, _ = fmt.Fprintf(narration(cmd), "runhost %s keeps no credits token for %s: one is made for it now\n", name, reg)
			if _, err := giveRunhostCredits(cmd, name, yes); err != nil {
				return err
			}
		}
	}
	doc := remote.PutModel{ID: sm.ID, Name: sm.Name, ProviderID: sm.Provider, Model: sm.Model, ContextWindow: sm.ContextWindow, Key: key, Yes: agreed || yes,
		Provider: remote.ModelProvider{Name: pr.Name, APIKeyEnv: m.APIKeyEnv, APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ProxyAllow: m.ProxyAllow}}
	if m.APIKeyKind == policy.KeyKindClaudeSignIn {
		// The person agreed to the check's message above, or with --yes.
		doc.Yes = true
	}
	if doc.Provider.ProxyAllow == nil {
		doc.Provider.ProxyAllow = []string{}
	}
	ans, err := remote.PutSavedModel(ctx, c, doc, runhostSays(cmd, name))
	if err != nil {
		return fmt.Errorf("runhost %s: %w", name, err)
	}
	rememberRunhostModels(name, ans.Models)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "model": ans.Model, "checked": ans.Checked, "models": ans.Models})
	}
	if ans.Checked.Warning != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+ans.Checked.Warning)
	}
	line := fmt.Sprintf("runhost %s holds the saved model %q (%s/%s)", name, ans.Model.Name, ans.Model.Provider, ans.Model.Model)
	if ans.Checked.Resolved != "" && ans.Checked.Resolved != ans.Model.Model {
		line += fmt.Sprintf(", %s at the check", ans.Checked.Resolved)
	}
	if ans.Model.Default {
		line += ", its default"
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
	printNextSteps(cmd.ErrOrStderr(),
		nextStep{command: fmt.Sprintf("agentry agent configure <agent> --on %s --model %q", name, ans.Model.Name), notes: []string{"puts one of its persistent agents on it"}},
		nextStep{command: fmt.Sprintf("agentry schedule add <ns/name> --on %s --model %q", name, ans.Model.Name), notes: []string{"a schedule there on it"}})
	return nil
}

// runhostKeepsSignIn says whether the runhost c reaches keeps a working
// sign-in to vendor.
func runhostKeepsSignIn(ctx context.Context, c *runhost.Client, vendor string) bool {
	list, err := remote.ListStore(ctx, c)
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

// runhostKeepsCredits says whether the runhost c reaches keeps a credits
// token for the registry reg.
func runhostKeepsCredits(ctx context.Context, c *runhost.Client, reg string) bool {
	list, err := remote.ListStore(ctx, c)
	return err == nil && list.Credits != nil && strings.TrimRight(list.Credits.Registry, "/") == strings.TrimRight(reg, "/")
}

func runRunhostModelRemove(cmd *cobra.Command, name, ref, moveTo string, keep bool) error {
	c, _, err := dialAgentRunhost(cmd, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	out, err := remote.RemoveSavedModel(cmd.Context(), c, remote.RemoveModel{Ref: ref, MoveTo: moveTo, KeepCredential: keep}, runhostSays(cmd, name))
	if err != nil {
		return fmt.Errorf("runhost %s: %w", name, err)
	}
	rememberRunhostModels(name, out.Models)
	forgetEndedCredentials(cmd.Context(), c, name)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "removed": out})
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "runhost %s no longer holds the saved model %q\n", name, out.Removed.Name)
	for _, u := range out.Moved {
		_, _ = fmt.Fprintf(w, "moved %s %s to %s\n", u.Kind, u.Name, out.MovedTo)
	}
	if out.Credential != "" {
		_, _ = fmt.Fprintf(w, "credential: %s\n", safeTerminalText(out.Credential, 400))
	}
	if def, ok := out.Models.DefaultModel(); ok {
		_, _ = fmt.Fprintf(w, "default: %s\n", safeTerminalText(def.Name, 64))
	} else {
		_, _ = fmt.Fprintln(w, "it holds no saved model now")
	}
	return nil
}

// forgetEndedCredentials brings this computer's record of the runhost's
// sign-ins and credits token in line with what its store keeps.
func forgetEndedCredentials(ctx context.Context, c *runhost.Client, name string) {
	list, err := remote.ListStore(ctx, c)
	if err != nil {
		return
	}
	home, err := runnerHome()
	if err != nil {
		return
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return
	}
	rec, ok := recs.Find(name)
	if !ok {
		return
	}
	kept := rec.SignIns[:0]
	for _, s := range rec.SignIns {
		for _, k := range list.SignIns {
			if k.Vendor == s.Vendor {
				kept = append(kept, s)
				break
			}
		}
	}
	rec.SignIns = kept
	if list.Credits == nil {
		rec.Credits = nil
	}
	_ = recs.Save(home)
}

func runRunhostModelDefault(cmd *cobra.Command, name, ref string) error {
	c, _, err := dialAgentRunhost(cmd, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	list, err := remote.SetDefaultModel(cmd.Context(), c, ref)
	if err != nil {
		return fmt.Errorf("runhost %s: %w", name, err)
	}
	rememberRunhostModels(name, list)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "models": list})
	}
	def, _ := list.DefaultModel()
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s's default saved model is %q (%s/%s): its agents and schedules that name none run on it\n", name, def.Name, def.Provider, def.Model)
	return nil
}

// runhostModelRef is the runhost's saved model ref names, as its id, from
// the runhost c reaches: "default" stays itself. One the runhost does not
// hold is refused by name.
func runhostModelRef(ctx context.Context, c remote.Caller, name, ref string) (string, remote.SavedModel, error) {
	if strings.EqualFold(strings.TrimSpace(ref), job.RunhostDefault) {
		return job.RunhostDefault, remote.SavedModel{}, nil
	}
	list, err := remote.ListModels(ctx, c)
	if err != nil {
		return "", remote.SavedModel{}, err
	}
	rememberRunhostModels(name, list)
	sm, ok := list.Find(ref)
	if !ok {
		return "", sm, engine.Classify(engine.ExitUsage, fmt.Errorf("--model: runhost %s holds no saved model %q: agentry runhost model %s lists those it holds, and agentry runhost model %s --add <saved model> makes one of this computer's available there", name, ref, name, name))
	}
	return sm.ID, sm, nil
}
