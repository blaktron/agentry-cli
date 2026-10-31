package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
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
	bare := runSummary(&receipt.Envelope{Status: "ok"})
	if len(bare) != 2 || bare[1] != "no files written; the result is the output above" {
		t.Errorf("a run with nothing: %q", bare)
	}
}

func TestManyFilesAreCounted(t *testing.T) {
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
	report := failureReport(engine.Classify(engine.ExitHarness, errors.New("agent exited 1\n\x1b]0;x\x07tail")), "")
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
	plain := failureReport(errors.New("intake: no answer"), "")
	if plain[0] != "agentry: run failed: cli: "+engine.ExitClassOf(1).Meaning || len(plain) != 3 || !strings.HasSuffix(plain[2], "unlikely to help") {
		t.Errorf("an unclassified error reads as cli, with no receipts line: %q", plain)
	}
}

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
	_ = reportFailure(&out, errors.New("intake: the answer was not read"), true, filepath.Join(dir, "none"))
	if !strings.Contains(out.String(), "run failed: cli:") || !strings.Contains(out.String(), "cause: interrupted") || strings.Contains(out.String(), "receipts:") {
		t.Errorf("an interrupted run, with no receipts folder on disk:\n%s", out.String())
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
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		850 * time.Millisecond:                "850ms",
		12 * time.Second:                      "12s",
		72*time.Second + 400*time.Millisecond: "1m12s",
		2*time.Hour + 5*time.Minute:           "2h05m",
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
}

// --dry-run without --json says the plan in words on stdout; with --json it
// prints the plan itself.
func TestDryRunWithoutJSONSaysThePlanInWords(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
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
}
