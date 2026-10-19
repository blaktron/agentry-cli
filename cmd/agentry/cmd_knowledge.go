// `agentry knowledge`: make and extend knowledge packs with the built-in
// Knowledge Creator, on this machine, against the runner policy's model,
// and keep them sealed in the local store the desktop shares
// (cmd_knowledge_store.go).

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/build"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
)

// knowledgeBuildOpts are create's and add's shared flags.
type knowledgeBuildOpts struct {
	files []string
	// repos and urls are the git repositories (URL, with @ref for one
	// other than the default branch) and the files at web addresses to
	// ingest beside the files; sourceTokens their access tokens, each
	// HOST=ENV_NAME.
	repos, urls, sourceTokens []string
	// brief and briefFile are the brief, what the pack should cover and for
	// whom: a build takes documents, a brief, or both.
	brief, briefFile string
	// web and noWeb are the Search the web choice: on by default with no
	// documents, off with them.
	web, noWeb bool
	// hosted is a sandbox host's build: the web's scans and searches go
	// through the registry with the job's credential. render lets it render
	// a page drawn by scripts: the host dispatched it as a browser job.
	hosted, render bool
	dir            string
	out            string
	policyPath     string
	budgetFlags
	// maxPackMB caps the pack, documents included, in MiB: 1024 (1 GB,
	// knowledge.MaxLocalBytes) for a build on this machine; a sandbox host
	// building In Agentry passes 10, Agentry's cap.
	maxPackMB int
	// outputFormat json is the form a sandbox host drives for a hosted
	// build: the Creator run's envelope on stdout, its usage narrated
	// on stderr.
	outputFormat string
	// stage is where a build that fails before the Creator starts stopped,
	// for that form's failure envelope.
	stage string
	// onUsage narrates the harness's usage on stderr in the json form
	// (usageReporter), nil otherwise.
	onUsage func(receipt.Usage)
}

// knowledgeDocker is the docker client a build uses; a seam for tests.
var knowledgeDocker = func() dockerx.Client { return dockerx.New() }

// creatorBuild is the build itself (pkg/knowledge/build), the step inside
// knowledgeBuilder that starts the Creator; a seam for the tests of what
// runKnowledgeBuild hands it, its budgets above all.
var creatorBuild = build.Build

// knowledgeBuilder runs the Creator over a pack directory; a seam for the
// commands' tests, which check what is kept and said around a build (the
// build itself is pkg/knowledge/build's, tested there).
var knowledgeBuilder = runKnowledgeBuild

