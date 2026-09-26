// `agentry draft` (plan builder-rework.md §7.1–§7.5, D1, D15, D22, D25;
// agentry-cli#345): the built-in Drafter, run on this computer against the
// runner policy's model with the person's own keys, for no credits; or,
// with --hosted, as a sandbox host runs it for a draft made in Agentry
// (M5), with the job's credential and the registry's scan and search.
//
// The order is the plan's: the description is read by AI Review first, and
// the Drafter does not start unless it passes; the Drafter's web tools have
// every result read by AI Review before its model does; its answer, held to
// output.schema.json by the runner, is rendered into a package and checked
// by the same code as `agentry validate`, and the problems are written into
// draft.json rather than refusing the draft.

package main

import (
	"cmp"
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
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// EnvJobToken carries a hosted draft's job credential to `agentry draft
// --hosted` (minted by the runs extension, M5, agentry-enterprise#183).
const EnvJobToken = "AGENTRY_JOB_TOKEN"

type draftOpts struct {
	hints           []string
	descriptionFile string
	out             string
	policyPath      string
	hosted          bool
	timeout         int
	maxTurns        int
	maxCost         float64
	// jevURL and searchURL point the scans and the search at another root
	// (hidden: tests, a person's own proxy).
	jevURL    string
	searchURL string
}

// draftDocker is the docker client a draft uses; a seam for tests.
var draftDocker = func() dockerx.Client { return dockerx.New() }

// draftRun runs the Drafter; a seam for the command's tests, which check
// what is said, refused and written around a run.
var draftRun = func(cmd *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
	return r.Execute(cmd.Context(), draftDocker())
}

// draftScanner builds the description's scanner; a seam for tests.
var draftScanner = func(w *engine.WebRun, jevURL string) (webscan.Scanner, error) {
	if w.Scan == toolsrv.WebScanRegistry {
		return &webscan.Registry{Base: w.Registry, JobToken: w.JobToken}, nil
	}
	return drafter.CoreJev(w.JevKey, jevURL, nil)
}

func newDraftCmd() *cobra.Command {
	var o draftOpts
	cmd := &cobra.Command{
		Use:   `draft "<description>" [--hint TEXT…] [--out DIR]`,
		Short: "Draft a BriefAgent from a description, with the built-in Drafter",
		Long: "Draft a BriefAgent from a plain description of what it should do. The built-in Drafter searches the\n" +
			"registry and the web, reads what it finds, and returns a draft: the manifest, the instructions, notes on\n" +
			"what it chose and why, the published BriefAgents that already do the task, its sources, and what it\n" +
			"could not settle. It never submits anything.\n\n" +
			"AI Review (TypeSafe's Jev) reads the description before the Drafter starts, and every search result,\n" +
			"page and registry answer before the Drafter's model does; what it flags, or cannot scan, is withheld,\n" +
			"and the draft lists it. On this computer that takes your own Jev key ($TYPESAFE_API_KEY, or the\n" +
			"policy's judge block), and the Drafter does not run without one. It runs against the model your runner\n" +
			"policy names, for no credits. Web search needs a search provider of your own in the policy's webSearch\n" +
			"block; without one the Drafter reads pages and the registry only.\n\n" +
			"The description, the searches and the pages go to the search provider, to TypeSafe and to the sites\n" +
			"read, so the description should hold no secret.\n\n" +
			"The draft is written to DIR/draft.json, and rendered as a package in DIR/package, checked with the same\n" +
			"rules as `agentry validate`; the problems are in draft.json. Review every part before you publish it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runDraft(cmd, args, o) },
	}
	f := cmd.Flags()
	f.StringArrayVar(&o.hints, "hint", nil, "a hint: the services or sites it works with, whether it runs on its own or in a conversation, an example of the output wanted (repeatable)")
	f.StringVar(&o.descriptionFile, "description-file", "", "read the description from this file instead of the argument")
	f.StringVar(&o.out, "out", "", "output folder for the draft, its package and the run's receipts (default ./agentry-out/agentry-drafter-<runid>)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&o.hosted, "hosted", false, "draft as a sandbox host does for Agentry: scans and search through the registry with the job's credential in $"+EnvJobToken)
	f.IntVar(&o.timeout, "timeout", 0, "the Drafter run's wall-clock budget in seconds (default: the policy's)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the Drafter run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the Drafter run once the harness reports this much cost in USD (default: the policy's)")
	f.StringVar(&o.jevURL, "jev-url", "", "the Jev API root for a local draft's scans (default TypeSafe's)")
	f.StringVar(&o.searchURL, "search-url", "", "the search provider's API root for a local draft (default the provider's)")
	_ = f.MarkHidden("jev-url")
	_ = f.MarkHidden("search-url")
	return cmd
}

func runDraft(cmd *cobra.Command, args []string, o draftOpts) error {
	errOut := cmd.ErrOrStderr()
	description, err := draftDescription(args, o)
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
	web, searchNote, err := draftWeb(pol, o)
	if err != nil {
		return err
	}

	// AI Review reads the description first; nothing else happens unless
	// it passes (D22).
	sc, err := draftScanner(web, o.jevURL)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("AI Review cannot scan with this key: %w", err))
	}
	desc, err := drafter.Prescan(cmd.Context(), sc, task)
	if err != nil {
		return err
	}
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
	env, runErr := draftRun(cmd, localrun.Run{
		Builtin: localrun.Drafter(), Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile,
		Build: version.Build(), Task: strings.NewReader(task), OutDir: outDir, Budgets: &budgets,
		RunID: runID, Stderr: errOut, Web: web,
	})
	if runErr != nil {
		var ce *engine.ClassifiedError
		if errors.As(runErr, &ce) && ce.Code == engine.ExitOutputSchema {
			_, _ = fmt.Fprintf(errOut, "The Drafter's answer was not a draft, so none was written; its answer and the run's receipts are in %s.\n", outDir)
		}
		return runErr
	}
	d, err := finishDraft(env, outDir, desc)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), d)
	}
	printDraft(cmd.OutOrStdout(), outDir, d)
	return nil
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
			return "", err
		}
		return string(b), nil
	}
	return "", errors.New(`describe what the BriefAgent should do: agentry draft "<description>"`)
}

