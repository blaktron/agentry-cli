package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/validate"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// The repair turn's own caps (plan builder-simplify.md §5.2 step 3): it
// reads nothing and writes one manifest, and at most the instruction file,
// so it needs a few minutes and a few tool calls at most. It also gets no
// more than the draft's budgets left after the first turn (repairBudgets).
const (
	RepairTimeoutSeconds = 300
	RepairMaxTurns       = 8
)

// PostStep is the post-step over the Drafter's first answer, after
// Prescan and the first turn (plan builder-rework.md §7.5;
// builder-simplify.md §5.2, D6; agentry-cli#478): the answer finished with
// AI Review's record and its mechanical slips corrected (Finish), rendered
// into OutDir/package and checked by `agentry validate`'s own code
// (pkg/validate), AI Review's check of the finished package, one repair
// turn for a draft the rules refuse or the check flagged as not doing the
// task, and the second answer checked again. `agentry draft` and the
// desktop's draft on this computer run it alike, so a draft's validation,
// its repair turn's input and its check are one implementation.
type PostStep struct {
	// OutDir is the first turn's output folder: the package is rendered in
	// OutDir/package and the repair turn writes OutDir/repair.
	OutDir string
	// Description is the person's description, which the check reads; Task
	// is the first turn's task, which a repair turn a check flag sent is
	// also given (AI Review passed it before that turn started).
	Description, Task string
	// Scan is AI Review's verdict on the description (Prescan).
	Scan webscan.Result
	// Ref and Build name the Drafter and the client that ran it.
	Ref, Build string
	// Budgets are the first turn's; the repair turn gets what it left.
	Budgets spec.Budgets
	// Checker asks AI Review's check of the finished draft (CheckerFor).
	// CheckerErr is why CheckerFor could make none; a nil Checker (an
	// untyped nil) is recorded as unanswered, never as a pass.
	Checker    Checker
	CheckerErr error
	// Repair runs the Drafter's one repair turn. It must run the Drafter
	// without its web tools and with no web configuration, so no key and
	// nothing to read: localrun.RepairRun builds that run.
	Repair func(context.Context, RepairTurn) (*receipt.Envelope, error)
	// Rules loads the S4 bundle the package is validated under; nil is the
	// bundle embedded in this binary (validate.Embedded).
	Rules validate.RulesLoader
	// Log takes the line announcing the repair turn; nil discards it.
	Log io.Writer
	// Knowledge is what the first turn was given of the person's
	// knowledge (ReadyKnowledge): recorded in the draft, each withheld
	// file named in its notes. Nil for a draft given none. The repair
	// turn is given none.
	Knowledge *KnowledgeRead
	// Revision is the release a revision was drafted from and the person's
	// two texts (agentry-cli#526): the draft records it in Revises and
	// keeps its name, the first answer and the repair turn's alike. Nil for
	// a new QuickAgent.
	Revision *Revision
	// OnUsage is the live usage reporter the first turn had; the repair
	// turn's lines count on from the first turn's total, so a host's caps
	// see the draft whole. Nil reports none.
	OnUsage func(receipt.Usage)
	// Servers resolves a declared MCP server's pin for the rule on its
	// runtime (checkServers); nil resolves against Registries, the
	// registries the person's runs install from (the runner policy's
	// mcpRegistries), or the default npm and PyPI registries when none are
	// given.
	Servers    ServerResolver
	Registries policy.MCPRegistries

	// serverChecks keeps each pin's resolution for this post-step's run.
	serverChecks map[string]serverCheck
}

// RepairTurn is what the post-step gives the repair turn's run: the task
// (RepairTask), the folder, the budgets the first turn left, a run id of
// its own, and its usage reporter.
type RepairTurn struct {
	Task    string
	OutDir  string
	Budgets spec.Budgets
	RunID   string
	OnUsage func(receipt.Usage)
}

// CheckerFor is the check of the finished draft for a run's web
// configuration: the registry's route with the job's credential for a
// hosted draft, the core's pkg/draftcheck with the person's own Jev key on
// this computer (DEV-150: inside the draft's one AI Review use).
func CheckerFor(w *engine.WebRun) (Checker, error) {
	if w.Scan == toolsrv.WebScanRegistry {
		return &RegistryChecker{Base: w.Registry, JobToken: w.JobToken}, nil
	}
	return CoreCheck(w.JevKey)
}