// newKnowledgeCmd is `agentry knowledge`: the knowledge packs a run can
// search. create and add drive the built-in Knowledge Creator on this
// machine; the packs are kept sealed in the store under the runner home,
// which the desktop shares, and list, import, upload, download, export and
// delete work on it. Attaching one is `agentry run REF --knowledge NAME`.
func newKnowledgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Make, keep and move the knowledge packs a run can search",
		Long: "A knowledge pack is an LLM wiki — cited pages with an index, and the documents they were compiled\n" +
			"from — that a run searches with the knowledge tools (`agentry run REF --knowledge NAME`). A pack made on\n" +
			"this machine holds up to 1 GB, documents included; Agentry keeps packs of at most 10 MB, so a larger one\n" +
			"stays on this computer. Before the Creator reads them, the documents are decoded on this machine, at most\n" +
			"2,000 files and 30 minutes, and each document's decoded text is cut to 10 MiB.\n" +
			"`create` and `add` run the built-in Knowledge Creator on this machine, against the model your runner\n" +
			"policy names: it costs no credits, and neither its documents nor its pack are scanned. A build has 60 minutes, or\n" +
			"your policy's run timeout when that is longer (--timeout sets it for one build), and no token limit.\n" +
			"A build takes documents, a brief (--brief: what the pack should cover and for whom), or both. Specific\n" +
			"documents make the best knowledge. Without them the Creator researches the web and its own knowledge,\n" +
			"which can be wrong or out of date. --web lets it search (the default for a new pack with no documents),\n" +
			"with AI Review reading every result first: that takes your own TypeSafe key and a search key, as\n" +
			"`agentry draft` does. It reads only pages its searches returned.\n\n" +
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
	f.StringArrayVar(&o.files, "file", nil, "a document or a directory of documents to ingest (repeatable; documents, a brief, or both)")
	f.StringArrayVar(&o.repos, "repo", nil, "a git repository to ingest, by its https address, with @BRANCH, @TAG or @COMMIT for a ref other than the default branch (repeatable, at most 10): its text files are read as documents under repo-<host>-<path>/, within 2,000 files a pack holds; nothing inside .git is fetched")
	f.StringArrayVar(&o.urls, "url", nil, "a file at a web address to ingest, fetched with one https GET and read as a document under its own name (repeatable, at most 20; a page drawn by scripts is read by a build with --web instead)")
	f.StringArrayVar(&o.sourceTokens, "source-token", nil, "the access token for a repository's or an address's host: HOST=ENV_NAME, the name of an environment variable holding it (repeatable; otherwise the key saved for the host in the system keychain, then a prompt on a terminal, then none)")
	f.StringVar(&o.brief, "brief", "", "what the pack should cover and for whom, at most 8 KiB, kept in the pack as sources/brief.md")
	f.StringVar(&o.briefFile, "brief-file", "", "read the brief from this file instead")
	f.BoolVar(&o.web, "web", false, "let the Creator search the web, AI Review reading every result first (the default for a new pack with no --file, --repo or --url; it can raise what a build costs)")
	f.BoolVar(&o.noWeb, "no-web", false, "build without the web (the default with --file, --repo or --url, and for add)")
	f.BoolVar(&o.render, "render", false, "on a sandbox host, render a page drawn by scripts that web_read reads (the host dispatched this build as a browser job); on this computer pages are rendered without it")
	f.BoolVar(&o.hosted, "hosted", false, "build as a sandbox host does for a build made in Agentry (with --output-format json): the web's scans and searches through the registry with the job's credential in $"+envJobToken)
	f.StringVar(&o.out, "out", "", "output folder for the Creator run's reports and receipts (default ./agentry-out/agentry-knowledge-creator-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.IntVar(&o.timeout, "timeout", 0, "the Creator run's wall-clock budget in seconds (default: the policy's or 3600, whichever is longer)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the Creator run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the Creator run once the harness reports this much cost in USD (default: the policy's)")
	f.IntVar(&o.maxPackMB, "max-pack-mb", knowledge.MaxLocalBytes>>20, "the pack's cap in MiB, documents included (at most 1024; Agentry keeps packs of at most 10)")
	f.StringVar(&o.outputFormat, "output-format", "text", "stdout format: text | json (json = the Creator run's result envelope, as `agentry run` writes one, with the build's report under knowledge; the harness's usage is narrated on stderr)")
}

// knowledgeBuildRunE wraps create's and add's work: the output format is
// checked first, and in its json form a build that failed without writing
// an envelope still writes one, as `agentry run` does.
func knowledgeBuildRunE(o *knowledgeBuildOpts, work func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if o.outputFormat != "text" && o.outputFormat != "json" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("unknown --output-format %q (want text or json)", o.outputFormat))
		}
		o.stage = receipt.StagePolicy
		o.onUsage = usageReporter(o.outputFormat, cmd.ErrOrStderr())
		err := work(cmd, args)
		var printed *envelopePrinted
		if err != nil && o.outputFormat == "json" && !errors.As(err, &printed) {
			printBuiltinFailure(cmd, localrun.KnowledgeCreator().Ref(), o.stage, err)
		}
		if errors.As(err, &printed) {
			return printed.err
		}
		return err
	}
}

