// `agentry draft` (plan builder-rework.md §7.1–§7.5, D1, D15, D22, D25;
// agentry-cli#345): the built-in Drafter, run on this computer against the
// runner policy's model with the person's own keys, for no credits; or,
// with --hosted, as a sandbox host runs it for a draft made in Agentry
// (M5), with the job's credential and the registry's scan and search.
//
// The order is the plan's: the description is read by AI Review first, and
// the Drafter does not start unless it passes; the Drafter's web tools have
// every result read by AI Review before its model does; its answer, held to
// output.schema.json by the runner, has its mechanical slips corrected, is
// rendered into a package and checked by the same code as `agentry
// validate`; a draft the rules still refuse gets one repair turn
// (builder-simplify.md §5.2, agentry-cli#417), and the problems of the
// answer kept are written into draft.json rather than refusing the draft.

package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/termtext"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// envJobToken carries a hosted draft's job credential to `agentry draft
// --hosted` (minted by the runs extension, M5, agentry-enterprise#183).
const envJobToken = "AGENTRY_JOB_TOKEN"

type draftOpts struct {
	hints           []string
	descriptionFile string
	out             string
	policyPath      string
	hosted          bool
	timeout         int
	maxTurns        int
	maxCost         float64
	// outputFormat json is the form a sandbox host drives for a hosted
	// draft (agentry-cli#384): the Drafter run's envelope on stdout, with
	// the draft's report under draft, and its usage narrated on stderr.
	outputFormat string
	// stage is where a draft that fails before the Drafter's run writes an
	// envelope stopped, for that form's failure envelope.
	stage string
}

// draftDocker is the docker client a draft uses; a seam for tests.
var draftDocker = func() dockerx.Client { return dockerx.New() }

// draftRun runs the Drafter; a seam for the command's tests, which check
// what is said, refused and written around a run.
var draftRun = func(cmd *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
	return r.Execute(cmd.Context(), draftDocker())
}

// draftScanner builds the description's scanner; a seam for tests.
var draftScanner = func(w *engine.WebRun) (webscan.Scanner, error) {
	if w.Scan == toolsrv.WebScanRegistry {
		return &webscan.Registry{Base: w.Registry, JobToken: w.JobToken}, nil
	}
	return drafter.CoreJev(w.JevKey)
}