// Run is the post-step over the first turn's envelope and error. An answer
// the output schema refused (exit 17) is first recovered when it ends in
// a schema-valid object; otherwise it is given the one repair turn with
// that error and its own last message; a draft it returns stands as the
// run's answer, and the envelope is made ok (its usage stays both turns').
// Any other failed run, or a second answer that is still not a draft, comes
// back as runErr with its envelope and no draft. Otherwise the draft comes
// back finished, validated and checked, with the envelope of both turns; an
// error is an answer that was not a draft (Finish).
func (p PostStep) Run(ctx context.Context, env *receipt.Envelope, runErr error) (*Draft, *receipt.Envelope, error) {
	p.serverChecks = map[string]serverCheck{}
	var d *Draft
	recoveredChars := -1
	var ce *engine.ClassifiedError
	if runErr != nil && env != nil && errors.As(runErr, &ce) && ce.Code == engine.ExitOutputSchema && ctx.Err() == nil {
		if recovered, chars, err := recoverAnswer(env); err == nil {
			env, recoveredChars, runErr = recovered, chars, nil
		}
		if runErr != nil {
			u := Unparsed(env, p.OutDir, p.Scan, p.Ref, p.Build, runErr)
			u.readKnowledge(p.Knowledge)
			lines, _ := rulesReport(u)
			d, env = p.repair(ctx, u, env, RepairReport{Rules: lines})
			if d.IsRepaired() {
				runErr = nil
			} else if d.repairFailure != "" {
				runErr = fmt.Errorf("%w; its one repair turn returned no draft (%s)", runErr, d.repairFailure)
				env.Error = runErr.Error()
			}
		}
		if runErr == nil {
			env.Status, env.ExitCode, env.ExitClass, env.Error, env.Stage = "ok", engine.ExitOK, receipt.ExitClassName(engine.ExitOK), "", ""
		}
	}
	if runErr != nil {
		return nil, env, runErr
	}
	// A draft that came from the unparsed answer's repair turn has spent it.
	repairSpent := d != nil
	if d == nil {
		var err error
		if d, err = Finish(env, p.OutDir, p.Scan, p.Ref, p.Build); err != nil {
			return nil, env, err
		}
		d.readKnowledge(p.Knowledge)
		if recoveredChars >= 0 {
			d.Recovered = true
			d.composeNotes(fmt.Sprintf("## Recovered\n\nThe runner refused the last message for %d characters of text before the answer. The post-step took the schema-valid JSON object after them. The original message is in the run's receipts.\n", recoveredChars))
		}
		d.revise(p.Revision)
		p.validate(d)
		p.checkServers(ctx, d)
	}
	d, env = p.repairAndCheck(ctx, d, env, repairSpent)
	d.noteExecProblems()
	d.noteServerProblems()
	return d, env, nil
}

// Passed says the rules pass the draft's rendered package.
func (d *Draft) Passed() bool {
	var v validate.Report
	return d.ManifestError == "" && json.Unmarshal(d.Validation, &v) == nil && v.Passed
}

func (p PostStep) pkg() string { return filepath.Join(p.OutDir, "package") }

// validate renders a draft into the package folder and checks it, when its
// answer let it be rendered at all.
func (p PostStep) validate(d *Draft) {
	if d.ManifestError == "" {
		d.ManifestError, d.Validation = p.renderAndValidate(d)
	}
}

// checkNotRun opens a ManifestError that is this computer's, not the
// draft's: the package could not be written, or validate could not run.
const checkNotRun = "the draft could not be checked: "

// renderAndValidate renders the draft into the package folder and checks
// it with validate's own code: the report, or why there is none.
func (p PostStep) renderAndValidate(d *Draft) (string, json.RawMessage) {
	if _, err := Render(d, p.pkg()); err != nil {
		// A manifest that does not parse is rendered all the same, for the
		// person to fix; validate would stop at the same error. A folder
		// that could not be written is this computer's failure.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return checkNotRun + err.Error(), nil
		}
		return err.Error(), nil
	}
	report, err := validate.Workspace(p.pkg(), validate.Options{Rules: p.Rules})
	if err != nil {
		return checkNotRun + err.Error(), nil
	}
	b, err := json.Marshal(report)
	if err != nil {
		return err.Error(), nil
	}
	return "", b
}

