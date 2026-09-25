// `agentry knowledge` (plan knowledge.md §4.3, §4.8; agentry-cli#318, #319):
// make and extend knowledge packs with the built-in Knowledge Creator, on
// this machine, against the runner policy's model, and keep them sealed in
// the local store the desktop shares (cmd_knowledge_store.go).

package main

import (
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
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
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

// knowledgeBuilder runs the Creator over a pack directory; a seam for the
// commands' tests, which check what is kept and said around a build (the
// build itself is pkg/knowledge/build's, tested there).
var knowledgeBuilder = runKnowledgeBuild

// newKnowledgeCmd is `agentry knowledge` (plan knowledge.md §4.3, §4.8): the
// knowledge packs a run can search. create and add drive the built-in
// Knowledge Creator on this machine (agentry-cli#318); the packs are kept
// sealed in the store under the runner home, which the desktop shares, and
// list, import, upload, download, export and delete work on it
// (agentry-cli#319). Attaching one is `agentry run REF --knowledge NAME`.
func newKnowledgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Make, keep and move the knowledge packs a run can search",
		Long: "A knowledge pack is an LLM wiki of at most 10 MB — cited pages with an index, and the documents\n" +
			"they were compiled from — that a run searches with the knowledge tools (`agentry run REF --knowledge NAME`).\n" +
			"`create` and `add` run the built-in Knowledge Creator on this machine, against the model your runner\n" +
			"policy names: it costs no credits, and nothing it reads or writes is scanned.\n\n" +
			"Packs are kept sealed on this computer, each under a key of its own that the registry you are signed in\n" +
			"to wraps for your account (Agentry holds that key), so the same pack opens on another computer after you\n" +
			"sign in there. Signed out, this computer's key wraps it instead, until your next sign-in. `upload` keeps a\n" +
			"copy in Agentry, where it is scanned; `download` brings a copy back.",
	}
	cmd.AddCommand(newKnowledgeListCmd(), newKnowledgeCreateCmd(), newKnowledgeAddCmd(), newKnowledgeImportCmd(),
		newKnowledgeUploadCmd(), newKnowledgeDownloadCmd(), newKnowledgeExportCmd(), newKnowledgeDeleteCmd())
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
		Long: "Make a knowledge pack named NAME (lowercase letters, digits, '.', '-', '_') from the documents given:\n" +
			"Markdown, text, HTML, CSV, JSON, YAML and source files as they are; PDF, DOCX, ODT, EPUB, RTF, PPTX, XLSX\n" +
			"and PNG, JPEG, TIFF or WebP images as the text decoded from them before the run. The documents themselves\n" +
			"are kept in the pack beside the pages. The pack is kept sealed on this computer; --dir DIR makes it as a\n" +
			"plain directory instead, in the clear, which `agentry knowledge import DIR` can seal later.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := knowledge.ValidName(args[0]); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if o.dir == "" {
				return createKept(cmd, args[0], o)
			}
			if _, err := os.Stat(filepath.Join(o.dir, knowledge.ManifestFile)); err == nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already holds a knowledge pack; extend it with `agentry knowledge add %s --file …`", o.dir, o.dir))
			}
			res, outDir, err := knowledgeBuilder(cmd, o.dir, args[0], o)
			return finishDirBuild(cmd, o.dir, outDir, res, err)
		},
	}
	knowledgeBuildFlags(cmd, &o)
	cmd.Flags().StringVar(&o.dir, "dir", "", "make the pack as a plain directory here, not kept sealed")
	return cmd
}

func newKnowledgeAddCmd() *cobra.Command {
	var o knowledgeBuildOpts
	cmd := &cobra.Command{
		Use:   "add NAME|DIR --file PATH [--file PATH…]",
		Short: "Add documents to a knowledge pack, with the built-in Knowledge Creator",
		Long: "Extend the kept pack NAME (or the pack directory DIR) with the documents given: the Creator reads the pack\n" +
			"as it stands and rewrites the pages the new documents touch. A build that stopped at a budget continues\n" +
			"this way. A kept pack stays under the key it had.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keptPackNamed(cmd, args[0]) {
				return addKept(cmd, args[0], o)
			}
			if knowledgeArgIsPath(args[0]) || isDir(args[0]) {
				if _, err := os.Stat(filepath.Join(args[0], knowledge.ManifestFile)); err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no knowledge pack; make one with `agentry knowledge create NAME --file …`", args[0]))
				}
				res, outDir, err := knowledgeBuilder(cmd, args[0], "", o)
				return finishDirBuild(cmd, args[0], outDir, res, err)
			}
			return addKept(cmd, args[0], o)
		},
	}
	knowledgeBuildFlags(cmd, &o)
	return cmd
}

