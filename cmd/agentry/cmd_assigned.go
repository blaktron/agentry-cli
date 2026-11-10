// Installs an organization's managed settings ask for: the QuickAgents its
// quickagents row requires. They go through the install flow of
// `agentry install` with every check it makes (the install sources the
// settings allow, the digest, the attestation, the risk the person may
// accept, the manifest rules), but nothing is asked and nothing is
// consented to for the person: the record left holds no capabilities, so
// the person's first run shows the contract and asks.

package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// assignedInstall is an install made for an organization's managed settings.
type assignedInstall struct {
	// organization is the handle of the organization that requires it.
	organization string
	// registry and token are the registry the settings came from and the
	// sign-in that fetched them.
	registry, token string
}

// current says the runner home already holds the resolved release, intact,
// and its record names that version: nothing is installed again.
func (a *assignedInstall) current(res *apiclient.ResolveResult) (bool, error) {
	home, err := runnerHome()
	if err != nil {
		return false, err
	}
	state, err := installed.Load()
	if err != nil {
		return false, nil
	}
	rec := state.Find(installdir.ConsentKey(home, res.Namespace, res.Name))
	if rec == nil || rec.Version != res.Version {
		return false, nil
	}
	_, ierr := installdir.Inspect(installdir.ReleaseDir(home, res.Namespace, res.Name, res.Version, res.Digest), res.Digest)
	return ierr == nil, nil
}

// installRequired installs or updates each QuickAgent the freshly fetched
// document requires. A failure installs nothing for that assignment, is said
// on one line of stderr, and never fails the command that fetched.
func installRequired(cmd *cobra.Command, st *managed.State, token string) {
	installRequiredExcept(cmd, st, token, "")
}

// installRequiredExcept is installRequired leaving the release skip (ns/name)
// as it is.
func installRequiredExcept(cmd *cobra.Command, st *managed.State, token, skip string) {
	if st == nil {
		return
	}
	for _, a := range (managed.Effective{Documents: []managed.State{*st}}).Required() {
		if a.Ref == skip {
			continue
		}
		err := runInstall(cmd, a.Selector(), installOpts{assigned: &assignedInstall{organization: a.Organization, registry: st.Registry, token: token}})
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s, required by %s, was not installed: %s\n", a.Ref, a.Organization, oneLineError(err))
		}
	}
}

// oneLineError is err's text on one line.
func oneLineError(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// assignedView is one assignment as `search --org --json` prints it.
type assignedView struct {
	Ref          string `json:"ref"`
	Version      string `json:"version"`
	Mode         string `json:"mode"`
	Organization string `json:"organization"`
	Registry     string `json:"registry"`
}

// runSearchOrg lists the assignments of the held documents whose reference
// contains query. It asks no registry.
func runSearchOrg(cmd *cobra.Command, query string, held managed.Effective) error {
	results := []assignedView{}
	for _, a := range held.Offered() {
		if query != "" && !strings.Contains(strings.ToLower(a.Ref), strings.ToLower(query)) {
			continue
		}
		results = append(results, assignedView{Ref: a.Ref, Version: a.Version, Mode: a.Mode, Organization: a.Organization, Registry: a.Registry})
	}
	out := cmd.OutOrStdout()
	if flags.jsonOut {
		return emitJSON(out, map[string]any{"results": results, "total": len(results)})
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(out, "No QuickAgents are assigned by your organization's managed settings.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REF\tVERSION\tMODE\tORGANIZATION")
	for _, r := range results {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Ref, r.Version, r.Mode, r.Organization)
	}
	return tw.Flush()
}

// assignmentOf is the assignment of the held documents that ref (ns/name,
// any version) is a release of.
func assignmentOf(ref string, held managed.Effective) (managed.Assigned, bool) {
	r, err := manifest.ParseRef(ref)
	if err != nil {
		return managed.Assigned{}, false
	}
	return held.AssignmentOf(r.Namespace + "/" + r.Name)
}

// applyPresetModel runs a QuickAgent an organization assigned with a
// preset model on the saved model of pol that is that model, when the
// person named none (localrun.PresetModel): a --model a locked preset does
// not allow is refused in the organization's name, and so is a preset
// model the managed model rows refuse. A run of a folder, a runhost's own
// run and a hosted run are left as they are.
func applyPresetModel(opts *runOpts, ref string, held managed.Effective, pol *policy.Policy, w io.Writer) error {
	if ref == "" || opts.local != "" || opts.installed || hostedFor(*opts) {
		return nil
	}
	model, warning, err := localrun.PresetModel(pol, held, ref, opts.model, opts.adapterName)
	var conflict *localrun.PresetModelConflict
	if errors.As(err, &conflict) {
		a, _ := assignmentOf(ref, held)
		return presetRefusal(a, "--model "+opts.model, fmt.Sprintf("its model at %s's %s", conflict.Want.Provider, conflict.Want.Model))
	}
	var missing *localrun.PresetModelMissing
	if errors.As(err, &missing) {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: missing.Error() + ": save it with agentry models add"})
	}
	if err != nil {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: err.Error()})
	}
	if warning != "" {
		_, _ = fmt.Fprintf(w, "agentry: %s\n", warning)
	}
	opts.model = model
	return nil
}