// repair is the post-step's third step (§5.2 step 3): a draft the rules
// still refuse gets one more turn of the Drafter, without its web tools,
// whose task is the rules' report on it and the draft itself (RepairTask),
// and nothing else. The second answer is corrected and checked like the
// first and kept whether or not it passes; a turn that returns no draft
// leaves the first, with its problems and a note saying why. The envelope
// that comes back carries both turns' usage, and the live usage lines the
// repair turn narrates count from the first turn's total, so a sandbox
// host meters the draft as a whole.
//
// r says what sent the draft to it: the rules' report, AI Review's check
// flagging it off_task, or both; with a flag the turn is
// also given the first turn's task, which AI Review passed before that
// turn started.
func (p PostStep) repair(ctx context.Context, d *Draft, env *receipt.Envelope, r RepairReport) (*Draft, *receipt.Envelope) {
	if r.Empty() || ctx.Err() != nil {
		return d, env
	}
	if p.Repair == nil {
		d.NoteNotRepaired(r, "this client runs no repair turn")
		return d, env
	}
	task, err := RepairTask(d, r)
	if err != nil {
		d.NoteNotRepaired(r, "its task could not be written")
		return d, env
	}
	budgets, ok := repairBudgets(p.Budgets, env.Usage)
	if !ok {
		d.NoteNotRepaired(r, "the first turn had spent the draft's budget")
		return d, env
	}
	runID, err := engine.NewRunID()
	if err != nil {
		d.NoteNotRepaired(r, "it could not be given a run id")
		return d, env
	}
	if p.Log != nil {
		_, _ = fmt.Fprintln(p.Log, RepairAnnouncement(r))
	}
	spent := env.Usage
	var onUsage func(receipt.Usage)
	if p.OnUsage != nil {
		onUsage = func(u receipt.Usage) { p.OnUsage(addUsage(spent, u)) }
	}
	dir := filepath.Join(p.OutDir, "repair")
	renv, rerr := p.Repair(ctx, RepairTurn{Task: task, OutDir: dir, Budgets: budgets, RunID: runID, OnUsage: onUsage})
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
		d.NoteNotRepaired(r, why)
		if ce != nil {
			d.repairFailure = fmt.Sprintf("%s (%s)", why, ce.Err)
		}
		return d, &total
	}
	rd, err := Repaired(d, renv, dir, r)
	if err != nil {
		d.NoteNotRepaired(r, "its answer was not a draft")
		return d, &total
	}
	if err := os.RemoveAll(p.pkg()); err != nil {
		d.NoteNotRepaired(r, "the first answer's package could not be replaced")
		return d, &total
	}
	rd.revise(p.Revision)
	p.validate(rd)
	p.checkServers(ctx, rd)
	return rd, &total
}

// repairAndCheck is the post-step's last two steps (plan builder-simplify.md
// §5.2 steps 3 and 4, D6; agentry-cli#417, #418). AI Review checks the
// rendered package (the release rubric as S5 will ask it at publish, and
// the draft question set against the person's description), and the
// verdicts go into draft.json under aiReview.check. A draft the rules
// refuse, or one the check flagged as not doing the task
// (Check.Repairable: off_task alone; the release rubric's findings are
// recorded as advisory and never repaired, quickagents-that-run.md D6), is
// given its one repair turn with the rules' report and what the check
// flagged, and the second answer is checked again. So a draft asks at most two checks, the
// runs extension's cap for a hosted one (agentry-enterprise#265), and none
// is ever retried: the registry counts a check as it admits it. A second
// answer that breaks rules the first passed, or drops a declared
// capability the report did not name, is not kept (keepFirst). A draft that
// could not be rendered is not checked; repairSpent is a draft whose turn
// was spent before (an answer that was not a draft).
func (p PostStep) repairAndCheck(ctx context.Context, d *Draft, env *receipt.Envelope, repairSpent bool) (*Draft, *receipt.Envelope) {
	var checkerErr error
	switch {
	case p.CheckerErr != nil:
		// The key's own error is not recorded: draft.json says why in the
		// CLI's words.
		checkerErr = errors.New("AI Review cannot check with this key")
	case p.Checker == nil:
		checkerErr = errors.New("this client asks AI Review for no check")
	}
	checks := 0
	var spent Check
	// ask checks d's package as it now stands: nil for a draft that could
	// not be rendered, or when there is no checker.
	ask := func(d *Draft) (*Check, error) {
		if d.ManifestError != "" || checkerErr != nil {
			return nil, checkerErr
		}
		pk, err := CheckPackage(p.Description, p.pkg())
		if err != nil {
			return nil, err
		}
		checks++
		c, err := p.Checker.Check(ctx, pk)
		if c != nil {
			spent.Requests += c.Requests
			spent.InputTokens += c.InputTokens
		}
		return c, err
	}
	record := func(d *Draft, c *Check, err error) (*Draft, *receipt.Envelope) {
		switch {
		case d.ManifestError != "":
		case err != nil:
			d.RecordCheck(&Check{Requests: spent.Requests, InputTokens: spent.InputTokens}, checks, err)
		default:
			c.Requests, c.InputTokens = spent.Requests, spent.InputTokens
			d.RecordCheck(c, checks, nil)
		}
		return d, env
	}

	c, err := ask(d)
	r := RepairReport{Task: p.Task}
	lines, refused := rulesReport(d)
	if refused {
		r.Rules = lines
	}
	if c != nil {
		r.Flagged = c.Repairable()
	}
	if repairSpent || r.Empty() || ctx.Err() != nil {
		return record(d, c, err)
	}
	rd, renv := p.repair(ctx, d, env, r)
	env = renv
	if !rd.IsRepaired() || d.IsRepaired() {
		return record(d, c, err)
	}
	if broke, dropped := keepFirst(d, rd, r); broke || len(dropped) > 0 {
		// The second answer dropped what the task needs, or broke a rule
		// the first passed, so the first is kept, and its package put back.
		if len(dropped) > 0 {
			d.NoteSecondAnswerDropped(r, dropped)
		} else {
			d.NoteSecondAnswerRefused(r)
		}
		if os.RemoveAll(p.pkg()) == nil {
			p.validate(d)
		}
		return record(d, c, err)
	}
	c, err = ask(rd)
	return record(rd, c, err)
}