func newKnowledgeCreateCmd() *cobra.Command {
	var o knowledgeBuildOpts
	cmd := &cobra.Command{
		Use:   "create NAME [--file PATH…] [--brief TEXT]",
		Short: "Make a knowledge pack from documents or a brief, with the built-in Knowledge Creator",
		Long: "Make a knowledge pack named NAME (lowercase letters, digits, '.', '-', '_') from the documents given:\n" +
			"Markdown, text, HTML, CSV, JSON, YAML and source files as they are; PDF, DOCX, ODT, EPUB, RTF, PPTX, XLSX\n" +
			"and PNG, JPEG, TIFF or WebP images as the text decoded from them before the run. The documents themselves\n" +
			"are kept in the pack beside the pages. With a brief and no documents the Creator researches what the brief\n" +
			"asks for, on the web (--no-web to forbid it) and from its model's own knowledge, and cites each. The pack\n" +
			"is kept sealed on this computer; --dir DIR makes it as a plain directory instead, in the clear, which\n" +
			"`agentry knowledge import DIR` can seal later.",
		Args: cobra.ExactArgs(1),
		RunE: knowledgeBuildRunE(&o, func(cmd *cobra.Command, args []string) error {
			if err := knowledge.ValidName(args[0]); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if o.dir == "" {
				return createKept(cmd, args[0], &o)
			}
			if _, err := os.Stat(filepath.Join(o.dir, knowledge.ManifestFile)); err == nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already holds a knowledge pack; extend it with `agentry knowledge add %s --file …`", o.dir, o.dir))
			}
			res, outDir, err := knowledgeBuilder(cmd, o.dir, args[0], &o)
			return finishDirBuild(cmd, o.dir, outDir, res, err, o.outputFormat)
		}),
	}
	knowledgeBuildFlags(cmd, &o)
	cmd.Flags().StringVar(&o.dir, "dir", "", "make the pack as a plain directory here, not kept sealed")
	return cmd
}

func newKnowledgeAddCmd() *cobra.Command {
	var o knowledgeBuildOpts
	cmd := &cobra.Command{
		Use:   "add NAME|DIR [--file PATH…] [--brief TEXT]",
		Short: "Add documents or a brief to a knowledge pack, with the built-in Knowledge Creator",
		Long: "Extend the kept pack NAME (or the pack directory DIR) with the documents or the brief given: the Creator\n" +
			"reads the pack as it stands and rewrites the pages they touch. A build that stopped at a budget continues\n" +
			"this way. A kept pack stays under the key it had.",
		Args: cobra.ExactArgs(1),
		RunE: knowledgeBuildRunE(&o, func(cmd *cobra.Command, args []string) error {
			if keptPackNamed(cmd, args[0]) {
				return addKept(cmd, args[0], &o)
			}
			if knowledgeArgIsPath(args[0]) || isDir(args[0]) {
				if _, err := os.Stat(filepath.Join(args[0], knowledge.ManifestFile)); err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no knowledge pack; make one with `agentry knowledge create NAME --file …`", args[0]))
				}
				res, outDir, err := knowledgeBuilder(cmd, args[0], "", &o)
				return finishDirBuild(cmd, args[0], outDir, res, err, o.outputFormat)
			}
			return addKept(cmd, args[0], &o)
		}),
	}
	knowledgeBuildFlags(cmd, &o)
	return cmd
}