func newDraftCmd() *cobra.Command {
	var o draftOpts
	cmd := &cobra.Command{
		Use:   `draft "<description>" [--hint TEXT…] [--out DIR]`,
		Short: "Draft a QuickAgent from a description, with the built-in Drafter",
		Long: "Draft a QuickAgent from a plain description of what it should do. The built-in Drafter searches the\n" +
			"registry and the web, reads what it finds, and returns a draft: the manifest, the instructions, notes on\n" +
			"what it chose and why, the published QuickAgents that already do the task, its sources, and what it\n" +
			"could not settle. It never submits anything.\n\n" +
			"AI Review (TypeSafe's Jev) reads the description before the Drafter starts, and every search result,\n" +
			"page and registry answer before the Drafter's model does; what it flags, or cannot scan, is withheld,\n" +
			"and the draft lists it. On this computer that takes your own Jev key ($TYPESAFE_API_KEY, or the\n" +
			"policy's judge block), and the Drafter does not run without one. It runs against the model your runner\n" +
			"policy names, for no credits. Web search needs a search provider of your own in the policy's webSearch\n" +
			"block (brave, with $BRAVE_API_KEY, or youcom, with $YDC_API_KEY); without one the Drafter reads pages\n" +
			"and the registry only.\n\n" +
			"The description, the searches and the pages go to the search provider, to TypeSafe and to the sites\n" +
			"read, so the description should hold no secret.\n\n" +
			"The draft is written to DIR/draft.json, and rendered as a package in DIR/package, checked with the same\n" +
			"rules as `agentry validate`. Its mechanical slips (a value named twice, an empty part, a list or a line\n" +
			"past its limit) are corrected first and named in the notes. If the rules still refuse it, the Drafter\n" +
			"gets one repair turn, without its web tools, given only their report and its own draft; the second\n" +
			"answer is kept, with any problems in draft.json. Review every part before you publish it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return draftRunE(cmd, args, &o) },
	}
	f := cmd.Flags()
	f.StringArrayVar(&o.hints, "hint", nil, "a hint: the services or sites it works with, whether it runs on its own or in a conversation, an example of the output wanted (repeatable)")
	f.StringVar(&o.descriptionFile, "description-file", "", "read the description from this file instead of the argument")
	f.StringVar(&o.out, "out", "", "output folder for the draft, its package and the run's receipts (default ./agentry-out/agentry-drafter-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&o.hosted, "hosted", false, "draft as a sandbox host will for a draft made in Agentry (the hosted draft kind, blaktron/agentry-enterprise#183, with --output-format json): scans and search through the registry with the job's credential in $"+envJobToken)
	f.IntVar(&o.timeout, "timeout", 0, "the Drafter run's wall-clock budget in seconds (default: the policy's)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the Drafter run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the Drafter run once the harness reports this much cost in USD (default: the policy's)")
	f.StringVar(&o.outputFormat, "output-format", "text", "stdout format: text | json (json = the Drafter run's result envelope, as `agentry run` writes one, with the draft's report under draft; the harness's usage is narrated on stderr)")
	return cmd
}

// draftRunE checks the output format first and, in its json form, writes
// the envelope of a draft that failed before the Drafter's run wrote one,
// as `agentry run` and `agentry knowledge` do.
func draftRunE(cmd *cobra.Command, args []string, o *draftOpts) error {
	if o.outputFormat != "text" && o.outputFormat != "json" {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("unknown --output-format %q (want text or json)", o.outputFormat))
	}
	o.stage = receipt.StagePolicy
	err := runDraft(cmd, args, o)
	var printed *envelopePrinted
	if errors.As(err, &printed) {
		return printed.err
	}
	if err != nil && o.outputFormat == "json" {
		// A description AI Review did not pass is exit 11 at stage
		// description, so a driver can say why nothing ran without
		// keeping the error's text.
		printBuiltinFailure(cmd, localrun.Drafter().Ref(), o.stage, err)
	}
	if err != nil && cmd.Context().Err() != nil {
		// The envelope says interrupted under the cli class; so does the
		// exit (a scan cut short by the signal reads as not scanned).
		return engine.Classify(engine.ExitCLI, fmt.Errorf("interrupted: %w", err))
	}
	return err
}

func runDraft(cmd *cobra.Command, args []string, o *draftOpts) error {
	errOut := cmd.ErrOrStderr()
	description, err := draftDescription(args, *o)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	task, err := drafter.Task(description, o.hints)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
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
	web, searchNote, err := draftWeb(pol, *o)
	if err != nil {
		return err
	}

	// AI Review reads the description first; nothing else happens unless
	// it passes (D22).
	sc, err := draftScanner(web)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("AI Review cannot scan with this key: %w", err))
	}
	o.stage = receipt.StageDescription
	desc, err := drafter.Prescan(cmd.Context(), sc, task)
	if err != nil {
		return err
	}
	o.stage = receipt.StagePrepare
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	if o.hosted {
		_, _ = fmt.Fprintf(errOut, "AI Review passed the description. The Drafter runs as a hosted draft against %s; the registry scans\n"+
			"every search result, page and registry answer before the model reads it.\n", model)
	} else {
		_, _ = fmt.Fprintf(errOut, "AI Review passed the description. The Drafter runs on this computer against the model your runner policy\n"+
			"names (%s), for no credits. %s Every result, like the description, goes to AI Review (TypeSafe's Jev)\n"+
			"with your key before the model reads it, and this computer reads each page from its site. It submits nothing.\n", model, searchNote)
	}

	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir := cmp.Or(o.out, filepath.Join("agentry-out", "agentry-drafter-"+runID))
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(outDir, "draft.json")); err == nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already holds a draft; name another folder with --out", outDir))
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
	first := localrun.Run{
		Builtin: localrun.Drafter(), Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Task: strings.NewReader(task), OutDir: outDir, Budgets: &budgets,
		RunID: runID, Stderr: errOut, Web: web, OnUsage: usageReporter(o.outputFormat, errOut),
	}
	env, runErr := draftRun(cmd, first)
	var d *drafter.Draft
	var ce *engine.ClassifiedError
	if runErr != nil && env != nil && errors.As(runErr, &ce) && ce.Code == engine.ExitOutputSchema && cmd.Context().Err() == nil {
		// The last message was not a draft: its repair turn is given the
		// error and that message back, and a draft it returns is kept.
		d, env = repairDraft(cmd, first, drafter.Unparsed(env, outDir, desc, localrun.Drafter().Ref(), version.Build(), runErr), env)
		if d.IsRepaired() {
			// The draft is the repair turn's answer, so the run stands as
			// that turn ended; the usage stays both turns'.
			runErr = nil
			env.Status, env.ExitCode, env.ExitClass, env.Error, env.Stage = "ok", engine.ExitOK, receipt.ExitClassName(engine.ExitOK), "", ""
		}
	}
	if runErr != nil {
		if errors.As(runErr, &ce) && ce.Code == engine.ExitOutputSchema {
			_, _ = fmt.Fprintf(errOut, "The Drafter's answer was not a draft, so none was written; its answer and the run's receipts are in %s.\n", outDir)
		}
		if o.outputFormat == "json" && env != nil {
			printDraftEnvelope(cmd, env, outDir, nil, nil)
			return &envelopePrinted{err: runErr}
		}
		return runErr
	}
	err = nil
	if d == nil {
		if d, err = finishDraft(env, outDir, desc); err == nil {
			d, env = repairDraft(cmd, first, d, env)
		}
	}
	if err == nil {
		err = writeDraft(outDir, d)
	}
	if o.outputFormat == "json" {
		printDraftEnvelope(cmd, env, outDir, d, err)
		if err == nil && cmd.Context().Err() != nil {
			err = engine.Classify(engine.ExitCLI, errors.New("interrupted"))
		}
		if err != nil {
			return &envelopePrinted{err: err}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), d)
	}
	printDraft(cmd.OutOrStdout(), outDir, d)
	return nil
}

