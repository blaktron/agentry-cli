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
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage"
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

// deliverySummary is what a run says on stderr about the copies and sends it
// made.
func deliverySummary(ds []receipt.Delivery) []string {
	if len(ds) == 0 {
		return nil
	}
	var folder, hooks []receipt.Delivery
	for _, d := range ds {
		if d.Kind == receipt.DeliveryKindWebhook || d.Kind == receipt.DeliveryKindS3 {
			hooks = append(hooks, d)
		} else {
			folder = append(folder, d)
		}
	}
	return append(folderSummary(folder), destinationSummary(hooks)...)
}

// folderSummary is the lines about the copies made to a folder.
func folderSummary(ds []receipt.Delivery) []string {
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

// destinationSummary is the lines about what was sent to each destination, in
// the order the destinations were first named.
func destinationSummary(ds []receipt.Delivery) []string {
	var order []string
	byName := map[string][]receipt.Delivery{}
	bucket := map[string]bool{}
	for _, d := range ds {
		if _, ok := byName[d.Destination]; !ok {
			order = append(order, d.Destination)
		}
		byName[d.Destination] = append(byName[d.Destination], d)
		bucket[d.Destination] = d.Kind == receipt.DeliveryKindS3
	}
	var out []string
	for _, name := range order {
		sent, left := 0, map[string]int{}
		result := false
		for _, d := range byName[name] {
			switch {
			case d.Status == receipt.DeliveryDelivered && d.Object == "result":
				result = true
			case d.Status == receipt.DeliveryDelivered:
				sent++
			default:
				left[d.Error]++
			}
		}
		switch {
		case sent > 0:
			out = append(out, fmt.Sprintf("destination %s: %d %s sent", name, sent, plural(sent, "file", "files")))
		case result:
			out = append(out, fmt.Sprintf("destination %s: the result sent", name))
		}
		for _, reason := range slices.Sorted(maps.Keys(left)) {
			if reason == receipt.DeliveryErrTooLarge && bucket[name] {
				out = append(out, fmt.Sprintf("destination %s: %d %s not sent (over the %d MiB an object may be); the run itself is unaffected", name, left[reason], plural(left[reason], "file", "files"), engine.MaxObjectBytes>>20))
				continue
			}
			if reason == receipt.DeliveryErrTooLarge {
				out = append(out, fmt.Sprintf("destination %s: %d %s listed without content (over the %d MiB body cap)", name, left[reason], plural(left[reason], "file", "files"), engine.MaxWebhookBody>>20))
				continue
			}
			out = append(out, fmt.Sprintf("destination %s: %d not sent (%s); the run itself is unaffected, and receipts/%s says which; agentry deliver RUN-FOLDER --destination %s tries again", name, left[reason], reason, deliver.LedgerFile, name))
		}
	}
	return out
}

// plainText is s, or s quoted when it holds a control or other character a
// terminal may act on: a record can carry a path the agent chose.
func plainText(s string) string {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return strconv.QuoteToASCII(s)
		}
	}
	return s
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
	var (
		to           string
		destinations []string
	)
	cmd := &cobra.Command{
		Use:   "deliver RUN-FOLDER",
		Short: "Copy a finished run's deliverables to a folder, or send them to a destination",
		Long: "Deliver a run you already made, from its output folder (the one the run printed,\n" +
			"./agentry-out/<namespace>-<name>-<run id> by default). The run's result.json lists what the run was for.\n\n" +
			"With --to FOLDER (or deliver.folder in the config when neither --to nor --destination is given) each file is\n" +
			"copied only after its SHA-256 matches that listing, to FOLDER/<QuickAgent>/<date time> <title or run id>/, with a\n" +
			"file name made safe for any system and ' (2)' after a name that is taken.\n\n" +
			"With --destination NAME (repeatable) the run's result and deliverables are sent again to that saved destination,\n" +
			"which is how a delivery that failed is retried: the same step as a run's, in a container that reaches the\n" +
			"destination's host alone. See agentry destinations.\n\n" +
			"Nothing is moved, rewritten or deleted: the run folder is left as it is, apart from the lines added to its\n" +
			"receipts/deliveries.ndjson. A run that failed delivers nothing. Exit 1 when any file could not be delivered.",
		Example: "  agentry deliver ./agentry-out/acme-weekly-3f9a1c2e --to ~/Documents/Agentry\n" +
			"  agentry deliver ./agentry-out/acme-weekly-3f9a1c2e --destination n8n",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var all []receipt.Delivery
			var firstErr error
			root := ""
			if len(destinations) == 0 || to != "" {
				cfg, cerr := loadCLIConfig()
				if cerr != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; deliver.folder is not read\n", cerr)
				}
				var err error
				if root, err = chooseDeliverFolder(to, cfg); err != nil {
					return err
				}
				if root == "" {
					return engine.Classify(engine.ExitUsage, errors.New("name where to deliver: --to FOLDER, --destination NAME, or deliver.folder in the config"))
				}
			}
			dests, err := destination.Resolve(destinations)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if root != "" {
				ds, err := deliver.Redeliver(args[0], deliver.FolderOptions{Root: root})
				if err != nil && len(ds) == 0 {
					return engine.Classify(engine.ExitCLI, err)
				}
				all, firstErr = append(all, ds...), err
			}
			if len(dests) > 0 {
				ds, err := redeliverToDestinations(cmd, args[0], dests)
				if err != nil && len(ds) == 0 {
					return engine.Classify(engine.ExitCLI, err)
				}
				all, firstErr = append(all, ds...), errors.Join(firstErr, err)
			}
			out := cmd.OutOrStdout()
			if flags.jsonOut {
				if jerr := emitJSON(out, map[string]any{"deliveries": all}); jerr != nil {
					return engine.Classify(engine.ExitCLI, jerr)
				}
			} else {
				for _, d := range all {
					switch {
					case d.Status == receipt.DeliveryDelivered && d.Kind == receipt.DeliveryKindFolder:
						_, _ = fmt.Fprintf(out, "copied %s\n", d.Object)
					case d.Status == receipt.DeliveryDelivered:
						_, _ = fmt.Fprintf(out, "sent %s to %s\n", plainText(d.Object), d.Destination)
					case d.Kind == receipt.DeliveryKindFolder:
						_, _ = fmt.Fprintf(out, "not copied (%s: %s) sha256:%s\n", d.Status, d.Error, d.SHA256)
					default:
						_, _ = fmt.Fprintf(out, "not sent to %s (%s: %s) %s\n", d.Destination, d.Status, d.Error, plainText(d.Object))
					}
				}
			}
			if firstErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: %v\n", firstErr)
			}
			bad := 0
			for _, d := range all {
				if d.Status != receipt.DeliveryDelivered {
					bad++
				}
			}
			if bad > 0 {
				return engine.Classify(engine.ExitCLI, fmt.Errorf("%d of %d %s could not be delivered", bad, len(all), plural(len(all), "file", "files")))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the folder to copy to (default: deliver.folder in the config, unless --destination is given)")
	cmd.Flags().StringArrayVar(&destinations, "destination", nil, "send to this saved destination (repeatable)")
	return cmd
}

// redeliverToDestinations sends a finished run's folder to the destinations
// through the delivery step, building the runner image first when this machine
// has none.
func redeliverToDestinations(cmd *cobra.Command, runDir string, dests []destination.Resolved) ([]receipt.Delivery, error) {
	pol, err := filesPolicy("")
	if err != nil {
		return nil, err
	}
	home, err := policy.Home()
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	docker := newDockerClient()
	if err := docker.Available(cmd.Context()); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureRunnerImage(cmd.Context(), docker, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	return engine.Redeliver(cmd.Context(), docker, engine.RedeliverParams{
		RunDir: runDir, Destinations: dests, Policy: pol, RunnerImage: runnerTag, RunRoot: home, Stderr: cmd.ErrOrStderr(),
	})
}
