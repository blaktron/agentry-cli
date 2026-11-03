package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func TestAFinishedRunSummarizesItsFilesTimeCostAndReceipts(t *testing.T) {
	env := &receipt.Envelope{Status: "ok", OutDir: "/runs/out", Receipts: receipt.Receipts{Dir: "/runs/out/receipts"},
		Usage: receipt.Usage{WallMS: 72_400, CostUSD: 0.031},
		Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{
			{Path: "reports/summary.md", Title: "Weekly summary"},
			{Path: "reports/data.csv", Title: "data.csv"},
		}}}
	got := strings.Join(runSummary(env), "\n")
	want := "run finished in 1m12s ($0.03)\ndeliverables: reports/summary.md (Weekly summary), reports/data.csv\nreceipts: /runs/out/receipts"
	if got != want {
		t.Errorf("summary:\n%s\nwant:\n%s", got, want)
	}

	credits := &receipt.Envelope{Status: "ok", Inference: &receipt.Inference{Credits: 3}, Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/a.md"}}}}
	if got := runSummary(credits)[0]; got != "run finished (3 credits)" {
		t.Errorf("credits: %q", got)
	}
	if got := runSummary(credits)[1]; got != "deliverable: reports/a.md" {
		t.Errorf("one deliverable: %q", got)
	}
	// The receipts are where the envelope says, not always under the output folder.
	elsewhere := runSummary(&receipt.Envelope{Status: "ok", OutDir: "/runs/out", Receipts: receipt.Receipts{Dir: "/elsewhere/receipts"}})
	if got := elsewhere[len(elsewhere)-1]; got != "receipts: /elsewhere/receipts" {
		t.Errorf("receipts: %q", got)
	}
	// The cost is named only where the receipt knows one: a single credit
	// is not plural, credits win over dollars, and nothing charged names nothing.
	for _, c := range []struct {
		name string
		env  receipt.Envelope
		want string
	}{
		{"one credit", receipt.Envelope{Status: "ok", Inference: &receipt.Inference{Credits: 1}}, "run finished (1 credit)"},
		{"credits and dollars", receipt.Envelope{Status: "ok", Inference: &receipt.Inference{Credits: 2}, Usage: receipt.Usage{CostUSD: 0.5}}, "run finished (2 credits)"},
		{"no credits charged", receipt.Envelope{Status: "ok", Inference: &receipt.Inference{}, Usage: receipt.Usage{CostUSD: 0.5}}, "run finished ($0.50)"},
		{"dollars", receipt.Envelope{Status: "ok", Usage: receipt.Usage{WallMS: 850, CostUSD: 12.345}}, "run finished in 850ms ($12.35)"},
		{"no duration and no cost", receipt.Envelope{Status: "ok"}, "run finished"},
	} {
		if got := runSummary(&c.env)[0]; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestARunWithoutDeliverablesNamesItsReportFiles(t *testing.T) {
	out := t.TempDir()
	for _, f := range []string{"reports/b.md", "reports/sub/a.txt", "receipts/events.ndjson"} {
		full := filepath.Join(out, f)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte("x"), 0o644)
	}
	env := &receipt.Envelope{Status: "ok", OutDir: out}
	lines := runSummary(env)
	if lines[1] != "output files: reports/b.md, reports/sub/a.txt" || lines[2] != "receipts: "+filepath.Join(out, "receipts") {
		t.Errorf("summary: %q", lines)
	}
	// Only regular files under reports/ are named: not a folder, not a link.
	only := t.TempDir()
	_ = os.MkdirAll(filepath.Join(only, "reports", "empty"), 0o755)
	_ = os.WriteFile(filepath.Join(only, "reports", "one.md"), []byte("x"), 0o644)
	if os.Symlink(filepath.Join(only, "reports", "one.md"), filepath.Join(only, "reports", "link.md")) == nil {
		if got := runSummary(&receipt.Envelope{Status: "ok", OutDir: only})[1]; got != "output file: reports/one.md" {
			t.Errorf("one report file and a link: %q", got)
		}
	}
	if got := runSummary(&receipt.Envelope{Status: "ok", OutDir: only})[1]; got != "output file: reports/one.md" {
		t.Errorf("one report file: %q", got)
	}
	// A deliverables list with no file in it is no list: the report files are named.
	if got := runSummary(&receipt.Envelope{Status: "ok", OutDir: only, Deliverables: &receipt.Deliverables{}})[1]; got != "output file: reports/one.md" {
		t.Errorf("an empty deliverables list: %q", got)
	}
	bare := runSummary(&receipt.Envelope{Status: "ok"})
	if len(bare) != 2 || bare[0] != "run finished" || bare[1] != "no files written; the result is the output above" {
		t.Errorf("a run with nothing: %q", bare)
	}
}

func TestManyFilesAreCounted(t *testing.T) {
	// Up to the limit every file is named; one past it, the rest are counted.
	for n, want := range map[int]string{11: "", 12: "", 13: " and 1 more"} {
		var files []receipt.Deliverable
		for i := 0; i < n; i++ {
			files = append(files, receipt.Deliverable{Path: fmt.Sprintf("reports/f%02d.md", i)})
		}
		line := runSummary(&receipt.Envelope{Status: "ok", Deliverables: &receipt.Deliverables{Files: files}})[1]
		if !strings.HasSuffix(line, want) || strings.Count(line, ".md") != min(n, 12) || (want == "" && strings.Contains(line, " and ")) {
			t.Errorf("%d files: %q", n, line)
		}
	}

	d := &receipt.Deliverables{}
	for i := 0; i < maxSummaryFiles+3; i++ {
		d.Files = append(d.Files, receipt.Deliverable{Path: "reports/f.md"})
	}
	line := runSummary(&receipt.Envelope{Status: "ok", Deliverables: d})[1]
	if !strings.HasSuffix(line, " and 3 more") || strings.Count(line, "reports/f.md") != maxSummaryFiles {
		t.Errorf("line = %q", line)
	}
	// A list the envelope itself cut has no exact count to give.
	d.Truncated = true
	if line := runSummary(&receipt.Envelope{Status: "ok", Deliverables: d})[1]; !strings.HasSuffix(line, " and more") {
		t.Errorf("truncated: %q", line)
	}
}

// What the agent named, and what a failure's text carries, is made safe for
// the terminal before it is shown.
func TestTheSummaryShowsNoTerminalControlCharacters(t *testing.T) {
	out := t.TempDir()
	_ = os.MkdirAll(filepath.Join(out, "reports"), 0o755)
	_ = os.WriteFile(filepath.Join(out, "reports", "a\x1b[31mb.md"), []byte("x"), 0o644)
	lines := runSummary(&receipt.Envelope{Status: "ok", OutDir: out})
	if strings.Contains(strings.Join(lines, "\n"), "\x1b") {
		t.Errorf("an escape reached the summary: %q", lines)
	}
	titled := runSummary(&receipt.Envelope{Status: "ok", Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/x.md", Title: "t\x07t"}}}})
	if strings.Contains(strings.Join(titled, "\n"), "\x07") {
		t.Errorf("a bell reached the summary: %q", titled)
	}
	named := runSummary(&receipt.Envelope{Status: "ok", Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/y\x1b[2J.md"}}}})
	if strings.Contains(strings.Join(named, "\n"), "\x1b") {
		t.Errorf("an escape in a deliverable's path reached the summary: %q", named)
	}
	report := failureReport(engine.Classify(engine.ExitHarness, errors.New("\x1b[2Jagent exited 1\n\x1b]0;x\x07tail")), "")
	if strings.Contains(strings.Join(report, "\n"), "\x1b") || strings.Contains(strings.Join(report, "\n"), "\x07") {
		t.Errorf("an escape reached the report: %q", report)
	}
}

func TestAFailureReportNamesTheClassCauseReceiptsAndRetry(t *testing.T) {
	err := engine.Classify(engine.ExitGateway, errors.New("gateway health probe failed\ngateway log tail:\nboom"))
	got := failureReport(err, "/runs/out/receipts")
	want := []string{
		"agentry: run failed: gateway: " + engine.ExitClassOf(13).Meaning,
		"  cause: gateway health probe failed",
		"    gateway log tail:",
		"    boom",
		"  receipts: /runs/out/receipts",
		"  running it again with the same inputs may well succeed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("report:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Under --verbose the detail an error carries beside its line is added.
	flags.verbose = true
	t.Cleanup(func() { flags.verbose = false })
	verbose := failureReport(detailedTestError{line: "registry unreachable", detail: "dial tcp: connection refused\x1b[2J"}, "")
	if len(verbose) != 4 || verbose[2] != "  detail: dial tcp: connection refused?[2J" {
		t.Errorf("a verbose report: %q", verbose)
	}
	if same := failureReport(detailedTestError{line: "same", detail: "same"}, ""); len(same) != 3 {
		t.Errorf("a detail that repeats the cause: %q", same)
	}
	if none := failureReport(errors.New("no detail"), ""); len(none) != 3 {
		t.Errorf("an error with no detail: %q", none)
	}
	flags.verbose = false
	if quiet := failureReport(detailedTestError{line: "registry unreachable", detail: "dial tcp: connection refused"}, ""); len(quiet) != 3 {
		t.Errorf("a report without --verbose: %q", quiet)
	}
	plain := failureReport(errors.New("intake: no answer"), "")
	if plain[0] != "agentry: run failed: cli: "+engine.ExitClassOf(1).Meaning || len(plain) != 3 || !strings.HasSuffix(plain[2], "unlikely to help") {
		t.Errorf("an unclassified error reads as cli, with no receipts line: %q", plain)
	}
}

// detailedTestError is an error with a longer form for --verbose.
type detailedTestError struct{ line, detail string }

func (e detailedTestError) Error() string  { return e.line }
func (e detailedTestError) Detail() string { return e.detail }

func TestAReportedFailureKeepsItsClassAndText(t *testing.T) {
	var out strings.Builder
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "receipts"), 0o755)
	inner := engine.Classify(engine.ExitPolicy, errors.New("refused: FIXTURE_TOKEN has no value"))
	err := reportFailure(&out, inner, false, dir)
	wantClassified(t, err, engine.ExitPolicy)
	if err.Error() != inner.Error() {
		t.Errorf("text changed: %q", err.Error())
	}
	var reported *reportedError
	if !errors.As(err, &reported) {
		t.Error("not marked reported")
	}
	if !strings.Contains(out.String(), "run failed: policy:") || !strings.Contains(out.String(), "receipts: "+filepath.Join(dir, "receipts")) {
		t.Errorf("report:\n%s", out.String())
	}
	out.Reset()
	interrupted := reportFailure(&out, errors.New("intake: the answer was not read"), true, filepath.Join(dir, "none"))
	if interrupted.Error() != "intake: the answer was not read" {
		t.Errorf("the error returned for an interrupted run = %q: the person is told it was interrupted, the caller keeps the cause", interrupted)
	}
	if !strings.Contains(out.String(), "run failed: cli:") || !strings.Contains(out.String(), "cause: interrupted") || strings.Contains(out.String(), "receipts:") {
		t.Errorf("an interrupted run, with no receipts folder on disk:\n%s", out.String())
	}
	// With no output folder, a receipts folder of the working directory is not the run's.
	t.Chdir(dir)
	out.Reset()
	_ = reportFailure(&out, errors.New("intake: the answer was not read"), false, "")
	if strings.Contains(out.String(), "receipts:") {
		t.Errorf("the working directory's receipts folder was named:\n%s", out.String())
	}
	// A receipts "folder" that is a file is no folder to point at.
	out.Reset()
	asFile := t.TempDir()
	_ = os.WriteFile(filepath.Join(asFile, "receipts"), []byte("x"), 0o644)
	_ = reportFailure(&out, errors.New("intake: the answer was not read"), false, asFile)
	if strings.Contains(out.String(), "receipts:") {
		t.Errorf("a receipts file was named as the receipts folder:\n%s", out.String())
	}
	// A signal that arrives while a classified failure is already in flight
	// does not rename the class: the process exits with that class's code.
	out.Reset()
	err = reportFailure(&out, inner, true, "")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(out.String(), "run failed: policy:") || strings.Contains(out.String(), "interrupted") {
		t.Errorf("a classified failure under an interrupt:\n%s", out.String())
	}
}

// What the run command makes of an error it returns: a failure of a run that
// began is explained and marked reported; a mistake before the run began is
// returned as it is, unless the run was interrupted.
func TestAFailedRunIsExplainedOnlyOnceItBegan(t *testing.T) {
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "receipts"), 0o755)
	live := func() *cobra.Command {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		return cmd
	}
	explain := func(cmd *cobra.Command, p *runProgress, err error) (said string, returned error) {
		var out strings.Builder
		cmd.SetErr(&out)
		returned = reportRunFailure(cmd, p, err)
		return out.String(), returned
	}
	mistake := errors.New("pass exactly one of ns/name[@range] or --local DIR")

	// Before the run had an output folder, an error of no class is the person's
	// mistake: returned as it is, nothing said.
	if said, got := explain(live(), &runProgress{}, mistake); got != mistake || said != "" {
		t.Errorf("a mistake before the run: %v, said %q", got, said)
	}
	// A classified failure is a run that failed, whatever the stage.
	said, got := explain(live(), &runProgress{}, engine.Classify(engine.ExitVerify, errors.New("digest mismatch")))
	var reported *reportedError
	if !errors.As(got, &reported) || !strings.Contains(said, "run failed: verify:") || !strings.Contains(said, "cause: digest mismatch") {
		t.Errorf("a classified failure: %v, said %q", got, said)
	}
	// So is any error once the output folder exists, and the report points at
	// the receipts that are there.
	said, got = explain(live(), &runProgress{outDir: dir}, mistake)
	if !errors.As(got, &reported) || !strings.Contains(said, "run failed: cli:") || !strings.Contains(said, "receipts: "+filepath.Join(dir, "receipts")) {
		t.Errorf("a failure after the output folder was made: %v, said %q", got, said)
	}
	// And an interrupted one, which says so.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := live()
	cmd.SetContext(ctx)
	said, got = explain(cmd, &runProgress{}, mistake)
	if !errors.As(got, &reported) || !strings.Contains(said, "cause: interrupted") {
		t.Errorf("an interrupted run: %v, said %q", got, said)
	}
	// A nil progress is no output folder.
	if said, got := explain(live(), nil, mistake); got != mistake || said != "" {
		t.Errorf("a mistake with no progress: %v, said %q", got, said)
	}
}

func TestRunHelpListsEveryExitClass(t *testing.T) {
	stdout, _, err := runCLI(t, nil, "run", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range engine.ExitClasses {
		if !strings.Contains(stdout, c.Name+" ") || !strings.Contains(stdout, c.Meaning) {
			t.Errorf("run --help lacks the class %q with its meaning", c.Name)
		}
	}
	if !strings.Contains(stdout, "18 blocked") || !strings.Contains(stdout, "(retry: yes)") {
		t.Errorf("help:\n%s", stdout)
	}
	for _, c := range engine.ExitClasses {
		retry := "no"
		if c.RetryLikelyHelps {
			retry = "yes"
		}
		if want := fmt.Sprintf("\n  %2d %-13s %s (retry: %s)\n", c.Code, c.Name, c.Meaning, retry); !strings.Contains(stdout, want) {
			t.Errorf("run --help lacks the line %q", want)
		}
	}
	if !strings.Contains(stdout, "(retry: no)") {
		t.Errorf("no class is said to be one a retry will not help:\n%s", stdout)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		850 * time.Millisecond:                "850ms",
		12 * time.Second:                      "12s",
		72*time.Second + 400*time.Millisecond: "1m12s",
		2*time.Hour + 5*time.Minute:           "2h05m",
		0:                                     "0ms",
		time.Second:                           "1s",
		12*time.Second + 600*time.Millisecond: "13s",
		59*time.Second + 400*time.Millisecond: "59s",
		time.Minute:                           "1m00s",
		72*time.Second + 600*time.Millisecond: "1m13s",
		time.Hour:                             "1h00m",
		2*time.Hour + 5*time.Minute + 40*time.Second: "2h06m",
		2*time.Hour + 59*time.Minute:                 "2h59m",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("%v = %q, want %q", d, got, want)
		}
	}
}

func TestTheDryRunSummaryReadsThePlan(t *testing.T) {
	plan := &spec.Plan{Ref: "acme/weekly@1.0.0", Source: "registry", Mode: "autonomous", Modes: []string{"autonomous", "interactive"},
		Harness:      spec.HarnessSpec{Adapter: "docker-agent", Model: "openai/gpt-5", Agent: spec.ContainerSpec{Image: "docker/agent@sha256:abc"}, ResultKind: "json"},
		Tools:        []string{"write_report", "list_inputs"},
		Capabilities: spec.CapSummary{EgressMode: "hosts", EgressHosts: []string{"api.example.com"}, FSReadMode: "workspace", FSWriteMode: "none", ExecCommands: []string{"jq"}},
		Secrets:      []string{"FIXTURE_TOKEN"},
		Budgets:      spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60},
	}
	got := strings.Join(dryRunSummary(plan, "/plans/x", runOpts{files: []string{"a.csv"}, input: "task.txt"}), "\n")
	for _, want := range []string{
		"run plan for acme/weekly@1.0.0 (registry)",
		"mode:        autonomous (declares autonomous, interactive)",
		"harness:     docker-agent, image docker/agent@sha256:abc",
		"model:       openai/gpt-5",
		"tools:       write_report, list_inputs",
		"network:     only api.example.com",
		"files:       read workspace; write none",
		"exec:        jq",
		"secrets:     FIXTURE_TOKEN (names only",
		"inputs:      task from task.txt; 1 file(s): a.csv",
		"budgets:     15m00s timeout, 60 turns",
		"deliverables: the files the agent writes under reports/, and a JSON result on stdout",
		"output:      ./agentry-out/<namespace>-<name>-<run id>",
		"nothing ran",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "planVersion") {
		t.Error("the summary is not the plan's JSON")
	}

	// The other shapes of a plan: a harness that is the person's own
	// installation, unrestricted egress, file paths, servers, target hosts,
	// a browser, a cost cap, the knowledge packs given, a chosen output folder.
	line := func(key, value string) string { return fmt.Sprintf("  %-12s %s", key+":", value) }
	other := &spec.Plan{Ref: "acme/box@2.0.0", Source: "registry", Mode: "interactive", Modes: []string{"interactive"},
		Harness: spec.HarnessSpec{Adapter: "claude-code", Host: &spec.HostProcessSpec{Command: "claude"}, ResultKind: "text"},
		Capabilities: spec.CapSummary{EgressMode: "all", FSReadMode: "paths", FSReadPaths: []string{"a", "b"}, FSWriteMode: "workspace", TargetHosts: []string{"server"},
			Browser: &spec.BrowserCap{Sites: []string{"x.example"}, Hosts: []string{"h.example"}, Public: true}},
		MCPServers: []spec.MCPServerSpec{{Name: "mcp-a", Version: "1.2.3"}, {Name: "mcp-b", Version: "2"}},
		Budgets:    spec.Budgets{TimeoutSeconds: 61, MaxCostUSD: 2.5},
	}
	hash, err := other.Hash()
	if err != nil {
		t.Fatal(err)
	}
	wantOther := []string{
		"run plan for acme/box@2.0.0 (registry)",
		line("mode", "interactive"),
		line("harness", "claude-code, from this machine's own installation"),
		line("network", "any host (UNRESTRICTED)"),
		line("files", "read a, b; write workspace"),
		line("servers", "mcp-a@1.2.3, mcp-b@2"),
		line("hosts", "server"),
		line("browser", "sites x.example; hosts h.example; any public site"),
		line("inputs", "task from stdin, or the agent's own instruction when there is none; knowledge k1, k2"),
		line("budgets", "1m01s timeout, $2.50"),
		line("deliverables", "the files the agent writes under reports/, and a text result on stdout"),
		line("output", "/out/here (reports/, receipts/, result.json)"),
		line("plan", hash[:12]+" in /plans/y"),
		"nothing ran: --dry-run compiles the plan and stops; --json prints the plan itself",
	}
	if gotOther := dryRunSummary(other, "/plans/y", runOpts{out: "/out/here", knowledge: []string{"k1", "k2"}}); !slices.Equal(gotOther, wantOther) {
		t.Errorf("summary:\n%s\nwant:\n%s", strings.Join(gotOther, "\n"), strings.Join(wantOther, "\n"))
	}
	bare := &spec.Plan{Ref: "acme/bare@1.0.0", Source: "local", Mode: "autonomous", Capabilities: spec.CapSummary{Browser: &spec.BrowserCap{}}}
	bareHash, err := bare.Hash()
	if err != nil {
		t.Fatal(err)
	}
	wantBare := []string{
		"run plan for acme/bare@1.0.0 (local)",
		line("mode", "autonomous"),
		line("network", "none"),
		line("files", "read none; write none"),
		line("browser", "yes"),
		line("inputs", "task from stdin, or the agent's own instruction when there is none"),
		line("budgets", "0ms timeout"),
		line("deliverables", "the files the agent writes under reports/, and a text result on stdout"),
		line("output", "./agentry-out/<namespace>-<name>-<run id> (reports/, receipts/, result.json)"),
		line("plan", bareHash[:12]+" in /plans/z"),
		"nothing ran: --dry-run compiles the plan and stops; --json prints the plan itself",
	}
	if gotBare := dryRunSummary(bare, "/plans/z", runOpts{}); !slices.Equal(gotBare, wantBare) {
		t.Errorf("summary:\n%s\nwant:\n%s", strings.Join(gotBare, "\n"), strings.Join(wantBare, "\n"))
	}
}

// --dry-run without --json says the plan in words on stdout; with --json it
// prints the plan itself.
func TestDryRunWithoutJSONSaysThePlanInWords(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", pkg, "--allow-unsigned", "--yes", "--dry-run", "--policy", policyPath, "--file", "notes.txt")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{"run plan for fixtures/pure-text@1.0.0 (local)", "mode:        autonomous", "tools:       ", "inputs:      task from stdin", "1 file(s): notes.txt", "nothing ran"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "planVersion") || strings.Contains(stderr, "run plan sha256") {
		t.Errorf("the plan's JSON leaked into the words:\n%s\n%s", stdout, stderr)
	}
	// What the person is shown before the run goes on stderr: the release,
	// its summary and its capabilities.
	if want := "fixtures/pure-text@1.0.0 (autonomous)\n  Answers from its prompt alone — declares no capabilities at all\ncapabilities:\n  none declared (pure text agent)\n"; !strings.Contains(stderr, want) {
		t.Errorf("the consent screen on stderr:\n%s\nwant it to hold\n%s", stderr, want)
	}

	// A release the person installed from a folder keeps its own record; a
	// run of another tree of it says its consent is not kept.
	key := installdir.ConsentKey(home, "fixtures", "pure-text")
	var st installed.State
	st.Upsert(key, "1.0.0", installed.Snapshot(manifest.Capabilities{}), false)
	st.MarkLocal(key, "sha256:an-installed-tree")
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runCLI(t, strings.NewReader(""), "run", "--local", pkg, "--allow-unsigned", "--yes", "--dry-run", "--policy", policyPath)
	if err != nil || !strings.Contains(stderr, "note: fixtures/pure-text's record is its local install; this run's consent is not kept\n") {
		t.Errorf("a run of another tree of a locally installed agent: %v\n%s", err, stderr)
	}
}