// runKnowledgeBuild runs the Creator over the pack directory dir (a new
// pack named name, or the one there when name is empty) and answers what
// it made and where its reports are. A result with RunErr set is a pack
// the Creator stopped short of finishing, which holds what it wrote.
func runKnowledgeBuild(cmd *cobra.Command, dir, name string, o *knowledgeBuildOpts) (*build.Result, string, error) {
	brief, err := o.readBrief()
	if err != nil {
		return nil, "", err
	}
	srcs, err := o.readSources(cmd)
	if err != nil {
		return nil, "", err
	}
	searchWeb, err := o.searchWeb(name != "")
	if err != nil {
		return nil, "", err
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
	o.stage = receipt.StagePrepare
	// Said before anything is built or read: a pack made here is
	// not scanned, and the person hears it first.
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The Knowledge Creator runs on this machine against the model your runner policy names (%s).\n"+
		"It costs no credits, and neither the documents nor the pack it makes are scanned: the pack is recorded\n"+
		"as \"not scanned\" until a registry scans it.\n", model)
	if len(o.files) == 0 && len(srcs) == 0 {
		_, _ = fmt.Fprintln(errOut, "Warning: no documents. Specific documents make the best knowledge. Without them the Creator researches\n"+
			"the web and its own knowledge, which can be wrong or out of date.")
	}
	web, err := knowledgeWeb(pol, o, searchWeb, errOut)
	if err != nil {
		return nil, "", err
	}

	limit, err := packLimit(o.maxPackMB, o.files, errOut)
	if err != nil {
		return nil, "", err
	}

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
	// The Creator's own budgets (the policy's, with at least 60 minutes),
	// then the command line's. A sandbox host always names its job's caps,
	// so a build made in Agentry never takes the floor.
	budgets := o.over(localrun.KnowledgeCreator().LocalBudgets(pol.Budgets))

	res, err := creatorBuild(cmd.Context(), knowledgeDocker(), build.Options{
		PackDir: dir, Name: name, Documents: o.files, Brief: brief, Sources: srcs, Web: web,
		Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Now: time.Now(), OutDir: outDir, Budgets: &budgets,
		RunID: runID, Stderr: errOut, OnUsage: o.onUsage, MaxBytes: limit, Hosted: o.hosted || hostedProcess(), Render: o.render,
	})
	if err != nil {
		if res != nil && len(res.Skipped) > 0 {
			_, _ = fmt.Fprintf(errOut, "not read (no decoder reads them, or the decoder failed): %s\n", strings.Join(res.Skipped, ", "))
		}
		if res != nil && res.Envelope != nil {
			// The Creator ran: its envelope is the build's record, even
			// of a build that failed after it.
			return res, outDir, err
		}
		return nil, "", err
	}
	return res, outDir, nil
}

// readBrief is the build's brief, from --brief or --brief-file, checked
// with the documents: a build takes documents, a brief, or both, and a
// brief is at most 8 KiB.
func (o *knowledgeBuildOpts) readBrief() (string, error) {
	brief := o.brief
	if o.briefFile != "" {
		if o.brief != "" {
			return "", engine.Classify(engine.ExitUsage, errors.New("name the brief with --brief or --brief-file, not both"))
		}
		f, err := os.Open(o.briefFile)
		if err != nil {
			return "", engine.Classify(engine.ExitUsage, fmt.Errorf("--brief-file: %w", err))
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, build.MaxBriefBytes+1))
		if err != nil {
			return "", engine.Classify(engine.ExitUsage, fmt.Errorf("--brief-file: %w", err))
		}
		brief = string(b)
	}
	if err := build.CheckBrief(o.inputs(), brief); err != nil {
		if errors.Is(err, build.ErrNothingToBuild) {
			err = fmt.Errorf("%w: name documents with --file PATH, a repository with --repo URL or a file's address with --url URL, or say what the pack should cover with --brief", err)
		}
		return "", engine.Classify(engine.ExitUsage, err)
	}
	return strings.TrimSpace(brief), nil
}

// hostCredentialEnv are the variables a sandbox host gives the CLI its
// own credentials in.
func hostCredentialEnv() []string { return []string{envJobToken, envRunGrant} }

// inputs are everything the build was given to read: the files, the
// repositories and the addresses.
func (o *knowledgeBuildOpts) inputs() []string {
	return append(append(append([]string{}, o.files...), o.repos...), o.urls...)
}

// readSources parses --repo and --url and gives each its access token: the
// variable --source-token names for its host, the key saved for the host,
// a prompt on a terminal, or none.
func (o *knowledgeBuildOpts) readSources(cmd *cobra.Command) ([]*sources.Source, error) {
	flags, err := sources.ParseBuildTokenFlags(o.sourceTokens)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	// A sandbox host's own credentials are never a source's token: the
	// fetch step would send them to the source's host.
	for host, env := range flags {
		if slices.Contains(hostCredentialEnv(), env) {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s %s=%s: $%s is this host's own credential, never a source's token", sources.BuildTokenFlag, host, env, env))
		}
	}
	srcs, err := build.ParseSources(o.repos, o.urls)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	if len(srcs) == 0 {
		if len(flags) > 0 {
			return nil, engine.Classify(engine.ExitUsage, errors.New(sources.BuildTokenFlag+" given but the build was given no --repo or --url"))
		}
		return nil, nil
	}
	var ask sources.TokenAsker
	if stdinIsTTY(cmd.InOrStdin()) {
		ask = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	if err := sources.ResolveBuildTokens(srcs, flags, os.Getenv, ask, func(msg string) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", msg)
	}); err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	return srcs, nil
}

