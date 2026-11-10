package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// runhostManagedFile is the device's managed settings a runhost's job
// carries, in the job's directory, for the job's run.
const runhostManagedFile = "managed.json"

// newManagedCmd builds `agentry managed`: the managed settings an
// organization's admins set for the signed-in account, as this computer
// holds them.
func newManagedCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "managed",
		Short: "Show or refresh the managed settings your organization sets for you",
		Long: "An organization's admins can set values for their members' Agentry clients,\n" +
			"for example that members may not turn on Unsafe Mode. The registry serves\n" +
			"them as one signed document; agentry verifies it against the registry's\n" +
			"signing keys pinned at sign-in, keeps it in the config directory, and\n" +
			"applies it to every run on this computer, offline too. Signing out\n" +
			"deletes it.",
		Args: cobra.NoArgs,
	}
	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the managed settings every run on this computer applies",
		Long: "Prints each organization's managed settings this computer holds: the\n" +
			"registry, the organization, the version, when the registry issued it, and\n" +
			"each setting's value and whether it is locked (you cannot change it) or the\n" +
			"organization's default (it applies until you choose otherwise). Every run\n" +
			"on this computer applies all of them. What cannot be verified is listed\n" +
			"too. While no run can start (a copy that cannot be verified, a locked\n" +
			"setting this version does not know, or an organization's minimum version\n" +
			"newer than this one), the command exits 11. --registry\n" +
			"shows one registry's alone. Reads the copies on this computer and asks no\n" +
			"registry. --json prints the same as one document.",
		Args: cobra.NoArgs,
		RunE: runManagedShow,
	})
	c.AddCommand(&cobra.Command{
		Use:   "refresh",
		Short: "Read the managed settings from the registry again",
		Long: "Asks the registry (the --registry flag, else the default registry) for the\n" +
			"managed settings of the account signed in to it, verifies the answer against\n" +
			"the registry's signing keys pinned at sign-in, and keeps it in place of the\n" +
			"copy this computer held. When none apply any more, the copy is deleted.\n" +
			"Without a sign-in for the registry the command exits 2. A run reads them\n" +
			"itself when the last answer is over an hour old.",
		Args: cobra.NoArgs,
		RunE: runManagedRefresh,
	})
	return c
}

// managedSettingView is one setting as `managed show` prints it.
type managedSettingView struct {
	ID     string          `json:"id"`
	Value  json.RawMessage `json:"value"`
	Locked bool            `json:"locked"`
	// Known says this version of Agentry knows the setting; Applied that
	// it applies it.
	Known   bool `json:"known"`
	Applied bool `json:"applied"`
}

// managedDocumentView is one organization's document.
type managedDocumentView struct {
	Registry     string               `json:"registry"`
	Organization string               `json:"organization"`
	Version      int64                `json:"version"`
	IssuedAt     time.Time            `json:"issuedAt"`
	FetchedAt    time.Time            `json:"fetchedAt"`
	Settings     []managedSettingView `json:"settings"`
}

// managedProblemView is what could not be verified.
type managedProblemView struct {
	Registry string `json:"registry"`
	Error    string `json:"error"`
}

// managedView is `managed show --json`.
type managedView struct {
	Documents []managedDocumentView `json:"documents"`
	Problems  []managedProblemView  `json:"problems"`
}

func viewOf(e managed.Effective) managedView {
	v := managedView{Documents: []managedDocumentView{}, Problems: []managedProblemView{}}
	for _, st := range e.Documents {
		d := st.Document
		dv := managedDocumentView{Registry: st.Registry, Organization: d.Org, Version: d.Version, IssuedAt: d.IssuedAt, FetchedAt: st.FetchedAt, Settings: []managedSettingView{}}
		ids := make([]string, 0, len(d.Settings))
		for id := range d.Settings {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			s := d.Settings[id]
			_, known := managedsettings.Lookup(id)
			dv.Settings = append(dv.Settings, managedSettingView{ID: id, Value: s.Value, Locked: s.Locked, Known: known, Applied: managed.AppliesSetting(id, s.Locked)})
		}
		v.Documents = append(v.Documents, dv)
	}
	for _, p := range e.Problems {
		v.Problems = append(v.Problems, managedProblemView{Registry: p.Registry, Error: p.Err.Error()})
	}
	return v
}

// heldManaged is every registry's managed settings, or one registry's when
// --registry names it.
func heldManaged() managed.Effective {
	held := managed.Current()
	if flags.registry != "" {
		held = held.Registry(registryURL())
	}
	return held
}

func runManagedShow(cmd *cobra.Command, _ []string) error {
	held := heldManaged()
	v := viewOf(held)
	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return err
		}
	} else {
		writeManaged(cmd.OutOrStdout(), v)
	}
	if reason := held.RunRefusal(); reason != "" {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	return nil
}

// writeManaged renders a managedView for a person.
func writeManaged(out io.Writer, v managedView) {
	if len(v.Documents) == 0 && len(v.Problems) == 0 {
		_, _ = fmt.Fprintln(out, "No managed settings.")
		return
	}
	for i, d := range v.Documents {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		_, _ = fmt.Fprintf(out, "%s, version %d, from %s (issued %s)\n", d.Organization, d.Version, d.Registry, d.IssuedAt.UTC().Format(time.RFC3339))
		width := 0
		for _, s := range d.Settings {
			width = max(width, len(s.ID))
		}
		for _, s := range d.Settings {
			_, _ = fmt.Fprintln(out, settingLine(s, width))
		}
	}
	for _, p := range v.Problems {
		_, _ = fmt.Fprintf(out, "cannot verify the managed settings held for %s: %s\n", cmp.Or(p.Registry, "this computer"), p.Error)
	}
}

