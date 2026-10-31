package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// outputDirWithRuns is a working folder whose agentry-out holds one finished
// and one failed run.
func outputDirWithRuns(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	t.Chdir(work)
	write := func(rel string, env receipt.Envelope, meta string) {
		dir := filepath.Join(work, "agentry-out", rel)
		if err := os.MkdirAll(filepath.Join(dir, "receipts"), 0o755); err != nil {
			t.Fatal(err)
		}
		env.Agentry, env.OutDir = receipt.EnvelopeVersion, dir
		if err := env.Write(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "receipts", "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("acme-weekly-3f9a1c2e", receipt.Envelope{Status: "ok", RunID: "3f9a1c2e", Ref: "acme/weekly@1.0.0", Usage: receipt.Usage{WallMS: 72_000},
		Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/summary.md"}}}},
		`{"started":"2026-10-31T10:00:00Z","ended":"2026-10-31T10:01:12Z"}`)
	write("acme-weekly-7b2d0e11", receipt.Envelope{Status: "error", ExitCode: 13, RunID: "7b2d0e11", Ref: "acme/weekly@1.0.0", Error: "gateway did not start"},
		`{"started":"2026-10-31T12:00:00Z","ended":"2026-10-31T12:00:05Z"}`)
	return work
}

func TestRunsListsTheRunsNewestFirst(t *testing.T) {
	isolateEnv(t)
	outputDirWithRuns(t)
	stdout, _, err := runCLI(t, nil, "runs")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "STARTED") {
		t.Fatalf("listing:\n%s", stdout)
	}
	if !strings.Contains(lines[1], "gateway") || !strings.Contains(lines[1], "7b2d0e11") || !strings.Contains(lines[1], "5s") {
		t.Errorf("the newest run first, with its class and duration: %q", lines[1])
	}
	if !strings.Contains(lines[2], "ok") || !strings.Contains(lines[2], "1m12s") || !strings.Contains(lines[2], "acme/weekly@1.0.0") {
		t.Errorf("the finished run: %q", lines[2])
	}

	stdout, _, err = runCLI(t, nil, "--json", "runs")
	if err != nil {
		t.Fatal(err)
	}
	var out runsOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if out.Dir != "agentry-out" || len(out.Runs) != 2 || out.Runs[0].ExitClass != "gateway" || !out.Runs[0].RetryLikelyHelps || out.Runs[0].ExitClassMeaning == "" {
		t.Errorf("runs --json: %+v", out)
	}
	if out.Runs[1].Deliverables[0] != "reports/summary.md" {
		t.Errorf("deliverables: %v", out.Runs[1].Deliverables)
	}
}

func TestRunsWithNoRunsSaysSo(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir())
	stdout, _, err := runCLI(t, nil, "runs")
	if err != nil || !strings.Contains(stdout, "no runs under agentry-out") {
		t.Fatalf("(%q, %v)", stdout, err)
	}
	stdout, _, err = runCLI(t, nil, "--json", "runs", "--dir", "elsewhere")
	if err != nil || !strings.Contains(stdout, `"runs":[]`) || !strings.Contains(stdout, `"dir":"elsewhere"`) {
		t.Fatalf("(%q, %v)", stdout, err)
	}
}

func TestOpenFindsTheRunAndHandsItToTheFileManager(t *testing.T) {
	isolateEnv(t)
	work := outputDirWithRuns(t)
	var opened string
	saved := openFolder
	openFolder = func(path string) error { opened = path; return nil }
	t.Cleanup(func() { openFolder = saved })

	stdout, _, err := runCLI(t, nil, "open", "3f9a")
	want := filepath.Join(work, "agentry-out", "acme-weekly-3f9a1c2e")
	if err != nil || strings.TrimSpace(stdout) != want || opened != want {
		t.Fatalf("open: (%q, %v), opened %q, want %q", stdout, err, opened, want)
	}

	opened = ""
	stdout, _, err = runCLI(t, nil, "open", "--print", "acme-weekly-7b2d0e11")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(stdout), "acme-weekly-7b2d0e11") || opened != "" {
		t.Fatalf("open --print: (%q, %v), opened %q", stdout, err, opened)
	}

	_, _, err = runCLI(t, nil, "open", "nonesuch")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "agentry runs lists them") {
		t.Errorf("error = %v", err)
	}

	openFolder = func(string) error { return &exec.Error{Name: "xdg-open", Err: exec.ErrNotFound} }
	stdout, _, err = runCLI(t, nil, "open", "7b2d0e11")
	wantClassified(t, err, engine.ExitCLI)
	if !strings.Contains(err.Error(), "no program to open a folder") || !strings.Contains(stdout, "acme-weekly-7b2d0e11") {
		t.Errorf("without a file manager the path is still printed: %q, %v", stdout, err)
	}
}