// searchWeb is the Search the web choice: --web or --no-web, else on for a
// new pack with no documents (no file, repository or address) and off
// otherwise. A pack being extended holds
// documents already, and the web's searches would carry what the Creator
// read in them to the search provider, which stays off unless asked.
func (o *knowledgeBuildOpts) searchWeb(creating bool) (bool, error) {
	switch {
	case o.web && o.noWeb:
		return false, engine.Classify(engine.ExitUsage, errors.New("choose whether the build searches the web with --web or --no-web, not both"))
	case o.web, o.noWeb:
		return o.web, nil
	}
	return creating && len(o.inputs()) == 0, nil
}

// knowledgeWeb is a build's web configuration, or nil for a build without
// the web, and says which. A sandbox host's build scans and searches
// through the registry with the job's credential, as a hosted draft does.
// On this computer the web takes the person's own Jev key, which every
// result is scanned with, and a search key, as a draft's does; without
// either, or under a sandboxed runtime with no name servers for the tools,
// the build runs without the web and says why, rather than failing.
func knowledgeWeb(pol *policy.Policy, o *knowledgeBuildOpts, search bool, errOut io.Writer) (*engine.WebRun, error) {
	if !search {
		return nil, nil
	}
	if o.hosted {
		w, err := hostedWeb("knowledge build that searches the web")
		if err == nil {
			_, _ = fmt.Fprintln(errOut, "The Creator may search the web: the registry searches, and AI Review reads every result first.")
		}
		return w, err
	}
	without := func(why string) (*engine.WebRun, error) {
		_, _ = fmt.Fprintf(errOut, "The Creator builds without the web: %s.\n", why)
		return nil, nil
	}
	if rt := pol.Docker.Runtime; rt != "" && len(pol.Docker.Resolvers) == 0 {
		return without(fmt.Sprintf("under the %s runtime the web tools need the host's name servers in your runner policy's docker.resolvers", rt))
	}
	w, state, err := drafter.LocalWeb(pol, "")
	switch {
	case errors.Is(err, drafter.ErrNoJevKey):
		return without(fmt.Sprintf("AI Review reads every result first, which takes your own TypeSafe Jev key, and none was found ($%s, or the keychain entry %s)",
			pol.Judge.KeyEnv(), modelkey.Account(policy.JudgeKeyProvider)))
	case err != nil:
		return nil, engine.Classify(engine.ExitPolicy, err)
	case state == drafter.SearchNoProvider:
		return without("your runner policy's webSearch block names no search provider")
	case state == drafter.SearchNoKey:
		return without(fmt.Sprintf("no %s key in $%s", pol.WebSearch.ProviderName(), pol.WebSearch.KeyEnv()))
	}
	_, _ = fmt.Fprintf(errOut, "The Creator may search the web with %s and your key; AI Review reads every result first, with your Jev key.\n"+
		"Searching can raise what a build costs. It searches only for what the documents and the pack do not hold.\n", pol.WebSearch.ProviderName())
	return w, nil
}

// finishDirBuild reports a build into a plain pack directory.
func finishDirBuild(cmd *cobra.Command, dir, outDir string, res *build.Result, err error, format string) error {
	if err != nil {
		return failedAfterRun(cmd, dir, outDir, res, err, format)
	}
	printKnowledgeBuild(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), dir, outDir, res, nil, format)
	return printedOr(stoppedShort(cmd, dir, res), format)
}