// draftReport is what the json form's envelope says of the draft, under
// draft: where it is and how it stands, never a word the Drafter wrote
// (the draft itself is draft.json, in the output folder).
type draftReport struct {
	File          string `json:"file"`
	Rendered      bool   `json:"rendered"`
	Passed        bool   `json:"passed"`
	ManifestError bool   `json:"manifestError"`
	Matches       int    `json:"matches"`
	OpenQuestions int    `json:"openQuestions"`
	Withheld      int    `json:"withheld"`
	// Corrections counts the slips the post-step corrected without the
	// model; Repaired says the draft is the Drafter's second answer, after
	// its repair turn.
	Corrections int  `json:"corrections"`
	Repaired    bool `json:"repaired"`
}

func draftReportOf(outDir string, d *drafter.Draft) draftReport {
	r := draftReport{File: filepath.Join(outDir, "draft.json"), ManifestError: d.ManifestError != "",
		Matches: len(d.Matches), OpenQuestions: len(d.OpenQuestions), Rendered: isDir(filepath.Join(outDir, "package")),
		Corrections: len(d.Corrections()), Repaired: d.IsRepaired()}
	var v validateOutput
	r.Passed = d.ManifestError == "" && json.Unmarshal(d.Validation, &v) == nil && v.Passed
	if a := d.AIReview; a != nil {
		r.Withheld = len(a.WithheldSources)
	}
	return r
}

// printDraftEnvelope is the json form of a draft the Drafter ran in: its
// run's envelope with the draft's report under draft (none for a run that
// failed), and its result left empty — the answer is the draft, in
// draft.json and the run's own result.json, so nothing the Drafter wrote
// rides on stdout.
func printDraftEnvelope(cmd *cobra.Command, run *receipt.Envelope, outDir string, d *drafter.Draft, finishErr error) {
	env := *run
	env.Result = receipt.TextResult("")
	var report any
	if d != nil {
		report = draftReportOf(outDir, d)
	}
	printBuiltinEnvelope(cmd.Context(), cmd.OutOrStdout(), env, "draft", report, finishErr)
}

// draftDescription is the argument or the file, exactly one.
func draftDescription(args []string, o draftOpts) (string, error) {
	switch {
	case len(args) == 1 && o.descriptionFile != "":
		return "", errors.New("give the description as the argument or with --description-file, not both")
	case len(args) == 1:
		return args[0], nil
	case o.descriptionFile != "":
		b, err := os.ReadFile(o.descriptionFile) //nolint:gosec // the person names the file
		if err != nil {
			return "", fmt.Errorf("read the description: %w", err)
		}
		return string(b), nil
	}
	return "", errors.New(`describe what the QuickAgent should do: agentry draft "<description>"`)
}