// runKnowledgeBuild runs the Creator over the pack directory dir (a new
// pack named name, or the one there when name is empty) and answers what
// it made and where its reports are. A result with RunErr set is a pack
// the Creator stopped short of finishing, which holds what it wrote.
func runKnowledgeBuild(cmd *cobra.Command, dir, name string, o knowledgeBuildOpts) (*build.Result, string, error) {
	if len(o.files) == 0 {
		return nil, "", engine.Classify(engine.ExitUsage, errors.New("name the documents to ingest with --file PATH (repeatable)"))
	}
	errOut := cmd.ErrOrStderr()
	home, err := runnerHome()
	if err != nil {
		return nil, "", err
	}
	policyFile, err := policy.Resolve(o.policyPath)
	if err != nil {
		return nil, "", err
	}
	pol, polSHA, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitPolicy, err)
	}
	// Said before anything is built or read (plan D19): a pack made here
	// is not scanned, and the person hears it first.
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The Knowledge Creator runs on this machine against the model your runner policy names (%s).\n"+
		"It costs no credits, and nothing it reads or writes is scanned: the documents are not, and the pack it makes\n"+
		"is recorded as \"not scanned\" until a registry scans it.\n", model)

	runID, err := engine.NewRunID()
	if err != nil {
		return nil, "", err
	}
	outDir := o.out
	if outDir == "" {
		outDir = filepath.Join("agentry-out", "agentry-knowledge-creator-"+runID)
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return nil, "", err
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
		return nil, "", err
	}
	return res, outDir, nil
}

// finishDirBuild reports a build into a plain pack directory.
func finishDirBuild(cmd *cobra.Command, dir, outDir string, res *build.Result, err error) error {
	if err != nil {
		return err
	}
	printKnowledgeBuild(cmd.OutOrStdout(), cmd.ErrOrStderr(), dir, outDir, res, nil)
	return stoppedShort(cmd, dir, res)
}

// stoppedShort says a Creator run stopped before it finished, and how to
// continue it (target is the pack's name, or its directory).
func stoppedShort(cmd *cobra.Command, target string, res *build.Result) error {
	if res.RunErr == nil {
		return nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The Creator run stopped before it finished (%v). The pack holds what it wrote;\n"+
		"run `agentry knowledge add %s` with the same documents to continue.\n", res.RunErr, target)
	return res.RunErr
}

// createKept builds a new pack in a 0700 directory inside the store, then
// keeps it sealed there; the plain directory goes with the build, and so
// does the Creator run's output, which holds the pages it wrote (unless
// --out named a place for it). A pack that could not be kept is left where
// it was built, and said so, rather than thrown away with the run that
// made it.
func createKept(cmd *cobra.Command, name string, o knowledgeBuildOpts) error {
	s, err := knowledgeStore(cmd)
	if err != nil {
		return err
	}
	if _, err := s.Find(name); err == nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a knowledge pack named %s is already kept on this computer; extend it with `agentry knowledge add %s --file …`", name, name))
	}
	rewrapOnSignIn(cmd, s)
	scratch, done, err := s.Scratch()
	if err != nil {
		return err
	}
	kept := false
	defer func() {
		if kept {
			done()
		}
	}()
	o, private := privateCreatorOut(cmd, o, scratch)
	dir := filepath.Join(scratch, name)
	res, outDir, err := knowledgeBuilder(cmd, dir, name, o)
	if err != nil {
		kept = true // nothing worth keeping: the scratch goes
		return err
	}
	e, err := s.Keep(cmd.Context(), dir)
	if err != nil {
		return fmt.Errorf("the pack was built but could not be kept sealed (%w); it is in %s, in the clear, until the store's sweep removes it an hour from now — keep it with `agentry knowledge import %s`, or copy it out", err, dir, dir)
	}
	kept = true
	if private {
		outDir = ""
	}
	printKnowledgeBuild(cmd.OutOrStdout(), cmd.ErrOrStderr(), "", outDir, res, &e)
	return stoppedShort(cmd, name, res)
}

