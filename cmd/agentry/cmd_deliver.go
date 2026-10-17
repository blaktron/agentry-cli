package main

// A run's deliverables, taken out of its output folder. `agentry run
// --deliver-to DIR` (or deliver.folder in the config) copies them as the run
// ends; `agentry deliver RUN-FOLDER` copies them from a folder a run already
// made. Both read the run's envelope for what the run was for, copy each file
// only after its SHA-256 matches the envelope's, and leave the run folder as
// it is: the folder is the proof, and the copies are the person's.

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// deliverConfig is config.yaml's deliver block.
type deliverConfig struct {
	// Folder is the folder every run copies its deliverables to when
	// --deliver-to does not name another: an absolute path, or one that
	// begins with ~ for the home folder. Unset, a run delivers nothing.
	Folder string `yaml:"folder,omitempty"`
}

// chooseDeliverFolder is the folder a run delivers to: --deliver-to, made
// absolute against the working folder, else deliver.folder in the config, else
// none. A config path must be absolute or begin with ~: a relative one would
// mean a different folder wherever the run is started from.
func chooseDeliverFolder(flagValue string, cfg cliConfig) (string, error) {
	usage := func(format string, a ...any) (string, error) {
		return "", engine.Classify(engine.ExitUsage, fmt.Errorf(format, a...))
	}
	if flagValue != "" {
		abs, err := filepath.Abs(expandHome(flagValue))
		if err != nil {
			return usage("--deliver-to %s: %v", flagValue, err)
		}
		return abs, nil
	}
	folder := strings.TrimSpace(cfg.Deliver.Folder)
	if folder == "" {
		return "", nil
	}
	dir := expandHome(folder)
	if !filepath.IsAbs(dir) {
		return usage("deliver.folder %q is relative: write an absolute path, or one beginning with ~", folder)
	}
	return filepath.Clean(dir), nil
}

// deliverySummary is what a run says on stderr about the copies it made.
func deliverySummary(ds []receipt.Delivery) []string {
	if len(ds) == 0 {
		return nil
	}
	var out []string
	delivered, notMarked := 0, 0
	folder := ""
	failed := map[string]int{}
	for _, d := range ds {
		if d.Status == receipt.DeliveryDelivered {
			delivered++
			if folder == "" {
				folder = filepath.Dir(d.Object)
			}
			if d.Error == receipt.DeliveryErrNotMarked {
				notMarked++
			}
			continue
		}
		failed[d.Error]++
	}
	if delivered > 0 {
		out = append(out, fmt.Sprintf("deliverables: %d %s copied to %s", delivered, plural(delivered, "file", "files"), folder))
	}
	if notMarked > 0 {
		out = append(out, fmt.Sprintf("deliverables: %d %s could not be flagged as downloaded on this file system", notMarked, plural(notMarked, "copy", "copies")))
	}
	for _, reason := range slices.Sorted(maps.Keys(failed)) {
		out = append(out, fmt.Sprintf("deliverables: %d %s not copied (%s); the run itself is unaffected, and receipts/%s says which", failed[reason], plural(failed[reason], "file", "files"), reason, deliver.LedgerFile))
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func printDeliverySummary(w io.Writer, env *receipt.Envelope) {
	for _, line := range deliverySummary(env.Deliveries) {
		_, _ = fmt.Fprintln(w, line)
	}
}

func newDeliverCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "deliver RUN-FOLDER",
		Short: "Copy a finished run's deliverables to a folder",
		Long: "Copy the deliverables of a run you already made, from its output folder (the one the run\n" +
			"printed, ./agentry-out/<namespace>-<name>-<run id> by default), to --to FOLDER, or to\n" +
			"deliver.folder in the config when --to is not given. The run's result.json lists what the\n" +
			"run was for; each file is copied only after its SHA-256 matches that listing, to\n" +
			"FOLDER/<QuickAgent>/<date time> <title or run id>/, with a file name made safe for any system\n" +
			"and ' (2)' after a name that is taken. Nothing is moved, rewritten or deleted: the run folder\n" +
			"is left as it is, apart from the lines added to its receipts/deliveries.ndjson.\n\n" +
			"A run that failed lists no deliverables. Exit 1 when any file could not be copied.",
		Example: "  agentry deliver ./agentry-out/acme-weekly-3f9a1c2e --to ~/Documents/Agentry",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cerr := loadCLIConfig()
			if cerr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; deliver.folder is not read\n", cerr)
			}
			root, err := chooseDeliverFolder(to, cfg)
			if err != nil {
				return err
			}
			if root == "" {
				return engine.Classify(engine.ExitUsage, errors.New("name the folder to copy to: --to FOLDER, or deliver.folder in the config"))
			}
			ds, err := deliver.Redeliver(args[0], deliver.FolderOptions{Root: root})
			if err != nil && len(ds) == 0 {
				return engine.Classify(engine.ExitCLI, err)
			}
			out := cmd.OutOrStdout()
			if flags.jsonOut {
				if jerr := emitJSON(out, map[string]any{"deliveries": ds}); jerr != nil {
					return engine.Classify(engine.ExitCLI, jerr)
				}
			} else {
				for _, d := range ds {
					if d.Status == receipt.DeliveryDelivered {
						_, _ = fmt.Fprintf(out, "copied %s\n", d.Object)
						continue
					}
					_, _ = fmt.Fprintf(out, "not copied (%s: %s) sha256:%s\n", d.Status, d.Error, d.SHA256)
				}
			}
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: %v\n", err)
			}
			bad := 0
			for _, d := range ds {
				if d.Status != receipt.DeliveryDelivered {
					bad++
				}
			}
			if bad > 0 {
				return engine.Classify(engine.ExitCLI, fmt.Errorf("%d of %d %s could not be copied", bad, len(ds), plural(len(ds), "file", "files")))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the folder to copy to (default: deliver.folder in the config)")
	return cmd
}
