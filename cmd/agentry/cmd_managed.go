package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
)

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
		Short: "Print the managed settings that apply, with their values and whether each is locked",
		Long: "Prints the organization, the version, when the registry issued it and each\n" +
			"setting: its value, and whether it is locked (you cannot change it) or the\n" +
			"organization's default (it applies until you choose otherwise). Reads the\n" +
			"verified copy on this computer and asks no registry; `agentry managed\n" +
			"refresh` reads it again. --json prints the same as one document.",
		Args: cobra.NoArgs,
		RunE: runManagedShow,
	})
	c.AddCommand(&cobra.Command{
		Use:   "refresh",
		Short: "Read the managed settings from the registry again",
		Args:  cobra.NoArgs,
		RunE:  runManagedRefresh,
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

// managedView is `managed show --json`.
type managedView struct {
	Registry     string               `json:"registry"`
	Managed      bool                 `json:"managed"`
	Organization string               `json:"organization,omitempty"`
	Version      int64                `json:"version,omitempty"`
	IssuedAt     *time.Time           `json:"issuedAt,omitempty"`
	FetchedAt    *time.Time           `json:"fetchedAt,omitempty"`
	Settings     []managedSettingView `json:"settings,omitempty"`
}

func viewOf(reg string, st *managed.State) managedView {
	v := managedView{Registry: reg}
	if st == nil {
		return v
	}
	d := st.Document
	issued, fetched := d.IssuedAt, st.FetchedAt
	v.Managed, v.Organization, v.Version, v.IssuedAt, v.FetchedAt = true, d.Org, d.Version, &issued, &fetched
	ids := make([]string, 0, len(d.Settings))
	for id := range d.Settings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := d.Settings[id]
		_, known := managedsettings.Lookup(id)
		v.Settings = append(v.Settings, managedSettingView{ID: id, Value: s.Value, Locked: s.Locked, Known: known, Applied: slices.Contains(managed.Enforced, id)})
	}
	return v
}

func runManagedShow(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	st, err := managed.Load(reg)
	if err != nil {
		// The refusal class: a copy that does not verify holds Unsafe
		// Mode off until it is read again.
		return &exitError{code: 11, err: fmt.Errorf("%w; `agentry managed refresh` reads them again", err)}
	}
	v := viewOf(reg, st)
	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	writeManaged(cmd.OutOrStdout(), v)
	return nil
}

// writeManaged renders a managedView for a person.
func writeManaged(out io.Writer, v managedView) {
	if !v.Managed {
		_, _ = fmt.Fprintf(out, "No managed settings for %s.\n", v.Registry)
		return
	}
	_, _ = fmt.Fprintf(out, "registry:     %s\norganization: %s\nversion:      %d\nissued:       %s\nfetched:      %s\n",
		v.Registry, v.Organization, v.Version, v.IssuedAt.UTC().Format(time.RFC3339), v.FetchedAt.UTC().Format(time.RFC3339))
	if len(v.Settings) == 0 {
		_, _ = fmt.Fprintln(out, "settings:     none")
		return
	}
	_, _ = fmt.Fprintln(out, "settings:")
	width := 0
	for _, s := range v.Settings {
		width = max(width, len(s.ID))
	}
	for _, s := range v.Settings {
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
		_, _ = fmt.Fprintln(out, line)
	}
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
	_, _ = fmt.Fprintf(out, "managed by %s (version %d)\n", st.Document.Org, st.Document.Version)
	return nil
}

// managedLine is whoami's line for the registry's managed settings, ""
// when none are held.
func managedLine(reg string) string {
	st, err := managed.Load(reg)
	switch {
	case err != nil:
		return "settings: the managed settings held for this registry do not verify; `agentry managed refresh` reads them again"
	case st == nil:
		return ""
	}
	return fmt.Sprintf("settings: managed by %s (version %d)", st.Document.Org, st.Document.Version)
}

// syncManaged reads the registry's managed settings before a run when
// the last answer is over managed.FetchInterval old, with the stored
// sign-in or an environment token for the registry. Not signed in, it
// asks nothing. A registry that cannot be asked leaves the last verified
// copy in force, and says so.
func syncManaged(cmd *cobra.Command, reg string) {
	_, token, _, err := envTokenFor(reg)
	if err != nil {
		return
	}
	if token == "" {
		token = storedCredential(reg)
	}
	if token == "" {
		return
	}
	if _, err := managed.Sync(cmd.Context(), httpClient, reg, token, managed.FetchInterval); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the managed settings could not be read again (%v); the last verified copy applies\n", err)
	}
}

// forgetManaged deletes the registry's managed settings at sign-out and
// before a sign-in, which may be another account's.
func forgetManaged(cmd *cobra.Command, reg string) {
	if err := managed.Forget(reg); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
}

// loginManaged replaces the registry's managed settings at sign-in: the
// copy held belonged to whoever was signed in before, and the new
// account's are read now. A registry that cannot answer is not a failed
// sign-in; the next run asks again.
func loginManaged(cmd *cobra.Command, reg, token string) {
	forgetManaged(cmd, reg)
	st, err := managed.Refresh(cmd.Context(), httpClient, reg, token)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the managed settings could not be read (%v); the next run asks again\n", err)
	case st != nil && !flags.jsonOut:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "settings: managed by %s (version %d)\n", st.Document.Org, st.Document.Version)
	}
}