// presetRefusal is the refusal of a flag that conflicts with a locked preset.
func presetRefusal(a managed.Assigned, flag, what string) error {
	return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: fmt.Sprintf(
		"%s conflicts with %s's managed settings, which fix %s for %s (%s)",
		flag, a.Organization, what, a.Ref, managedsettings.QuickAgents)})
}

// applyPreset fills the run's options from the preset an organization gave
// the QuickAgent the run is of. Each preset field is a default: the person's
// own flag stands, unless the preset is locked, when a flag that differs is
// refused. The result is only the options the run would otherwise take from
// flags, so every other managed setting (a cap, a lock on where runs go, on
// the model, on the install sources) still holds the run afterwards. The
// preset's model is applied once the policy is read (applyPresetModel).
func applyPreset(opts *runOpts, ref string, held managed.Effective) error {
	if ref == "" || opts.local != "" {
		return nil
	}
	a, ok := assignmentOf(ref, held)
	if !ok || a.Preset == nil {
		return nil
	}
	p := a.Preset
	if p.TimeoutSeconds > 0 {
		switch {
		case opts.timeout <= 0:
			opts.timeout = p.TimeoutSeconds
		case a.PresetLocked && opts.timeout != p.TimeoutSeconds:
			return presetRefusal(a, "--timeout", fmt.Sprintf("the timeout at %d seconds", p.TimeoutSeconds))
		}
	}
	if p.MaxTurns > 0 {
		switch {
		case opts.maxTurns < 0:
			opts.maxTurns = p.MaxTurns
		case a.PresetLocked && opts.maxTurns != p.MaxTurns:
			return presetRefusal(a, "--max-turns", fmt.Sprintf("the tool-call budget at %d", p.MaxTurns))
		}
	}
	if a.PresetLocked && p.Where != "" {
		onRunhost := opts.on != ""
		switch {
		case p.Where == managedsettings.WhereLocal && onRunhost:
			return presetRefusal(a, "--on", "its runs to this computer")
		case p.Where == managedsettings.WhereRunhost && !onRunhost:
			return presetRefusal(a, "a run on this computer", "its runs to a runhost; pass --on")
		case p.Where == managedsettings.WhereHosted:
			return presetRefusal(a, "a run from the command line", "its runs to In Agentry")
		}
	}
	given := map[string]string{}
	for _, kv := range opts.intake {
		if id, v, ok := strings.Cut(kv, "="); ok {
			given[id] = v
		}
	}
	ids := make([]string, 0, len(p.Inputs))
	for id := range p.Inputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v, answered := given[id]
		switch {
		case !answered:
			opts.orgIntake = append(slices.Clone(opts.orgIntake), id+"="+p.Inputs[id])
		case a.PresetLocked && v != p.Inputs[id]:
			return presetRefusal(a, "--intake "+id, "the answer to "+id)
		}
	}
	return nil
}