// draftWeb is the run's web configuration: the person's own keys, or, with
// --hosted, the job's credential and the registry. It refuses a draft that
// could not have AI Review read everything first (D22, D25), and says what
// search the draft has.
func draftWeb(pol *policy.Policy, o draftOpts) (*engine.WebRun, string, error) {
	reg := registryURL()
	if o.hosted {
		tok := strings.TrimSpace(os.Getenv(envJobToken))
		if tok == "" {
			return nil, "", engine.Classify(engine.ExitPolicy, fmt.Errorf("a hosted draft runs with the job's credential in $%s, and it is not set", envJobToken))
		}
		return &engine.WebRun{Scan: toolsrv.WebScanRegistry, Search: toolsrv.WebScanRegistry, Registry: reg, JobToken: tok},
			"The registry searches the web.", nil
	}
	w, search, err := drafter.LocalWeb(pol, reg)
	switch {
	case errors.Is(err, drafter.ErrNoJevKey):
		return nil, "", engine.Classify(engine.ExitPolicy, fmt.Errorf(
			"the Drafter runs only with AI Review reading everything first: the description, every search result, every page. "+
				"On this computer that takes your own TypeSafe Jev key, and none was found. Set $%s, or name another variable in "+
				"your runner policy's judge.apiKeyEnv; with judge.apiKeySource: keyring it is read from the OS keychain entry %s",
			pol.Judge.KeyEnv(), modelkey.Account(policy.JudgeKeyProvider)))
	case err != nil:
		return nil, "", err
	}
	switch search {
	case drafter.SearchOn:
		return w, fmt.Sprintf("Its searches go to %s with your key.", pol.WebSearch.ProviderName()), nil
	case drafter.SearchNoKey:
		return w, fmt.Sprintf("It has no web search: no %s key in $%s.", pol.WebSearch.ProviderName(), pol.WebSearch.KeyEnv()), nil
	}
	return w, "It has no web search (name a provider in your runner policy's webSearch block), so it reads the registry and pages it knows.", nil
}

// finishDraft is the post-step over the Drafter's answer (plan §7.5;
// builder-simplify.md §5.2 steps 1 and 2): the answer as a draft with AI
// Review's record and its mechanical slips corrected (drafter.Finish),
// rendered as a package and checked as `agentry validate` checks one. A
// draft that fails the rules is kept, with its problems.
func finishDraft(env *receipt.Envelope, outDir string, desc webscan.Result) (*drafter.Draft, error) {
	d, err := drafter.Finish(env, outDir, desc, localrun.Drafter().Ref(), version.Build())
	if err != nil {
		return nil, err
	}
	checkDraft(d, filepath.Join(outDir, "package"))
	return d, nil
}

// checkDraft renders a draft into pkg and checks it, when its answer let it
// be rendered at all.
func checkDraft(d *drafter.Draft, pkg string) {
	if d.ManifestError == "" {
		d.ManifestError, d.Validation = renderAndValidate(d, pkg)
	}
}

// writeDraft writes outDir/draft.json.
func writeDraft(outDir string, d *drafter.Draft) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "draft.json"), append(b, '\n'), 0o644); err != nil { //nolint:gosec // the person's own output folder
		return engine.Classify(engine.ExitCLI, fmt.Errorf("write draft.json: %w", err))
	}
	return nil
}

// The repair turn's own caps (plan builder-simplify.md §5.2 step 3): it
// reads nothing and writes one manifest, and at most the instruction file,
// so it needs a few minutes and a few tool calls at most. It also gets no
// more than the draft's budgets left after the first turn (repairBudgets).
const (
	repairTimeoutSeconds = 300
	repairMaxTurns       = 8
)

