// `agentry list` (agentry-cli#102): the shared install state as this machine
// holds it — every record `install` and `run` wrote, each with whether its
// directory still exists. Text for a person, --json for a client; nothing
// is changed. The runner's own consent records (target "runner") are listed
// as what they are.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
)

// listedRecord is one install record with the fact a client sorts on.
type listedRecord struct {
	installed.Record
	// Present is true when Dir exists and is a directory. A record whose
	// directory is gone is still a record: nothing prunes it silently.
	Present bool `json:"present"`
	// Under --check (agentry-cli#190): Latest is the newest version the
	// record's registry resolves ns/name to, UpdateAvailable whether it is
	// newer than the installed one, CheckError why the registry could not
	// say — one record's unreachable registry is that record's fact, not
	// the listing's failure.
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable,omitempty"`
	CheckError      string `json:"checkError,omitempty"`
}

// listOutput is the `list --json` shape.
type listOutput struct {
	// Path is the state file the records came from.
	Path    string         `json:"path"`
	Records []listedRecord `json:"records"`
}

func newListCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List this machine's install records (runner installs, embedded installs and runner consents)",
		Long: "List the shared install state (~/.config/agentry/installed.yaml, or under\n" +
			"AGENTRY_CONFIG_DIR): every record `install` and `run` wrote, with its target,\n" +
			"directory and version, and whether the directory still exists. Nothing is\n" +
			"changed. --json prints the records as data. --check asks each record's\n" +
			"registry (the configured one for runner records; the credential goes to the\n" +
			"configured registry only) for the newest version and marks the records an\n" +
			"`agentry install ns/name` would update — in JSON, latest, updateAvailable and\n" +
			"checkError per record; a registry that cannot be reached is said on that\n" +
			"record, and the listing goes on.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runList(cmd, check) },
	}
	cmd.Flags().BoolVar(&check, "check", false, "ask each record's registry for the newest version and mark the ones with an update available")
	return cmd
}

func runList(cmd *cobra.Command, check bool) error {
	path, err := installed.StatePath()
	if err != nil {
		return err
	}
	state, err := installed.Load()
	if err != nil {
		return err
	}
	recs := make([]listedRecord, 0, len(state.Records))
	for _, r := range state.Records {
		info, statErr := os.Stat(r.Dir)
		rec := listedRecord{Record: r, Present: statErr == nil && info.IsDir()}
		// A local install (agentry-cli#491) has no registry to ask.
		if check && !r.Local() {
			checkUpdate(cmd, &rec)
		}
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Dir < b.Dir
	})
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), listOutput{Path: path, Records: recs})
	}
	out := cmd.OutOrStdout()
	if len(recs) == 0 {
		_, _ = fmt.Fprintf(out, "nothing recorded in %s\n", path)
		return nil
	}
	for _, r := range recs {
		mark := ""
		if r.Local() {
			mark = "  (local, unsigned)"
		}
		if !r.Present {
			mark = "  (directory missing)"
		}
		switch {
		case r.UpdateAvailable:
			mark += "  update available: " + r.Latest
		case r.CheckError != "":
			mark += "  (could not check: " + r.CheckError + ")"
		case check && r.Latest != "":
			mark += "  up to date"
		}
		_, _ = fmt.Fprintf(out, "%s/%s@%s  %s  %s%s\n", r.Namespace, r.Name, r.Version, r.Target, r.Dir, mark)
	}
	return nil
}

// checkUpdate asks the record's registry — the one it names, or the
// configured one for a runner record, whose key names none because a run
// resolves through the configured registry — what ns/name resolves to
// now, and compares versions as semver. What it learns, or why it could
// not, lands on the record.
func checkUpdate(cmd *cobra.Command, rec *listedRecord) {
	reg := rec.Registry
	if reg == installdir.ConsentRegistry {
		reg = registryURL()
	}
	// The credential rides only to the configured registry: a record's
	// registry column is a state file's, and a bearer must not go wherever
	// a row points. A private namespace on another registry reads as an
	// error on that record, not as a token sent there.
	token := ""
	if strings.TrimRight(reg, "/") == strings.TrimRight(registryURL(), "/") {
		token = credentialFor(reg)
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		rec.CheckError = err.Error()
		return
	}
	res, err := resolveRef(cmd.Context(), client, rec.Namespace+"/"+rec.Name, false)
	if err != nil {
		rec.CheckError = err.Error()
		return
	}
	rec.Latest = res.Version
	have, herr := semver.NewVersion(rec.Version)
	latest, lerr := semver.NewVersion(res.Version)
	if herr != nil || lerr != nil {
		// Registry versions are semver (V3); a record's that is not (a
		// hand-edited row) cannot be compared, and that is what is said.
		rec.CheckError = fmt.Sprintf("installed version %q and the registry's %q are not comparable", rec.Version, res.Version)
		return
	}
	rec.UpdateAvailable = latest.GreaterThan(have)
}
