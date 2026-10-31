package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// What a run says about itself on stderr once it has ended, in words a
// person can read, whatever --output-format stdout carries: a success names
// its files, its duration, its cost where one is known and its receipts; a
// failure names its exit class with the class's meaning, the cause, the
// receipts and whether a retry is likely to help. The class words come from
// the exit-class table (engine.ExitClasses), the one every client shows.

// maxSummaryFiles is how many files a summary names before counting the rest.
const maxSummaryFiles = 12

// runSummary is the lines of a finished run's summary.
func runSummary(env *receipt.Envelope) []string {
	head := "run finished"
	if env.Usage.WallMS > 0 {
		head += " in " + humanDuration(time.Duration(env.Usage.WallMS)*time.Millisecond)
	}
	if cost := runCost(env); cost != "" {
		head += " (" + cost + ")"
	}
	lines := []string{head}
	if files, more, label := summaryFiles(env); len(files) > 0 {
		line := label + ": " + strings.Join(files, ", ")
		switch {
		case more < 0:
			line += " and more"
		case more > 0:
			line += fmt.Sprintf(" and %d more", more)
		}
		lines = append(lines, line)
	} else {
		lines = append(lines, "no files written; the result is the output above")
	}
	if dir := receiptsDir(env); dir != "" {
		lines = append(lines, "receipts: "+dir)
	}
	return lines
}

// runCost is the run's cost as a phrase, where the receipt knows one: the
// credits an Agentry credits session charged, or the harness's own figure.
func runCost(env *receipt.Envelope) string {
	if env.Inference != nil && env.Inference.Credits > 0 {
		if env.Inference.Credits == 1 {
			return "1 credit"
		}
		return fmt.Sprintf("%d credits", env.Inference.Credits)
	}
	if env.Usage.CostUSD > 0 {
		return fmt.Sprintf("$%.2f", env.Usage.CostUSD)
	}
	return ""
}

// summaryFiles names what the run left for the person: its deliverables by
// path, with the title the agent gave each; failing that, the files under
// reports/. The second value counts the files past maxSummaryFiles, and is
// negative when the envelope's own list was cut and the count is unknown.
// Paths and titles are the agent's words, so each is made safe for a
// terminal before it is shown.
func summaryFiles(env *receipt.Envelope) (names []string, more int, label string) {
	if d := env.Deliverables; d != nil && len(d.Files) > 0 {
		for _, f := range d.Files {
			name := termtext.Safe(f.Path)
			if f.Title != "" && f.Title != filepath.Base(f.Path) {
				name += " (" + termtext.Safe(f.Title) + ")"
			}
			names = append(names, name)
		}
		label = "deliverables"
		if len(d.Files) == 1 {
			label = "deliverable"
		}
	} else if env.OutDir != "" {
		names = reportFiles(filepath.Join(env.OutDir, "reports"))
		label = "output files"
		if len(names) == 1 {
			label = "output file"
		}
	}
	if len(names) > maxSummaryFiles {
		more = len(names) - maxSummaryFiles
		names = names[:maxSummaryFiles]
	}
	if d := env.Deliverables; d != nil && d.Truncated {
		more = -1
	}
	return names, more, label
}

