package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/pastruns"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// `agentry runs` lists the runs made from here, from their output folders;
// `agentry open` opens one's folder in the system's file manager. Both read
// the folders as they are and change nothing.

// runsOutput is the `runs --json` shape.
type runsOutput struct {
	// Dir is the output directory the runs were read from.
	Dir  string         `json:"dir"`
	Runs []pastruns.Run `json:"runs"`
}

func newRunsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List the runs made from here, newest first",
		Long: "List the runs whose output folders are under the output directory (./agentry-out,\n" +
			"where agentry run writes unless --out names another folder; --dir reads another).\n" +
			"Each line is when the run started, its outcome (ok, or the exit class of a\n" +
			"failure), how long it took, the QuickAgent and its folder. --json prints the\n" +
			"runs as data, each with its class's meaning and retry hint. Nothing is changed.\n" +
			"agentry open RUN opens one's folder; agentry deliver RUN-FOLDER copies its files.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := pastruns.List(dir)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if flags.jsonOut {
				if runs == nil {
					runs = []pastruns.Run{}
				}
				return emitJSON(cmd.OutOrStdout(), runsOutput{Dir: dir, Runs: runs})
			}
			if len(runs) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no runs under %s\n", dir)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "STARTED\tOUTCOME\tTOOK\tQUICKAGENT\tFOLDER")
			for _, r := range runs {
				// The ref and the folder name are read from a folder on disk, so
				// each is made safe for the terminal, and for the table's columns.
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Started.Local().Format("2006-01-02 15:04"), r.Outcome(), took(r), cell(r.Ref), cell(r.Dir))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&dir, "dir", pastruns.DefaultDir, "the output directory to read the runs from")
	return cmd
}

// cell is a value read from disk as one table cell: safe for the terminal,
// with no tab or line break of its own.
func cell(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ").Replace(termtext.Safe(s))
}

// took is a run's duration for the listing: the measured one, else the span
// of its timestamps, else nothing.
func took(r pastruns.Run) string {
	if r.WallMS > 0 {
		return humanDuration(time.Duration(r.WallMS) * time.Millisecond)
	}
	if d := r.Ended.Sub(r.Started); d > 0 {
		return humanDuration(d)
	}
	return "-"
}

// openFolder hands a folder to the system's file manager; tests replace it.
var openFolder = func(path string) error { return systemOpen(path, "explorer") }

func newOpenCmd() *cobra.Command {
	var (
		dir       string
		printOnly bool
	)
	cmd := &cobra.Command{
		Use:   "open RUN",
		Short: "Open a run's output folder",
		Long: "Open a run's output folder in the system's file manager. RUN is the folder's\n" +
			"path, the folder's name under the output directory (./agentry-out, or --dir),\n" +
			"or the run's id, whole or its first characters when they name one run. The\n" +
			"folder's path is printed; --print prints it and opens nothing, for a script\n" +
			"or a terminal with no desktop. agentry runs lists the runs.",
		Example: "  agentry open 3f9a1c2e\n  agentry open ./agentry-out/acme-weekly-3f9a1c2e\n  cd \"$(agentry open --print 3f9a1c2e)\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := pastruns.Find(dir, args[0])
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			// The path is printed absolute, so a shell can cd to it from
			// anywhere.
			path, err := filepath.Abs(r.Dir)
			if err != nil {
				path = r.Dir
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			if printOnly {
				return nil
			}
			if err := openFolder(path); err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					return engine.Classify(engine.ExitCLI, fmt.Errorf("this system has no program to open a folder with; the path is printed above, and --print prints it alone"))
				}
				return engine.Classify(engine.ExitCLI, fmt.Errorf("open %s: %w", path, err))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", pastruns.DefaultDir, "the output directory to look the run up in")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the folder's path and open nothing")
	return cmd
}
