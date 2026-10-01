// `agentry review <dir>` (plan desktop-review-key-and-run-cards.md §6, D2,
// D18, D24, D28, D30; agentry-cli#504): the built-in Examiner, run on this
// computer over a QuickAgent the person built, with their own TypeSafe key
// and the model their runner policy names, for no credits. It is the
// Examiner's first caller; the desktop's Review Your QuickAgents locally is
// the second, over the same pkg/examiner.
//
// The order is the plan's: the pre-step runs the registry's own rules here
// and asks the registry for its rule bundle, its baseline policy and its
// record of each declared server; the Examiner runs over a read-only copy of
// the files a submission would carry, asks AI Review as stage S5 does and
// reads the files for what no client has the registry's code for; the
// post-step writes report.json and the command exits by the outcome the
// gate would be expected to reach.

package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// The exit codes of a review that ran, by the outcome the gate would be
// expected to reach: 0 published, and one code each for the rest, above
// the run's own classes (README "Exit codes").
const (
	exitReviewHeld         = 20
	exitReviewRejected     = 21
	exitReviewRefused      = 22
	exitReviewUndetermined = 23
)

type reviewOpts struct {
	out        string
	policyPath string
	budgetFlags
}

// reviewRun runs the Examiner; a seam for the command's tests.
var reviewRun = func(cmd *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
	return r.Execute(cmd.Context(), draftDocker())
}

// reviewRulesUpdate fetches the registry's current rule bundle into the
// cache loadRulesBundle reads; a seam for tests.
var reviewRulesUpdate = func(cmd *cobra.Command) error {
	_, err := fetchRulesBundle(cmd.Context(), io.Discard, registryURL())
	return err
}

func newReviewCmd() *cobra.Command {
	var o reviewOpts
	cmd := &cobra.Command{
		Use:   "review [dir]",
		Short: "Review a QuickAgent before you submit it, with the built-in Examiner",
		Long: "Review a QuickAgent you built, before you submit it, and see what Agentry's pipeline would be expected\n" +
			"to say about it. The built-in Examiner runs on this computer and submits nothing.\n\n" +
			"First the registry's own rules run here, as `agentry validate` runs them: the manifest rules, the\n" +
			"inventory checks, and the hidden-content and injection rules under the registry's current rule bundle.\n" +
			"The registry is asked for its baseline publication policy and for its record of each MCP server the\n" +
			"QuickAgent declares. Then the Examiner runs over a read-only copy of the files a submission would carry:\n" +
			"it asks AI Review (TypeSafe's Jev) the six questions the registry asks, with your own key, and its model\n" +
			"reads the files for secrets, for scripts that reach past the declared capabilities, and for text aimed\n" +
			"at the review. AI Review reads every file before the model does, and withholds what it flags. Nothing of\n" +
			"the QuickAgent runs.\n\n" +
			"The QuickAgent's text goes to TypeSafe with your key and to the model your runner policy names, for no\n" +
			"credits. A review needs your own TypeSafe Jev key ($TYPESAFE_API_KEY, or the policy's judge block).\n\n" +
			"The report is written to DIR/report.json, with the run's receipts. Its findings come in three kinds:\n" +
			"the registry's rules, AI Review, and the Examiner's own reading. The outcome the gate would be expected\n" +
			"to reach is computed from the first two under the registry's public policy; the Examiner's reading is\n" +
			"never counted. The report lists what did not run here. The registry scans again at Submit, and its\n" +
			"answer is the one that counts.\n\n" +
			"Exit codes, once the review ran: 0 published, 20 held for review, 21 rejected, 22 refused at Submit\n" +
			"(the manifest rules refuse it), 23 not determined here. A run that failed exits with its own class.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runReview(cmd, args, &o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.out, "out", "", "output folder for the report and the run's receipts (default ./agentry-out/agentry-examiner-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.IntVar(&o.timeout, "timeout", 0, "the Examiner run's wall-clock budget in seconds (default: the policy's or 1800, whichever is longer)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the Examiner run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the Examiner run once the harness reports this much cost in USD (default: the policy's)")
	return cmd
}

func runReview(cmd *cobra.Command, args []string, o *reviewOpts) error {
	errOut := cmd.ErrOrStderr()
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.yaml")); err != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no agent.yaml: name the folder of a QuickAgent", dir))
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
	key, err := examiner.LocalReview(pol)
	switch {
	case errors.Is(err, examiner.ErrNoJevKey):
		return engine.Classify(engine.ExitPolicy, fmt.Errorf(
			"a review runs only with your own TypeSafe Jev key: AI Review is asked with it, and reads every file before the Examiner's "+
				"model does. None was found. Set $%s, or name another variable in your runner policy's judge.apiKeyEnv; with "+
				"judge.apiKeySource: keyring it is read from the OS keychain entry %s",
			pol.Judge.KeyEnv(), modelkey.Account(policy.JudgeKeyProvider)))
	case err != nil:
		return err
	}

	if err := reviewRulesUpdate(cmd); err != nil {
		_, _ = fmt.Fprintf(errOut, "The registry's current rule bundle could not be fetched (%v); the newest one this computer has is used, and the report names it.\n",
			safeTerminalText(err.Error(), 300))
	}
	prep, err := examiner.Prepare(cmd.Context(), dir, examiner.Options{Registry: registryURL(), HTTP: httpClient, Rules: loadRulesBundle})
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	defer prep.Cleanup()

	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir := cmp.Or(o.out, filepath.Join("agentry-out", "agentry-examiner-"+runID))
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	// A folder that holds another review's receipts is refused, not only one
	// that holds its report: the post-step reads AI Review from the run's
	// ledger there, and must never read another run's.
	for _, name := range []string{examiner.ReportFile, "receipts"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already holds a review or a run's receipts; name another folder with --out", outDir))
		}
	}
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The registry's rules ran here. The Examiner runs on this computer against the model your runner policy names\n"+
		"(%s), for no credits, over a read-only copy of %d file(s). Their text goes to AI Review (TypeSafe's Jev) with your\n"+
		"key and to that model. Nothing of the QuickAgent runs, and nothing is submitted.\n", model, prep.Files)

	budgets := o.over(localrun.Examiner().LocalBudgets(pol.Budgets))
	env, runErr := reviewRun(cmd, localrun.Run{
		Builtin: localrun.Examiner(), Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Task: strings.NewReader(examiner.Task(prep)), Inputs: prep.Inputs, OutDir: outDir,
		Budgets: &budgets, RunID: runID, Stderr: errOut, Review: key,
	})
	report := examiner.Finish(prep, env, runErr, outDir, localrun.Examiner().Ref(), version.Build())
	path, err := examiner.Write(outDir, report)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if flags.jsonOut {
		if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
	} else {
		printReview(cmd.OutOrStdout(), path, report)
	}
	if runErr != nil {
		return runErr
	}
	if cmd.Context().Err() != nil {
		return engine.Classify(engine.ExitCLI, errors.New("interrupted"))
	}
	switch report.Expected.Outcome {
	case examiner.OutcomePublished:
		return nil
	case examiner.OutcomeNeedsReview:
		return engine.Classify(exitReviewHeld, errors.New("the gate would be expected to hold it for review"))
	case examiner.OutcomeRejected:
		return engine.Classify(exitReviewRejected, errors.New("the gate would be expected to reject it"))
	case examiner.OutcomeRefused:
		return engine.Classify(exitReviewRefused, errors.New("the manifest rules refuse it, so Submit would be expected to refuse it"))
	}
	return engine.Classify(exitReviewUndetermined, errors.New("the outcome is not determined here"))
}