func settingLine(s managedSettingView, width int) string {
	how := "default"
	if s.Locked {
		how = "locked"
	}
	value := managedsettings.Setting{Value: s.Value}.String()
	if value == "" {
		value = string(s.Value)
	}
	line := fmt.Sprintf("  %-*s  %s (%s)", width, s.ID, value, how)
	switch {
	case !s.Known && s.Locked:
		line += " — this version of Agentry does not know it, and runs nothing until it is updated"
	case !s.Known:
		line += " — this version of Agentry does not know it, and passes it over"
	}
	return line
}

func runManagedRefresh(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil {
		return err
	}
	if token == "" {
		return &exitError{code: 2, err: fmt.Errorf("not signed in to %s; managed settings are read with your sign-in — run `agentry login`", reg)}
	}
	st, err := managed.Refresh(cmd.Context(), httpClient, reg, token)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if st == nil {
		_, _ = fmt.Fprintf(out, "No managed settings for %s.\n", reg)
		return nil
	}
	_, _ = fmt.Fprintln(out, st.Summary())
	installRequired(cmd, st, token)
	return nil
}

// managedLines are whoami's lines for what `managed show` lists: one per
// organization's document, and one per copy that cannot be verified.
func managedLines(held managed.Effective) []string {
	var out []string
	for _, st := range held.Documents {
		out = append(out, "settings: "+st.Summary())
	}
	for _, p := range held.Problems {
		out = append(out, "settings: the managed settings held for "+cmp.Or(p.Registry, "this computer")+" cannot be verified; `agentry managed refresh` reads them again")
	}
	return out
}

// managedJSON is whoami's managedSettings: each organization and version.
func managedJSON(held managed.Effective) []map[string]any {
	var out []map[string]any
	for _, st := range held.Documents {
		out = append(out, map[string]any{"registry": st.Registry, "organization": st.Document.Org, "version": st.Document.Version})
	}
	return out
}

// syncManaged reads the registry's managed settings before a run when
// the last answer is over managed.FetchInterval old, with the stored
// sign-in or an environment token for the registry. Not signed in, it
// asks nothing. A registry that cannot be asked leaves the last verified
// copy in force, and says so.
func syncManaged(cmd *cobra.Command, reg string) {
	note := func(err error) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the managed settings could not be read again (%v); the last verified copy applies\n", err)
	}
	_, token, _, err := envTokenFor(reg)
	if err != nil {
		note(err)
		return
	}
	if token == "" {
		token = storedCredential(reg)
	}
	if token == "" {
		return
	}
	st, fetched, err := managed.SyncFetched(cmd.Context(), httpClient, reg, token, managed.FetchInterval)
	if err != nil {
		note(err)
		return
	}
	if fetched {
		installRequired(cmd, st, token)
	}
}

// forgetManaged deletes the registry's managed settings at sign-out.
func forgetManaged(cmd *cobra.Command, reg string) {
	if err := managed.Forget(reg); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
}

// loginManaged reads the registry's managed settings at sign-in. Another
// account's copy is deleted (managed.SignedIn); the same account's stays
// until the registry answers, so a sign-in it cannot answer at that moment
// does not lift a lock. A registry that cannot answer is not a failed
// sign-in; the next run asks again.
func loginManaged(cmd *cobra.Command, reg, accountID, token string) {
	if err := managed.SignedIn(reg, accountID); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
	st, err := managed.Refresh(cmd.Context(), httpClient, reg, token)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the managed settings could not be read (%v); the next run asks again\n", err)
	case st != nil && !flags.jsonOut:
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "settings: "+st.Summary())
	}
	if err == nil {
		installRequired(cmd, st, token)
	}
}

// senderManaged reads what the sending device's managed settings say of a
// remote runhost's run, which its service handed it.
func senderManaged(opts runOpts) (*managed.Narrowing, error) {
	if !opts.installed {
		return nil, engine.Classify(engine.ExitUsage, errors.New("--runhost-managed is a remote runhost's own, with --installed"))
	}
	b, err := os.ReadFile(opts.runhostManaged)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("the sending device's managed settings: %w", err))
	}
	var n managed.Narrowing
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("the sending device's managed settings: %w", err))
	}
	if err := n.Validate(); err != nil {
		return nil, engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: err.Error()})
	}
	return &n, nil
}

// refuseUnsafeMode refuses an Unsafe Mode run that this computer's managed
// settings forbid, or that sender's (the device that sent a runhost's run)
// do.
func refuseUnsafeMode(sender *managed.Narrowing) error {
	reason := managed.Current().UnsafeModeRefusal()
	if reason == "" && sender != nil && sender.UnsafeModeRefusal != "" {
		reason = sender.UnsafeModeRefusal + ", on the device that sent this run"
	}
	if reason == "" {
		return nil
	}
	return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: reason})
}

// heldRunPolicy is pol held to this computer's managed settings and, for a
// remote runhost's run, to those of the device that sent it. A sending
// device's settings that cannot be read are refused where the run reads
// them for its compile (senderManaged).
func heldRunPolicy(pol *policy.Policy, opts runOpts) *policy.Policy {
	held := managed.Current().PolicyHeld()
	if opts.runhostManaged != "" {
		if n, err := senderManaged(opts); err == nil {
			held = n.Held(held)
		}
	}
	return pol.WithHeld(pol.Held().Merge(held))
}