// keepFirst says a repair's first answer is kept over its second, r being
// what sent the first to the repair turn. First, dropped is what the second
// no longer declares of the first's MCP servers, egress hosts, commands and
// secrets and r does not name (droppedUnnamed): the task needs them, and a
// repair never clears a report by stripping them (agentry-cli#633), even
// when the second answer passes rules the first broke. Otherwise broke says
// the second broke a rule its first passed: validate's, which the registry
// enforces, or else the Drafter's cap on the instructions, which keeps them
// within what AI Review's check reads, or its rule on exec, which keeps them
// to what a run provides, or its rule on declared servers' runtimes
// (checkServers). A second answer that keeps what the first declared and
// mends what it broke is kept whatever else it does.
func keepFirst(first, second *Draft, r RepairReport) (broke bool, dropped []string) {
	if dropped = droppedUnnamed(first, second, r); len(dropped) > 0 {
		return false, dropped
	}
	return brokeARule(first, second), nil
}

// brokeARule is keepFirst's rule half.
func brokeARule(first, second *Draft) bool {
	if first.Passed() != second.Passed() {
		return first.Passed()
	}
	if first.withinCap() && !second.withinCap() {
		return true
	}
	if len(execProblems(first.Manifest)) == 0 && len(execProblems(second.Manifest)) > 0 {
		return true
	}
	return len(first.serverProblems) == 0 && len(second.serverProblems) > 0
}

// withinCap says the draft's instructions are within the Drafter's cap.
func (d *Draft) withinCap() bool { return len(d.Instructions) <= MaxInstructionBytes }

// rulesReport is what the rules said of a draft, one line each, in their
// own words: validate's report on its package, and the Drafter's cap on its
// instructions, its rule on exec (execProblems) and its rule on declared
// servers' runtimes (checkServers), which validate does not hold. refused is false for a draft
// both pass, or one whose check could not run for a reason of this
// computer's rather than the draft's.
func rulesReport(d *Draft) (lines []string, refused bool) {
	lines, refused = validationReport(d)
	if !d.withinCap() {
		lines = append(lines, instructionOverCap(len(d.Instructions)))
		refused = true
	}
	if probs := execProblems(d.Manifest); len(probs) > 0 {
		lines = append(lines, probs...)
		refused = true
	}
	if len(d.serverProblems) > 0 {
		lines = append(lines, d.serverProblems...)
		refused = true
	}
	return lines, refused
}

// instructionOverCap is the rules' line for an instruction past the
// Drafter's own cap, which validate does not hold a package to.
func instructionOverCap(n int) string {
	return fmt.Sprintf("the instructions: %d bytes, past the Drafter's cap of %d bytes; shorten them to at most %d by cutting what the run does not need, keeping all six headings",
		n, MaxInstructionBytes, MaxInstructionBytes)
}

// validationReport is what validate said of a draft's rendered package, as
// rulesReport describes.
func validationReport(d *Draft) ([]string, bool) {
	if d.Validation == nil {
		if d.ManifestError == "" || strings.HasPrefix(d.ManifestError, checkNotRun) {
			return nil, false
		}
		return []string{"the draft: " + d.ManifestError}, true
	}
	var v validate.Report
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
	r.TimeoutSeconds = RepairTimeoutSeconds
	if b.TimeoutSeconds > 0 {
		left := b.TimeoutSeconds - int(spent.WallMS/1000)
		if left <= 0 {
			return r, false
		}
		r.TimeoutSeconds = min(left, RepairTimeoutSeconds)
	}
	r.MaxTurns = RepairMaxTurns
	if b.MaxTurns > 0 {
		left := b.MaxTurns - spent.Turns
		if left <= 0 {
			return r, false
		}
		r.MaxTurns = min(left, RepairMaxTurns)
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