// repairDraft is the post-step's third step (§5.2 step 3): a draft the rules
// still refuse gets one more turn of the Drafter, without its web tools,
// whose task is the rules' report on it and the draft itself
// (drafter.RepairTask), and nothing else. The second answer is corrected and
// checked like the first and kept whether or not it passes; a turn that
// returns no draft leaves the first, with its problems and a note saying
// why. The envelope that comes back carries both turns' usage, and the live
// usage lines the repair turn narrates count from the first turn's total,
// so a sandbox host meters the draft as a whole.
func repairDraft(cmd *cobra.Command, first localrun.Run, d *drafter.Draft, env *receipt.Envelope) (*drafter.Draft, *receipt.Envelope) {
	report, ok := repairReport(d)
	if !ok || cmd.Context().Err() != nil {
		return d, env
	}
	errOut := cmd.ErrOrStderr()
	task, err := drafter.RepairTask(d, report)
	if err != nil {
		d.NoteNotRepaired("its task could not be written")
		return d, env
	}
	budgets, ok := repairBudgets(*first.Budgets, env.Usage)
	if !ok {
		d.NoteNotRepaired("the first turn had spent the draft's budget")
		return d, env
	}
	runID, err := engine.NewRunID()
	if err != nil {
		d.NoteNotRepaired("it could not be given a run id")
		return d, env
	}
	_, _ = fmt.Fprintf(errOut, "The rules refused the draft (%d problem(s)). The Drafter has one repair turn, given only their report and its own draft, without its web tools.\n", len(report))
	spent := env.Usage
	onUsage := first.OnUsage
	if onUsage != nil {
		onUsage = func(u receipt.Usage) { first.OnUsage(addUsage(spent, u)) }
	}
	dir := filepath.Join(first.OutDir, "repair")
	renv, rerr := draftRun(cmd, localrun.Run{
		Builtin: first.Builtin.WithoutWebTools(), Home: first.Home, Policy: first.Policy, PolicySHA: first.PolicySHA,
		PolicyFile: first.PolicyFile, Build: first.Build, Task: strings.NewReader(task), OutDir: dir, Budgets: &budgets,
		RunID: runID, Stderr: errOut, OnUsage: onUsage,
	})
	total := *env
	if renv != nil {
		total.Usage = addUsage(spent, renv.Usage)
	}
	if rerr != nil {
		why := "its run did not finish"
		var ce *engine.ClassifiedError
		if errors.As(rerr, &ce) {
			why = fmt.Sprintf("its run ended with exit %d", ce.Code)
		}
		d.NoteNotRepaired(why)
		return d, &total
	}
	rd, err := drafter.Repaired(d, renv, dir, report)
	if err != nil {
		d.NoteNotRepaired("its answer was not a draft")
		return d, &total
	}
	pkg := filepath.Join(first.OutDir, "package")
	if err := os.RemoveAll(pkg); err != nil {
		d.NoteNotRepaired("the first answer's package could not be replaced")
		return d, &total
	}
	checkDraft(rd, pkg)
	return rd, &total
}

// repairReport is what the rules said of a draft that did not pass, one
// line each, in their own words; ok is false for a draft that passed, or
// one whose check could not run for a reason of this computer's rather than
// the draft's.
func repairReport(d *drafter.Draft) ([]string, bool) {
	if d.Validation == nil {
		if d.ManifestError == "" || strings.HasPrefix(d.ManifestError, checkNotRun) {
			return nil, false
		}
		return []string{"the draft: " + d.ManifestError}, true
	}
	var v validateOutput
	if err := json.Unmarshal(d.Validation, &v); err != nil || v.Passed {
		return nil, false
	}
	var out []string
	for _, is := range v.Issues {
		if is.Severity == "error" || v.Strict {
			out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s: %s", is.Rule, is.Path, is.Message)))
		}
	}
	for _, f := range v.Inventory {
		if f.Severity != "info" {
			out = append(out, fmt.Sprintf("%s %s: %s", f.Scanner, f.FilePath, f.Title))
		}
	}
	for _, f := range v.Findings {
		line := fmt.Sprintf("%s %s %s", f.Scanner, f.RuleID, f.FilePath)
		if f.LineStart > 0 {
			line += fmt.Sprintf(" line %d", f.LineStart)
		}
		line += ": " + f.Title
		if f.Excerpt != "" {
			line += fmt.Sprintf(" (%q)", f.Excerpt)
		}
		out = append(out, line)
	}
	for _, e := range v.ReadErrors {
		out = append(out, "read: "+e)
	}
	return out, len(out) > 0
}

