// `agentry knowledge` (plan knowledge.md §4.3, §4.8; agentry-cli#318): make
// and extend knowledge packs with the built-in Knowledge Creator, on this
// machine, against the runner policy's model.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/build"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// knowledgeBuildOpts are create's and add's shared flags.
type knowledgeBuildOpts struct {
	files      []string
	dir        string
	out        string
	policyPath string
	timeout    int
	maxTurns   int
	maxCost    float64
}

// knowledgeDocker is the docker client a build uses; a seam for tests.
var knowledgeDocker = func() dockerx.Client { return dockerx.New() }

// newKnowledgeCmd is `agentry knowledge` (plan knowledge.md §4.3, §4.8): the
// knowledge packs a run can search. create and add drive the built-in
// Knowledge Creator on this machine (agentry-cli#318); the store, list,
// attach, export and delete follow with agentry-cli#319.
func newKnowledgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Make and extend knowledge packs a run can search (knowledge create, knowledge add)",
		Long: "A knowledge pack is an LLM wiki of at most 10 MB — cited pages with an index, and the documents\n" +
			"they were compiled from — that a run searches with the knowledge tools (`agentry run --knowledge DIR`).\n" +
			"`create` and `add` run the built-in Knowledge Creator on this machine, against the model your runner\n" +
			"policy names: it costs no credits, and nothing it reads or writes is scanned.",
	}
	cmd.AddCommand(newKnowledgeCreateCmd(), newKnowledgeAddCmd())
	return cmd
}

func knowledgeBuildFlags(cmd *cobra.Command, o *knowledgeBuildOpts) {
	f := cmd.Flags()
	f.StringArrayVar(&o.files, "file", nil, "a document or a directory of documents to ingest (repeatable; required)")
	f.StringVar(&o.out, "out", "", "output folder for the Creator run's reports and receipts (default ./agentry-out/agentry-knowledge-creator-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.IntVar(&o.timeout, "timeout", 0, "the Creator run's wall-clock budget in seconds (default: the policy's)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the Creator run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the Creator run once the harness reports this much cost in USD (default: the policy's)")
}

func newKnowledgeCreateCmd() *cobra.Command {
	var o knowledgeBuildOpts
	cmd := &cobra.Command{
		Use:   "create NAME --file PATH [--file PATH…]",
		Short: "Make a knowledge pack from documents, with the built-in Knowledge Creator",
		Long: "Make a knowledge pack named NAME (lowercase letters, digits, '.', '-', '_') in --dir (default ./NAME)\n" +
			"from the documents given: Markdown, text, HTML, CSV, JSON, YAML and source files as they are; PDF, DOCX,\n" +
			"ODT, EPUB, RTF, PPTX, XLSX and PNG, JPEG, TIFF or WebP images as the text decoded from them before the\n" +
			"run. The documents themselves are kept in the pack beside the pages.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := knowledge.ValidName(args[0]); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			dir := o.dir
			if dir == "" {
				dir = args[0]
			}
			if _, err := os.Stat(filepath.Join(dir, knowledge.ManifestFile)); err == nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already holds a knowledge pack; extend it with `agentry knowledge add %s --file …`", dir, dir))
			}
			return runKnowledgeBuild(cmd, dir, args[0], o)
		},
	}
	knowledgeBuildFlags(cmd, &o)
	cmd.Flags().StringVar(&o.dir, "dir", "", "the directory to make the pack in (default ./NAME)")
	return cmd
}

func newKnowledgeAddCmd() *cobra.Command {
	var o knowledgeBuildOpts
	cmd := &cobra.Command{
		Use:   "add DIR --file PATH [--file PATH…]",
		Short: "Add documents to a knowledge pack, with the built-in Knowledge Creator",
		Long: "Extend the knowledge pack in DIR with the documents given: the Creator reads the pack as it stands and\n" +
			"rewrites the pages the new documents touch. A build that stopped at a budget continues this way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(filepath.Join(args[0], knowledge.ManifestFile)); err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no knowledge pack; make one with `agentry knowledge create NAME --file …`", args[0]))
			}
			return runKnowledgeBuild(cmd, args[0], "", o)
		},
	}
	knowledgeBuildFlags(cmd, &o)
	return cmd
}