// addKept opens a kept pack into a 0700 directory inside the store, runs
// the Creator over it, and seals the result back under the same key.
func addKept(cmd *cobra.Command, name string, o knowledgeBuildOpts) error {
	if len(o.files) == 0 {
		return engine.Classify(engine.ExitUsage, errors.New("name the documents to ingest with --file PATH (repeatable)"))
	}
	s, err := knowledgeStore(cmd)
	if err != nil {
		return err
	}
	if _, err := s.Find(name); err != nil {
		return storeErr(err)
	}
	scratch, done, err := s.Scratch()
	if err != nil {
		return err
	}
	defer done()
	o, private := privateCreatorOut(cmd, o, scratch)
	dir := filepath.Join(scratch, name)
	if _, err := s.Open(cmd.Context(), name, dir); err != nil {
		return err
	}
	res, outDir, err := knowledgeBuilder(cmd, dir, "", o)
	if err != nil {
		return err
	}
	e, err := s.Reseal(cmd.Context(), name, dir)
	if err != nil {
		return err
	}
	if private {
		outDir = ""
	}
	printKnowledgeBuild(cmd.OutOrStdout(), cmd.ErrOrStderr(), "", outDir, res, &e)
	if e.Key.Agentry != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Agentry still keeps the copy from before; send this one with `agentry knowledge upload %s`\n", name)
	}
	return stoppedShort(cmd, name, res)
}

// privateCreatorOut puts the Creator run's output, whose reports/ holds
// every page it wrote, inside the scratch that goes with a kept build,
// unless --out named a place for it, which is then said to hold the pages
// in the clear. private reports which.
func privateCreatorOut(cmd *cobra.Command, o knowledgeBuildOpts, scratch string) (knowledgeBuildOpts, bool) {
	if o.out != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the Creator run's output in %s keeps the pages it writes in the clear, under reports/\n", o.out)
		return o, false
	}
	o.out = filepath.Join(scratch, "creator-run")
	return o, true
}

// knowledgeBuildReport is create's and add's --json shape: dir for a
// plain pack directory, kept for a pack kept in the store.
type knowledgeBuildReport struct {
	Dir       string              `json:"dir,omitempty"`
	Kept      *knowledgeEntryJSON `json:"kept,omitempty"`
	Manifest  knowledge.Manifest  `json:"manifest"`
	Documents []string            `json:"documents"`
	Skipped   []string            `json:"skipped,omitempty"`
	Written   []string            `json:"written"`
	Dropped   []knowledgeDropJSON `json:"dropped,omitempty"`
	Receipts  string              `json:"receipts,omitempty"`
	Decode    string              `json:"decode,omitempty"`
	Stopped   string              `json:"stopped,omitempty"`
}

type knowledgeDropJSON struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Kept   bool   `json:"kept,omitempty"`
}

// printKnowledgeBuild says what a build made, in dir or kept in the store:
// text, or one --json document.
func printKnowledgeBuild(out, errOut io.Writer, dir, outDir string, res *build.Result, kept *store.Entry) {
	m := res.Applied.Manifest
	if kept != nil {
		m = kept.Manifest
	}
	if flags.jsonOut {
		r := knowledgeBuildReport{Dir: dir, Manifest: m, Documents: res.Documents, Skipped: res.Skipped,
			Written: res.Applied.Written}
		if outDir != "" {
			r.Receipts = filepath.Join(outDir, "receipts")
		}
		if kept != nil {
			k := entryJSON(*kept)
			r.Kept = &k
		}
		for _, d := range res.Applied.Dropped {
			r.Dropped = append(r.Dropped, knowledgeDropJSON{Path: d.Path, Reason: d.Reason, Kept: d.Kept})
		}
		if res.Decoded != nil {
			r.Decode = res.Decoded.Line()
		}
		if res.RunErr != nil {
			r.Stopped = res.RunErr.Error()
		}
		printJSON(out, r)
		return
	}
	where, attach := "in "+dir, dir
	if kept != nil {
		where, attach = "kept on this computer, encrypted to "+kept.KeyHolder(), m.Name
	}
	_, _ = fmt.Fprintf(out, "knowledge pack %s: %s, %d bytes, %s, %s — %s\n",
		m.Name, knowledge.CountPages(m.Pages), m.Bytes, m.SHA256, m.Scan.Describe(), where)
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
	if outDir != "" {
		_, _ = fmt.Fprintf(out, "  receipts: %s\n", filepath.Join(outDir, "receipts"))
	} else {
		_, _ = fmt.Fprintln(out, "  receipts: removed with the Creator run's output, which held the pages in the clear (--out DIR keeps them)")
	}
	_, _ = fmt.Fprintf(errOut, "attach it to a run with: agentry run <ref> --knowledge %s\n", attach)
}

// keptPackNamed says whether arg is a pack name the store keeps: a name,
// not a path, found there. A store that cannot be read answers no, and the
// caller's own lookup says why.
func keptPackNamed(cmd *cobra.Command, arg string) bool {
	if knowledgeArgIsPath(arg) || knowledge.ValidName(arg) != nil {
		return false
	}
	s, err := knowledgeStore(cmd)
	if err != nil {
		return false
	}
	_, err = s.Find(arg)
	return err == nil
}