// draftWeb is the run's web configuration: the person's own keys, or, with
// --hosted, the job's credential and the registry. It refuses a draft that
// could not have AI Review read everything first (D22, D25), and says what
// search the draft has.
func draftWeb(pol *policy.Policy, o draftOpts) (*engine.WebRun, string, error) {
	reg := registryURL()
	if o.hosted {
		tok := strings.TrimSpace(os.Getenv(EnvJobToken))
		if tok == "" {
			return nil, "", engine.Classify(engine.ExitPolicy, fmt.Errorf("a hosted draft runs with the job's credential in $%s, and it is not set", EnvJobToken))
		}
		return &engine.WebRun{Scan: toolsrv.WebScanRegistry, Search: toolsrv.WebScanRegistry, Registry: reg, JobToken: tok},
			"The registry searches the web.", nil
	}
	jev, err := personalKey(pol.Judge.KeyEnv(), pol.Judge.APIKeySource, policy.JudgeKeyProvider)
	if err != nil {
		return nil, "", err
	}
	if jev == "" {
		return nil, "", engine.Classify(engine.ExitPolicy, fmt.Errorf(
			"the Drafter runs only with AI Review reading everything first: the description, every search result, every page. "+
				"On this computer that takes your own TypeSafe Jev key, and none was found. Set $%s, or name another variable in "+
				"your runner policy's judge.apiKeyEnv; with judge.apiKeySource: keyring it is read from the OS keychain entry %s",
			pol.Judge.KeyEnv(), modelkey.Account(policy.JudgeKeyProvider)))
	}
	w := &engine.WebRun{Scan: toolsrv.WebScanJev, JevKey: jev, Registry: reg, JevURL: o.jevURL, SearchURL: o.searchURL}
	note := "It has no web search (name a provider in your runner policy's webSearch block), so it reads the registry and pages it knows."
	if pol.WebSearch.On() {
		key, err := personalKey(pol.WebSearch.KeyEnv(), pol.WebSearch.APIKeySource, pol.WebSearch.Provider)
		switch {
		case err != nil:
			return nil, "", err
		case key == "":
			note = fmt.Sprintf("It has no web search: no %s key in $%s.", pol.WebSearch.Provider, pol.WebSearch.KeyEnv())
		default:
			w.Search, w.SearchKey = pol.WebSearch.Provider, key
			note = fmt.Sprintf("Its searches go to %s with your key.", pol.WebSearch.Provider)
		}
	}
	return w, note, nil
}

// personalKey reads one of the person's keys: the variable, else, with the
// keyring source, the keychain entry provider:<provider>. "" is no key; an
// error is a keychain that could not be read.
func personalKey(env, source, provider string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	if source != modelkey.SourceKeyring {
		return "", nil
	}
	present, err := modelkey.Present(provider)
	if err != nil || !present {
		return "", nil
	}
	v, err := modelkey.Lookup(provider)
	if err != nil {
		return "", engine.Classify(engine.ExitPolicy, err)
	}
	return strings.TrimSpace(v), nil
}