// failedAfterRun reports a build that failed once the Creator had run: in
// the json form its envelope, with the report of what the build got to,
// is the record; otherwise the error alone, as before.
func failedAfterRun(cmd *cobra.Command, dir, outDir string, res *build.Result, err error, format string) error {
	if format != "json" || res == nil || res.Envelope == nil {
		return err
	}
	printBuiltinEnvelope(cmd.Context(), cmd.OutOrStdout(), *res.Envelope, "knowledge", buildReport(dir, outDir, res, nil), err)
	return &envelopePrinted{err: err}
}

// printedOr marks err, in the json form, as a build whose envelope is out.
func printedOr(err error, format string) error {
	if err == nil || format != "json" {
		return err
	}
	return &envelopePrinted{err: err}
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
func createKept(cmd *cobra.Command, name string, o *knowledgeBuildOpts) error {
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
	private := privateCreatorOut(cmd, o, scratch)
	dir := filepath.Join(scratch, name)
	res, outDir, err := knowledgeBuilder(cmd, dir, name, o)
	if err != nil {
		kept = true // nothing worth keeping: the scratch goes
		if private {
			outDir = ""
		}
		return failedAfterRun(cmd, "", outDir, res, err, o.outputFormat)
	}
	e, err := s.Keep(cmd.Context(), dir)
	if err != nil {
		return fmt.Errorf("the pack was built but could not be kept sealed (%w); it is in %s, in the clear, until the store's sweep removes it an hour from now — keep it with `agentry knowledge import %s`, or copy it out", err, dir, dir)
	}
	kept = true
	if private {
		outDir = ""
	}
	printKnowledgeBuild(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), "", outDir, res, &e, o.outputFormat)
	return printedOr(stoppedShort(cmd, name, res), o.outputFormat)
}

// addKept opens a kept pack into a 0700 directory inside the store, runs
// the Creator over it, and seals the result back under the same key.
func addKept(cmd *cobra.Command, name string, o *knowledgeBuildOpts) error {
	if _, err := o.readBrief(); err != nil {
		return err
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
	private := privateCreatorOut(cmd, o, scratch)
	dir := filepath.Join(scratch, name)
	if _, err := s.Open(cmd.Context(), name, dir); err != nil {
		return err
	}
	res, outDir, err := knowledgeBuilder(cmd, dir, "", o)
	if err != nil {
		if private {
			outDir = ""
		}
		return failedAfterRun(cmd, "", outDir, res, err, o.outputFormat)
	}
	e, err := s.Reseal(cmd.Context(), name, dir)
	if err != nil {
		return err
	}
	if private {
		outDir = ""
	}
	printKnowledgeBuild(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), "", outDir, res, &e, o.outputFormat)
	if e.Key.Agentry != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Agentry still keeps the copy from before; send this one with `agentry knowledge upload %s`\n", name)
	}
	return printedOr(stoppedShort(cmd, name, res), o.outputFormat)
}

// privateCreatorOut puts the Creator run's output, whose reports/ holds
// every page it wrote, inside the scratch that goes with a kept build,
// unless --out named a place for it, which is then said to hold the pages
// in the clear. private reports which.
func privateCreatorOut(cmd *cobra.Command, o *knowledgeBuildOpts, scratch string) bool {
	if o.out != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the Creator run's output in %s keeps the pages it writes in the clear, under reports/\n", o.out)
		return false
	}
	o.out = filepath.Join(scratch, "creator-run")
	return true
}

// knowledgeBuildReport is create's and add's --json shape: dir for a
// plain pack directory, kept for a pack kept in the store.
type knowledgeBuildReport struct {
	Dir       string                `json:"dir,omitempty"`
	Kept      *knowledgeEntryJSON   `json:"kept,omitempty"`
	Manifest  knowledge.Manifest    `json:"manifest"`
	Documents []string              `json:"documents"`
	Sources   []build.FetchedSource `json:"sources,omitempty"`
	Skipped   []string              `json:"skipped,omitempty"`
	Written   []string              `json:"written"`
	Dropped   []knowledgeDropJSON   `json:"dropped,omitempty"`
	Receipts  string                `json:"receipts,omitempty"`
	Decode    string                `json:"decode,omitempty"`
	Stopped   string                `json:"stopped,omitempty"`
}

