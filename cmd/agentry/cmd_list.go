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

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/installed"
)

// listedRecord is one install record with the fact a client sorts on.
type listedRecord struct {
	installed.Record
	// Present is true when Dir exists and is a directory. A record whose
	// directory is gone is still a record: nothing prunes it silently.
	Present bool `json:"present"`
}

// listOutput is the `list --json` shape.
type listOutput struct {
	// Path is the state file the records came from.
	Path    string         `json:"path"`
	Records []listedRecord `json:"records"`
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List this machine's install records (embedded installs and runner consents)",
		Long: "List the shared install state (~/.config/agentry/installed.yaml, or under\n" +
			"AGENTRY_CONFIG_DIR): every record `install` and `run` wrote, with its target,\n" +
			"directory and version, and whether the directory still exists. Nothing is\n" +
			"changed. --json prints the records as data.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runList(cmd) },
	}
}

func runList(cmd *cobra.Command) error {
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
		recs = append(recs, listedRecord{Record: r, Present: statErr == nil && info.IsDir()})
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
		if !r.Present {
			mark = "  (directory missing)"
		}
		_, _ = fmt.Fprintf(out, "%s/%s@%s  %s  %s%s\n", r.Namespace, r.Name, r.Version, r.Target, r.Dir, mark)
	}
	return nil
}