// finishDraft is the post-step (plan §7.5): the Drafter's answer as a draft,
// rendered as a package and checked as `agentry validate` checks one, with
// AI Review's record, written to outDir/draft.json. A draft that fails the
// rules is kept, with its problems.
func finishDraft(env *receipt.Envelope, outDir string, desc webscan.Result) (*drafter.Draft, error) {
	d, err := drafter.Parse(env.Result)
	if err != nil {
		return nil, engine.Classify(engine.ExitOutputSchema, err)
	}
	d.Drafter = &drafter.Made{Ref: localrun.Drafter().Ref(), Build: version.Build(), Model: env.Harness.Model, RunID: env.RunID}
	d.AIReview = aiReviewOf(env.Web, desc, filepath.Join(outDir, "receipts", engine.WebLedgerFile))
	pkg := filepath.Join(outDir, "package")
	if _, err := drafter.Render(d, pkg); err != nil {
		d.ManifestError = err.Error()
	} else if report, verr := validateWorkspace(io.Discard, io.Discard, pkg, false); verr != nil {
		d.ManifestError = verr.Error()
	} else if b, merr := json.Marshal(report); merr == nil {
		d.Validation = b
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "draft.json"), append(b, '\n'), 0o644); err != nil { //nolint:gosec // the person's own output folder
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	return d, nil
}

// aiReviewOf is the draft's AI Review record: the receipt's counts, and each
// text the model was not given, from the run's ledger.
func aiReviewOf(w *receipt.Web, desc webscan.Result, ledger string) *drafter.AIReview {
	a := &drafter.AIReview{Description: string(desc.Verdict), Model: desc.Model, QuestionSet: desc.QuestionSet, WithheldSources: []drafter.WithheldSource{}}
	if w != nil {
		a.Scan, a.Read, a.Passed, a.Withheld, a.NotScanned = w.Scan, w.Read, w.Passed, w.Withheld, w.NotScanned
		if w.Model != "" {
			a.Model, a.QuestionSet = w.Model, w.QuestionSet
		}
	}
	b, err := os.ReadFile(ledger) //nolint:gosec // the run's own receipt
	if err != nil {
		return a
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e toolsrv.LedgerEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Refused != "" || e.Search || e.Passed() {
			continue
		}
		a.WithheldSources = append(a.WithheldSources, drafter.WithheldSource{Tool: e.Tool, URL: e.URL, Reason: e.Reason()})
	}
	return a
}

// printDraft says where the draft is and what it holds.
func printDraft(w io.Writer, outDir string, d *drafter.Draft) {
	_, _ = fmt.Fprintf(w, "Draft written: %s\n", filepath.Join(outDir, "draft.json"))
	var v struct {
		Passed bool `json:"passed"`
		Issues []struct {
			Rule    string `json:"rule"`
			Message string `json:"message"`
		} `json:"issues"`
		Counts struct {
			Errors, Warnings, InventoryFailing, Findings int
		} `json:"counts"`
	}
	switch {
	case d.ManifestError != "":
		_, _ = fmt.Fprintf(w, "  package:   not rendered: %s\n", d.ManifestError)
	case json.Unmarshal(d.Validation, &v) == nil && v.Passed:
		_, _ = fmt.Fprintf(w, "  package:   %s passes agentry validate\n", filepath.Join(outDir, "package"))
	default:
		_, _ = fmt.Fprintf(w, "  package:   %s does not pass agentry validate yet (%d error(s), %d scan finding(s)):\n",
			filepath.Join(outDir, "package"), v.Counts.Errors, v.Counts.Findings)
		for i, is := range v.Issues {
			if i == 5 {
				_, _ = fmt.Fprintf(w, "             … and %d more in draft.json\n", len(v.Issues)-5)
				break
			}
			_, _ = fmt.Fprintf(w, "             %s %s\n", is.Rule, is.Message)
		}
	}
	for _, m := range d.Matches {
		_, _ = fmt.Fprintf(w, "  registry:  %s already does some of this: %s\n", m.Ref, m.Why)
	}
	if a := d.AIReview; a != nil {
		_, _ = fmt.Fprintf(w, "  AI Review: read %d result(s); %d withheld\n", a.Read, a.Withheld+a.NotScanned)
		for _, s := range a.WithheldSources {
			_, _ = fmt.Fprintf(w, "             withheld %s: %s\n", s.URL, s.Reason)
		}
	}
	if n := len(d.OpenQuestions); n > 0 {
		_, _ = fmt.Fprintf(w, "  open:      %d question(s) the description leaves open, in draft.json\n", n)
	}
	_, _ = fmt.Fprintln(w, "Review every part before you publish it: the contract, the instructions and the notes.")
}
