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
// validate`; AI Review checks the finished draft as the pipeline will at
// publish (§5.2 step 4, agentry-cli#418); a draft the rules still refuse,
// or one the check flagged for something it can fix, gets one repair turn
// (builder-simplify.md §5.2, agentry-cli#417), and is checked again; and
// the problems and verdicts of the answer kept are written into draft.json
// rather than refusing the draft.

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	budgetFlags
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

// draftChecker builds the check of the finished draft: the registry's
// route with the job's credential for a hosted draft, the core's
// pkg/draftcheck with the person's own Jev key on this computer. A seam for
// tests.
var draftChecker = drafter.CheckerFor

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
			"past its limit) are corrected first and named in the notes. AI Review then checks the finished package:\n" +
			"the release rubric, asked as the pipeline asks it at publish, and whether the instructions do the task\n" +
			"described; its verdicts go into draft.json. If the rules still refuse the draft, or the check flagged\n" +
			"something it can fix, the Drafter gets one repair turn, without its web tools, given only their report,\n" +
			"what the check flagged (with the task) and its own draft; the second answer is checked again and kept,\n" +
			"with any problems in draft.json. Review every part before you publish it.\n\n" +
			"On this computer the draft has 30 minutes, or your runner policy's run timeout when that is longer, and\n" +
			"--timeout sets it for one draft. It has no token limit; the tool-call cap is your policy's.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return draftRunE(cmd, args, &o) },
	}
	f := cmd.Flags()
	f.StringArrayVar(&o.hints, "hint", nil, "a hint: the services or sites it works with, whether it runs on its own or in a conversation, an example of the output wanted (repeatable)")
	f.StringVar(&o.descriptionFile, "description-file", "", "read the description from this file instead of the argument")
	f.StringVar(&o.out, "out", "", "output folder for the draft, its package and the run's receipts (default ./agentry-out/agentry-drafter-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&o.hosted, "hosted", false, "draft as a sandbox host will for a draft made in Agentry (the hosted draft kind, blaktron/agentry-enterprise#183, with --output-format json): scans and search through the registry with the job's credential in $"+envJobToken)
	f.IntVar(&o.timeout, "timeout", 0, "the Drafter run's wall-clock budget in seconds (default: the policy's or 1800, whichever is longer; with --hosted, the policy's)")
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
	budgets := o.over(draftBudgets(pol, o.hosted))
	first := localrun.Run{
		Builtin: localrun.Drafter(), Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Task: strings.NewReader(task), OutDir: outDir, Budgets: &budgets,
		RunID: runID, Stderr: errOut, Web: web, OnUsage: usageReporter(o.outputFormat, errOut),
	}
	env, runErr := draftRun(cmd, first)
	checker, checkerErr := draftChecker(web)
	post := drafter.PostStep{
		OutDir: outDir, Description: description, Task: task, Scan: desc, Ref: localrun.Drafter().Ref(), Build: version.Build(),
		Budgets: budgets, Checker: checker, CheckerErr: checkerErr, Log: errOut, OnUsage: first.OnUsage,
		Repair: func(_ context.Context, t drafter.RepairTurn) (*receipt.Envelope, error) {
			return draftRun(cmd, localrun.RepairRun(first, t))
		},
		Rules: loadRulesBundle,
	}
	d, env, err := post.Run(cmd.Context(), env, runErr)
	if runErr != nil && err != nil {
		// The run failed, or its answer was not a draft and its repair turn
		// returned none either.
		var ce *engine.ClassifiedError
		if errors.As(err, &ce) && ce.Code == engine.ExitOutputSchema {
			_, _ = fmt.Fprintf(errOut, "The Drafter's answer was not a draft, so none was written; its answer and the run's receipts are in %s.\n", outDir)
		}
		if o.outputFormat == "json" && env != nil {
			printDraftEnvelope(cmd, env, outDir, nil, nil)
			return &envelopePrinted{err: err}
		}
		return err
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

// draftBudgets are the budgets a draft starts from, before the command
// line's. On this computer they are the Drafter's own
// (localrun.Builtin.LocalBudgets): the runner policy's, with at least 30
// minutes. A hosted draft takes the staged policy's as they are: its caps
// travel in its job and arrive as flags, and this computer's rule is not
// the sandbox host's.
func draftBudgets(pol *policy.Policy, hosted bool) spec.Budgets {
	if hosted {
		return spec.Budgets{TimeoutSeconds: pol.Budgets.TimeoutSeconds, MaxTurns: pol.Budgets.MaxTurns, MaxCostUSD: pol.Budgets.MaxCostUSD}
	}
	return localrun.Drafter().LocalBudgets(pol.Budgets)
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
	// Check is AI Review's check of the finished draft as one word: pass,
	// flagged or not_scanned, unanswered when it has no answer, absent
	// when the draft could not be rendered to check.
	Check string `json:"check,omitempty"`
}

func draftReportOf(outDir string, d *drafter.Draft) draftReport {
	r := draftReport{File: filepath.Join(outDir, "draft.json"), ManifestError: d.ManifestError != "",
		Matches: len(d.Matches), OpenQuestions: len(d.OpenQuestions), Rendered: isDir(filepath.Join(outDir, "package")),
		Corrections: len(d.Corrections()), Repaired: d.IsRepaired()}
	r.Passed = d.Passed()
	if a := d.AIReview; a != nil {
		r.Withheld = len(a.WithheldSources)
		r.Check = a.Check.Word()
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
		switch c := a.Check; c.Word() {
		case "":
		case drafter.CheckUnanswered:
			say("  checked:   no answer: %s", safeTerminalText(c.Unanswered, 300))
		default:
			say("  checked:   the finished draft, %s (release rubric %s, the task %s), in draft.json", c.Word(), c.Rubric.Verdict, c.Draft.Verdict)
		}
		for _, s := range a.WithheldSources {
			say("             withheld %s: %s", safeTerminalText(s.URL, 200), s.Reason)
		}
	}
	if n := len(d.OpenQuestions); n > 0 {
		say("  open:      %d question(s) the description leaves open, in draft.json", n)
	}
	say("Review every part before you publish it: the contract, the instructions and the notes.")
}