// printReview says what the review found. Everything from the package and
// everything the Examiner wrote is cleaned of terminal control characters
// before it is printed.
func printReview(w io.Writer, path string, r *examiner.Report) {
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintln(w, termtext.Safe(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
	}
	at := func(file string, line int) string {
		file = safeTerminalText(file, 200)
		if line > 0 {
			return fmt.Sprintf("%s:%d", file, line)
		}
		return file
	}
	say("Review written: %s", path)
	say("  expected:  %s. %s", examiner.OutcomeWords(r.Expected.Outcome), r.Expected.Reason)
	source, counted := "the registry's baseline policy", "the registry's rules and AI Review"
	if r.Expected.Policy.Source != examiner.PolicyFromRegistry {
		source = "the baseline policy compiled into this build"
	}
	if !r.AIReview.Ran {
		counted = "the registry's rules alone"
	}
	say("             (under %s, which publishes up to %s; from %s)", source, examiner.AutoPublishUpTo(r.Expected.Policy.Policy), counted)

	errs, warns := 0, 0
	for _, is := range r.RegistryRules.Manifest {
		if is.Severity == "error" {
			errs++
		} else {
			warns++
		}
	}
	say("  registry's rules (bundle %s, %s): %d manifest error(s), %d warning(s), %d finding(s)",
		r.RegistryRules.Bundle.Version, r.RegistryRules.Bundle.Source, errs, warns, len(r.RegistryRules.Findings))
	for _, is := range r.RegistryRules.Manifest {
		say("             %s %s %s", strings.ToUpper(is.Severity), is.Rule, safeTerminalText(is.Message, 200))
	}
	for _, f := range r.RegistryRules.Findings {
		say("             %-8s %s %s — %s", strings.ToUpper(f.Severity), f.Scanner, at(f.File, f.Line), safeTerminalText(f.Title, 200))
	}
	for _, s := range r.RegistryRules.Servers {
		say("             server %s: %s", safeTerminalText(s.Ref, 200), s.Status)
	}

	if a := r.AIReview; a.Ran {
		say("  AI Review (%s, rubric %s): %d finding(s) over %d file(s)", a.Model, a.RubricHash, len(a.Findings), a.Files)
		for _, f := range a.Findings {
			say("             %-8s %s %s — %s", strings.ToUpper(f.Severity), f.Scanner, at(f.File, 0), f.Title)
		}
	} else {
		say("  AI Review: did not answer here: %s", safeTerminalText(a.Failure, 300))
	}

	rd := r.Reading
	say("  the Examiner's own reading (not counted in the outcome):")
	if rd.Error != "" {
		say("             none: %s", safeTerminalText(rd.Error, 300))
	} else {
		say("             %s", safeTerminalText(rd.Summary, 600))
		for _, f := range rd.Findings {
			say("             %s %s — %s", f.Scanner, at(f.File, f.Line), safeTerminalText(f.Title, 200))
		}
	}
	for _, n := range rd.Withheld {
		say("             withheld from its model: %s (%s)", at(n.File, 0), safeTerminalText(n.Why, 200))
	}
	say("  not run here:")
	for _, n := range r.NotRunHere {
		say("             - %s", safeTerminalText(n, 400))
	}
	say("%s", examiner.Notice)
}