type knowledgeDropJSON struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Kept   bool   `json:"kept,omitempty"`
}

// buildReport is what a build made or got to, as --json and the json
// form's knowledge block state it.
func buildReport(dir, outDir string, res *build.Result, kept *store.Entry) knowledgeBuildReport {
	r := knowledgeBuildReport{Dir: dir, Documents: res.Documents, Sources: res.Sources, Skipped: res.Skipped}
	if res.Applied != nil {
		r.Manifest, r.Written = res.Applied.Manifest, res.Applied.Written
		for _, d := range res.Applied.Dropped {
			r.Dropped = append(r.Dropped, knowledgeDropJSON{Path: d.Path, Reason: d.Reason, Kept: d.Kept})
		}
	}
	if kept != nil {
		r.Manifest = kept.Manifest
		k := entryJSON(*kept)
		r.Kept = &k
	}
	if outDir != "" {
		r.Receipts = filepath.Join(outDir, "receipts")
	}
	if res.Decoded != nil {
		r.Decode = res.Decoded.Line()
	}
	if res.RunErr != nil {
		r.Stopped = res.RunErr.Error()
	}
	return r
}

// printKnowledgeBuild says what a build made, in dir or kept in the store:
// text, one --json document, or, in the json output format, the Creator
// run's envelope with that document under knowledge.
func printKnowledgeBuild(ctx context.Context, out, errOut io.Writer, dir, outDir string, res *build.Result, kept *store.Entry, format string) {
	m := res.Applied.Manifest
	if kept != nil {
		m = kept.Manifest
	}
	if format == "json" && res.Envelope != nil {
		printBuiltinEnvelope(ctx, out, *res.Envelope, "knowledge", buildReport(dir, outDir, res, kept), nil)
		return
	}
	if flags.jsonOut {
		printJSON(out, buildReport(dir, outDir, res, kept))
		return
	}
	where, attach := "in "+dir, dir
	if kept != nil {
		where, attach = "kept on this computer, encrypted to "+kept.KeyHolder(), m.Name
	}
	_, _ = fmt.Fprintf(out, "knowledge pack %s: %s, %d bytes, %s, %s — %s\n",
		m.Name, knowledge.CountPages(m.Pages), m.Bytes, m.SHA256, m.Scan.Describe(), where)
	_, _ = fmt.Fprintf(out, "  documents: %s\n", strings.Join(res.Documents, ", "))
	for _, s := range res.Sources {
		_, _ = fmt.Fprintf(out, "  fetched: %s\n", s.Words())
	}
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

// packLimit is the pack's cap from --max-pack-mb, and says first, when the
// documents come to more than Agentry keeps, that a build that size can
// take a very long time and that its pack stays on this computer. A build
// past the cap itself is refused by the build, before anything runs.
func packLimit(maxPackMB int, files []string, errOut io.Writer) (int64, error) {
	limit := int64(maxPackMB) << 20
	if maxPackMB <= 0 || limit > knowledge.MaxLocalBytes {
		return 0, fmt.Errorf("--max-pack-mb %d: a pack's cap is 1 to %d MiB", maxPackMB, knowledge.MaxLocalBytes>>20)
	}
	if total := documentsBytes(files); total > knowledge.MaxBytes && total <= limit {
		_, _ = fmt.Fprintf(errOut, "The documents come to %.1f MB. A build that size can take a very long time, and the pack will stay on\n"+
			"this computer: Agentry keeps packs of at most %s.\n", float64(total)/(1<<20), knowledge.FormatCap(knowledge.MaxBytes))
	}
	return limit, nil
}

// documentsBytes is the size of the regular files the paths name, walking
// directories, for the warning a large build gets; a path it cannot read
// counts nothing here, and the build says why.
func documentsBytes(paths []string) int64 {
	var total int64
	for _, p := range paths {
		_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}