// repairBudgets is the repair turn's budgets: what the first turn left of
// the draft's b, after it spent spent, under the repair turn's own caps; ok
// is false when the first turn left none of the time, the tool calls or
// the cost.
func repairBudgets(b spec.Budgets, spent receipt.Usage) (spec.Budgets, bool) {
	r := b
	r.TimeoutSeconds = repairTimeoutSeconds
	if b.TimeoutSeconds > 0 {
		left := b.TimeoutSeconds - int(spent.WallMS/1000)
		if left <= 0 {
			return r, false
		}
		r.TimeoutSeconds = min(left, repairTimeoutSeconds)
	}
	r.MaxTurns = repairMaxTurns
	if b.MaxTurns > 0 {
		left := b.MaxTurns - spent.Turns
		if left <= 0 {
			return r, false
		}
		r.MaxTurns = min(left, repairMaxTurns)
	}
	if b.MaxCostUSD > 0 {
		left := b.MaxCostUSD - spent.CostUSD
		if left <= 0 {
			return r, false
		}
		r.MaxCostUSD = left
	}
	return r, true
}

// addUsage is two turns' usage as one draft's.
func addUsage(a, b receipt.Usage) receipt.Usage {
	return receipt.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens: a.TotalTokens + b.TotalTokens, CostUSD: a.CostUSD + b.CostUSD, WallMS: a.WallMS + b.WallMS,
		Turns: a.Turns + b.Turns, UserTurns: a.UserTurns + b.UserTurns}
}

// checkNotRun opens a ManifestError that is this computer's, not the
// draft's: the package could not be written, or validate could not run.
const checkNotRun = "the draft could not be checked: "

// renderAndValidate renders the draft into pkg and checks it with
// validate's own code: the report, or why there is none.
func renderAndValidate(d *drafter.Draft, pkg string) (string, json.RawMessage) {
	if _, err := drafter.Render(d, pkg); err != nil {
		// A manifest that does not parse is rendered all the same, for the
		// person to fix; validate would stop at the same error. A folder
		// that could not be written is this computer's failure.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return checkNotRun + err.Error(), nil
		}
		return err.Error(), nil
	}
	report, err := validateWorkspace(io.Discard, io.Discard, pkg, false)
	if err != nil {
		return checkNotRun + err.Error(), nil
	}
	b, err := json.Marshal(report)
	if err != nil {
		return err.Error(), nil
	}
	return "", b
}

// printDraft says where the draft is and what it holds. What the Drafter
// wrote is agent-written text: every line of it is cleaned of terminal
// control characters before it is printed.
func printDraft(w io.Writer, outDir string, d *drafter.Draft) {
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintln(w, termtext.Safe(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
	}
	pkg := filepath.Join(outDir, "package")
	say("Draft written: %s", filepath.Join(outDir, "draft.json"))
	var v validateOutput
	switch {
	case d.ManifestError != "" && isDir(pkg):
		say("  package:   %s holds the draft, which does not pass yet: %s", pkg, safeTerminalText(d.ManifestError, 400))
	case d.ManifestError != "":
		say("  package:   not rendered: %s", safeTerminalText(d.ManifestError, 400))
	case json.Unmarshal(d.Validation, &v) == nil && v.Passed:
		say("  package:   %s passes agentry validate", pkg)
	default:
		say("  package:   %s does not pass agentry validate yet (%d error(s), %d inventory finding(s), %d scan finding(s)):",
			pkg, v.Counts.Errors, v.Counts.InventoryFailing, v.Counts.Findings)
		for i, is := range v.Issues {
			if i == 5 {
				say("             … and %d more in draft.json", len(v.Issues)-5)
				break
			}
			say("             %s %s", is.Rule, safeTerminalText(is.Message, 200))
		}
	}
	if n := len(d.Corrections()); n > 0 {
		say("  corrected: %d slip(s) without the model, each named in the notes", n)
	}
	if d.IsRepaired() {
		say("  repaired:  the rules refused the first answer; this is the Drafter's second, after one repair turn")
	}
	for _, m := range d.Matches {
		say("  registry:  %s already does some of this: %s", safeTerminalText(m.Ref, 120), safeTerminalText(m.Why, 300))
	}
	if a := d.AIReview; a != nil {
		say("  AI Review: read %d result(s); %d withheld", a.Read, a.Withheld+a.NotScanned)
		for _, s := range a.WithheldSources {
			say("             withheld %s: %s", safeTerminalText(s.URL, 200), s.Reason)
		}
	}
	if n := len(d.OpenQuestions); n > 0 {
		say("  open:      %d question(s) the description leaves open, in draft.json", n)
	}
	say("Review every part before you publish it: the contract, the instructions and the notes.")
}