func runKnowledgeBuild(cmd *cobra.Command, dir, name string, o knowledgeBuildOpts) error {
	if len(o.files) == 0 {
		return engine.Classify(engine.ExitUsage, errors.New("name the documents to ingest with --file PATH (repeatable)"))
	}
	errOut := cmd.ErrOrStderr()
	home, err := runnerHome()
	if err != nil {
		return err
	}
	policyFile, err := policy.Resolve(o.policyPath)
	if err != nil {
		return err
	}
	pol, polSHA, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	// Said before anything is built or read (plan D19): a pack made here
	// is not scanned, and the person hears it first.
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The Knowledge Creator runs on this machine against the model your runner policy names (%s).\n"+
		"It costs no credits, and nothing it reads or writes is scanned: the documents are not, and the pack it makes\n"+
		"is recorded as \"not scanned\" until a registry scans it.\n", model)

	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir := o.out
	if outDir == "" {
		outDir = filepath.Join("agentry-out", "agentry-knowledge-creator-"+runID)
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	budgets := spec.Budgets{TimeoutSeconds: pol.Budgets.TimeoutSeconds, MaxTurns: pol.Budgets.MaxTurns, MaxCostUSD: pol.Budgets.MaxCostUSD}
	if o.timeout > 0 {
		budgets.TimeoutSeconds = o.timeout
	}
	if o.maxTurns >= 0 {
		budgets.MaxTurns = o.maxTurns
	}
	if o.maxCost >= 0 {
		budgets.MaxCostUSD = o.maxCost
	}

	res, err := build.Build(cmd.Context(), knowledgeDocker(), build.Options{
		PackDir: dir, Name: name, Documents: o.files,
		Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Now: time.Now(), OutDir: outDir, Budgets: &budgets,
		RunID: runID, Stderr: errOut,
	})
	if err != nil {
		if res != nil && len(res.Skipped) > 0 {
			_, _ = fmt.Fprintf(errOut, "not read (no decoder reads them, or the decoder failed): %s\n", strings.Join(res.Skipped, ", "))
		}
		return err
	}
	printKnowledgeBuild(cmd.OutOrStdout(), errOut, dir, outDir, res)
	if res.RunErr != nil {
		_, _ = fmt.Fprintf(errOut, "The Creator run stopped before it finished (%v). The pack holds what it wrote;\n"+
			"run `agentry knowledge add %s` with the same documents to continue.\n", res.RunErr, dir)
		return res.RunErr
	}
	return nil
}

// knowledgeBuildReport is create's and add's --json shape.
type knowledgeBuildReport struct {
	Dir       string              `json:"dir"`
	Manifest  knowledge.Manifest  `json:"manifest"`
	Documents []string            `json:"documents"`
	Skipped   []string            `json:"skipped,omitempty"`
	Written   []string            `json:"written"`
	Dropped   []knowledgeDropJSON `json:"dropped,omitempty"`
	Receipts  string              `json:"receipts"`
	Decode    string              `json:"decode,omitempty"`
	Stopped   string              `json:"stopped,omitempty"`
}

type knowledgeDropJSON struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Kept   bool   `json:"kept,omitempty"`
}

// printKnowledgeBuild says what a build made: text, or one --json document.
func printKnowledgeBuild(out, errOut io.Writer, dir, outDir string, res *build.Result) {
	m := res.Applied.Manifest
	if flags.jsonOut {
		r := knowledgeBuildReport{Dir: dir, Manifest: m, Documents: res.Documents, Skipped: res.Skipped,
			Written: res.Applied.Written, Receipts: filepath.Join(outDir, "receipts")}
		for _, d := range res.Applied.Dropped {
			r.Dropped = append(r.Dropped, knowledgeDropJSON{Path: d.Path, Reason: d.Reason, Kept: d.Kept})
		}
		if res.Decoded != nil {
			r.Decode = res.Decoded.Line()
		}
		if res.RunErr != nil {
			r.Stopped = res.RunErr.Error()
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		_, _ = fmt.Fprintln(out, string(b))
		return
	}
	_, _ = fmt.Fprintf(out, "knowledge pack %s: %s, %d bytes, %s, %s — in %s\n",
		m.Name, knowledge.CountPages(m.Pages), m.Bytes, m.SHA256, m.Scan.Describe(), dir)
	_, _ = fmt.Fprintf(out, "  documents: %s\n", strings.Join(res.Documents, ", "))
	if len(res.Skipped) > 0 {
		_, _ = fmt.Fprintf(out, "  not read: %s (no decoder reads them, or the decoder failed)\n", strings.Join(res.Skipped, ", "))
	}
	for _, d := range res.Applied.Dropped {
		verb := "dropped"
		if d.Kept {
			verb = "kept"
		}
		_, _ = fmt.Fprintf(out, "  %s %s: %s\n", verb, d.Path, d.Reason)
	}
	_, _ = fmt.Fprintf(out, "  receipts: %s\n", filepath.Join(outDir, "receipts"))
	_, _ = fmt.Fprintf(errOut, "attach it to a run with: agentry run <ref> --knowledge %s\n", dir)
}