// reportFiles lists the regular files under a run's reports folder, by path
// relative to the output folder, sorted. A folder that cannot be read lists
// nothing.
func reportFiles(reports string) []string {
	var out []string
	_ = filepath.WalkDir(reports, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(filepath.Dir(reports), path)
		if rerr == nil {
			out = append(out, termtext.Safe(filepath.ToSlash(rel)))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// receiptsDir is where a run's receipts are, as the envelope records it.
func receiptsDir(env *receipt.Envelope) string {
	if env.Receipts.Dir != "" {
		return env.Receipts.Dir
	}
	if env.OutDir != "" {
		return filepath.Join(env.OutDir, "receipts")
	}
	return ""
}

// humanDuration is a duration in the words a person reads: 850ms, 12s,
// 1m12s, 2h05m.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// printRunSummary writes a finished run's summary, then what its deliveries
// did.
func printRunSummary(w io.Writer, env *receipt.Envelope) {
	for _, line := range runSummary(env) {
		_, _ = fmt.Fprintln(w, line)
	}
	printDeliverySummary(w, env)
}

// failureReport is the lines a failed run prints: the class by name with its
// meaning, the cause as the receipt records it (its first line after the
// label, the rest indented beneath, since a gateway's log tail or an agent's
// stderr tail rides along in it, each line made safe for a terminal), the
// receipts folder when the run has one, and the retry hint.
func failureReport(err error, receipts string) []string {
	class := engine.ExitClassOf(exitCodeOfRun(err))
	lines := []string{fmt.Sprintf("agentry: run failed: %s: %s", class.Name, class.Meaning)}
	cause := strings.Split(strings.TrimSpace(err.Error()), "\n")
	lines = append(lines, "  cause: "+termtext.Safe(cause[0]))
	for _, l := range cause[1:] {
		lines = append(lines, "    "+termtext.Safe(l))
	}
	if receipts != "" {
		lines = append(lines, "  receipts: "+receipts)
	}
	if class.RetryLikelyHelps {
		lines = append(lines, "  running it again with the same inputs may well succeed")
	} else {
		lines = append(lines, "  running it again with the same inputs is unlikely to help")
	}
	return lines
}

// reportedError is a run failure the run command has already explained on
// stderr. main exits with its class and prints nothing more, so the cause is
// said once, in the report.
type reportedError struct{ err error }

func (e *reportedError) Error() string { return e.err.Error() }
func (e *reportedError) Unwrap() error { return e.err }

// exitCodeOfRun is the code the process exits with for a run's error: its
// class, or cli for an error of no class.
func exitCodeOfRun(err error) int {
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return engine.ExitCLI
}

// reportFailure prints the failure report for err and returns the error
// marked as reported. A run interrupted while nothing else had failed it
// reports "interrupted" under the cli class, as its envelope does; an error
// of another class keeps its class, which is the code the process exits
// with. The receipts folder is named only once the run has one on disk.
func reportFailure(w io.Writer, err error, interrupted bool, outDir string) error {
	shown := err
	if interrupted && exitCodeOfRun(err) == engine.ExitCLI {
		shown = engine.Classify(engine.ExitCLI, errors.New("interrupted"))
	}
	receipts := ""
	if outDir != "" {
		if st, serr := os.Stat(filepath.Join(outDir, "receipts")); serr == nil && st.IsDir() {
			receipts = filepath.Join(outDir, "receipts")
		}
	}
	for _, line := range failureReport(shown, receipts) {
		_, _ = fmt.Fprintln(w, line)
	}
	return &reportedError{err: err}
}

// exitClassHelp is the exit-class paragraph of run --help, one line a class
// from the table: its code, its name, its meaning and whether a retry helps.
func exitClassHelp() string {
	var b strings.Builder
	b.WriteString("Every failure class has its own exit code, named in the result's exitClass; a\n")
	b.WriteString("pipeline branches on the name. The classes, and whether running the same\n")
	b.WriteString("thing again is likely to help:\n")
	for _, c := range engine.ExitClasses {
		retry := "no"
		if c.RetryLikelyHelps {
			retry = "yes"
		}
		fmt.Fprintf(&b, "  %2d %-13s %s (retry: %s)\n", c.Code, c.Name, c.Meaning, retry)
	}
	return b.String()
}

// dryRunSummary is the human reading of a compiled plan, for --dry-run
// without --json: what would run, as what, with which grants, on which
// inputs, and what it would leave behind. Nothing here is the plan's bytes;
// --json prints those.
func dryRunSummary(plan *spec.Plan, planDir string, opts runOpts) []string {
	var lines []string
	add := func(k, v string) {
		if v != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", k+":", v))
		}
	}
	lines = append(lines, fmt.Sprintf("run plan for %s (%s)", plan.Ref, plan.Source))
	mode := plan.Mode
	if len(plan.Modes) > 1 {
		mode += " (declares " + strings.Join(plan.Modes, ", ") + ")"
	}
	add("mode", mode)
	harness := plan.Harness.Adapter
	if plan.Harness.Agent.Image != "" {
		harness += ", image " + plan.Harness.Agent.Image
	} else if plan.Harness.Host != nil {
		harness += ", from this machine's own installation"
	}
	add("harness", harness)
	add("model", plan.Harness.Model)
	add("tools", strings.Join(plan.Tools, ", "))
	add("network", egressSummary(plan.Capabilities))
	add("files", fsSummary(plan.Capabilities))
	add("exec", strings.Join(plan.Capabilities.ExecCommands, ", "))
	if len(plan.MCPServers) > 0 {
		var names []string
		for _, s := range plan.MCPServers {
			names = append(names, s.Name+"@"+s.Version)
		}
		add("servers", strings.Join(names, ", "))
	}
	if len(plan.Secrets) > 0 {
		add("secrets", strings.Join(plan.Secrets, ", ")+" (names only; values are never in the plan)")
	}
	if len(plan.Capabilities.TargetHosts) > 0 {
		add("hosts", strings.Join(plan.Capabilities.TargetHosts, ", "))
	}
	if plan.Capabilities.Browser != nil {
		add("browser", browserSummary(plan.Capabilities.Browser))
	}
	add("inputs", inputsSummary(opts))
	budgets := fmt.Sprintf("%s timeout", humanDuration(time.Duration(plan.Budgets.TimeoutSeconds)*time.Second))
	if plan.Budgets.MaxTurns > 0 {
		budgets += fmt.Sprintf(", %d turns", plan.Budgets.MaxTurns)
	}
	if plan.Budgets.MaxCostUSD > 0 {
		budgets += fmt.Sprintf(", $%.2f", plan.Budgets.MaxCostUSD)
	}
	add("budgets", budgets)
	result := "a text result on stdout"
	if plan.Harness.ResultKind == "json" {
		result = "a JSON result on stdout, matching the declared output schema"
	}
	add("deliverables", "the files the agent writes under reports/, and "+result)
	out := opts.out
	if out == "" {
		out = "./agentry-out/<namespace>-<name>-<run id>"
	}
	add("output", out+" (reports/, receipts/, result.json)")
	add("plan", shortHash(plan)+" in "+planDir)
	lines = append(lines, "nothing ran: --dry-run compiles the plan and stops; --json prints the plan itself")
	return lines
}

// egressSummary is one phrase for the plan's network contract.
func egressSummary(c spec.CapSummary) string {
	switch c.EgressMode {
	case "none", "":
		return "none"
	case "hosts":
		return "only " + strings.Join(c.EgressHosts, ", ")
	default:
		return "any host (UNRESTRICTED)"
	}
}

// fsSummary is one phrase for the plan's file contract.
func fsSummary(c spec.CapSummary) string {
	part := func(mode string, paths []string) string {
		switch mode {
		case "", "none":
			return "none"
		case "paths":
			return strings.Join(paths, ", ")
		default:
			return mode
		}
	}
	return "read " + part(c.FSReadMode, c.FSReadPaths) + "; write " + part(c.FSWriteMode, c.FSWritePaths)
}

// browserSummary is one phrase for the plan's browser line.
func browserSummary(b *spec.BrowserCap) string {
	var parts []string
	if len(b.Sites) > 0 {
		parts = append(parts, "sites "+strings.Join(b.Sites, ", "))
	}
	if len(b.Hosts) > 0 {
		parts = append(parts, "hosts "+strings.Join(b.Hosts, ", "))
	}
	if b.Public {
		parts = append(parts, "any public site")
	}
	if len(parts) == 0 {
		return "yes"
	}
	return strings.Join(parts, "; ")
}

// inputsSummary names what the run would be given: the task, the files
// and the knowledge packs the flags name.
func inputsSummary(opts runOpts) string {
	var parts []string
	switch {
	case opts.input != "":
		parts = append(parts, "task from "+opts.input)
	default:
		parts = append(parts, "task from stdin, or the agent's own instruction when there is none")
	}
	if len(opts.files) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s): %s", len(opts.files), strings.Join(opts.files, ", ")))
	}
	if len(opts.knowledge) > 0 {
		parts = append(parts, "knowledge "+strings.Join(opts.knowledge, ", "))
	}
	return strings.Join(parts, "; ")
}
