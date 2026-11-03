package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// writeRun leaves a run's output folder under work/agentry-out: its envelope
// and the receipts' timing.
func writeRun(t *testing.T, work, rel string, env receipt.Envelope, meta string) {
	t.Helper()
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

// outputDirWithRuns is a working folder whose agentry-out holds one finished
// and one failed run.
func outputDirWithRuns(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	t.Chdir(work)
	write := func(rel string, env receipt.Envelope, meta string) { writeRun(t, work, rel, env, meta) }
	write("acme-weekly-3f9a1c2e", receipt.Envelope{Status: "ok", RunID: "3f9a1c2e", Ref: "acme/weekly@1.0.0", Usage: receipt.Usage{WallMS: 72_000},
		Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/summary.md"}}}},
		`{"started":"2026-10-31T10:00:00Z","ended":"2026-10-31T10:01:12Z"}`)
	write("acme-weekly-7b2d0e11", receipt.Envelope{Status: "error", ExitCode: 13, RunID: "7b2d0e11", Ref: "acme/weekly@1.0.0", Error: "gateway did not start"},
		`{"started":"2026-10-31T12:00:00Z","ended":"2026-10-31T12:00:05Z"}`)
	return work
}

func TestRunsListsTheRunsNewestFirst(t *testing.T) {
	isolateEnv(t)
	work := outputDirWithRuns(t)
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

	// Each column is the run's own: when it started (in local time), its
	// outcome, how long it took, its QuickAgent and its folder.
	started := func(h int) string {
		return time.Date(2026, 10, 31, h, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	}
	if got, want := strings.Fields(lines[0]), []string{"STARTED", "OUTCOME", "TOOK", "QUICKAGENT", "FOLDER"}; !slices.Equal(got, want) {
		t.Errorf("header = %q, want %q", got, want)
	}
	if got, want := strings.Join(strings.Fields(lines[1]), " "), started(12)+" gateway 5s acme/weekly@1.0.0 "+filepath.Join("agentry-out", "acme-weekly-7b2d0e11"); got != want {
		t.Errorf("the failed run = %q, want %q", got, want)
	}
	if got, want := strings.Join(strings.Fields(lines[2]), " "), started(10)+" ok 1m12s acme/weekly@1.0.0 "+filepath.Join("agentry-out", "acme-weekly-3f9a1c2e"); got != want {
		t.Errorf("the finished run = %q, want %q", got, want)
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

	// What a folder says of itself is cleaned for the terminal and for the
	// columns; a run that recorded no end has no duration to show.
	writeRun(t, work, "acme-backwards-dddd4444", receipt.Envelope{Status: "ok", RunID: "dddd4444", Ref: "acme/backwards@1.0.0"},
		`{"started":"2026-10-31T07:00:10Z","ended":"2026-10-31T07:00:00Z"}`)
	if runtime.GOOS != "windows" { // a folder name with control characters
		writeRun(t, work, "acme-odd\tfolder\x1b[31m-eeee5555", receipt.Envelope{Status: "ok", RunID: "eeee5555", Ref: "acme/odd@1.0.0"}, `{"started":"2026-10-31T06:00:00Z"}`)
	}
	writeRun(t, work, "acme-slow-bbbb2222", receipt.Envelope{Status: "ok", RunID: "bbbb2222", Ref: "acme/slow@1.0.0", Usage: receipt.Usage{WallMS: 90_000}},
		`{"started":"2026-10-31T08:00:00Z","ended":"2026-10-31T08:00:03Z"}`)
	writeRun(t, work, "acme-evil-aaaa1111", receipt.Envelope{Status: "ok", RunID: "aaaa1111", Ref: "acme/evil\x1b[31m\tred@1.0.0\nnext"},
		`{"started":"2026-10-31T09:00:00Z"}`)
	stdout, _, err = runCLI(t, nil, "runs")
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(stdout), "\n")
	wantLines := 5 + 1
	if runtime.GOOS != "windows" {
		wantLines++
	}
	if len(lines) != wantLines || strings.ContainsAny(stdout, "\x1b\t") {
		t.Fatalf("a hostile ref broke the listing:\n%q", stdout)
	}
	if got := strings.Fields(lines[3]); len(got) < 5 || got[0]+" "+got[1] != started(9) || got[2] != "ok" || got[3] != "-" || got[len(got)-1] != filepath.Join("agentry-out", "acme-evil-aaaa1111") {
		t.Errorf("the run that recorded no end = %q", got)
	}
	// The measured duration is the one shown, whatever the timestamps span.
	if got, want := strings.Join(strings.Fields(lines[4]), " "), started(8)+" ok 1m30s acme/slow@1.0.0 "+filepath.Join("agentry-out", "acme-slow-bbbb2222"); got != want {
		t.Errorf("the run with a measured duration = %q, want %q", got, want)
	}
	if got := strings.Fields(lines[5]); len(got) != 6 || got[3] != "-" || got[4] != "acme/backwards@1.0.0" {
		t.Errorf("a run that ended before it started = %q", got)
	}
	if runtime.GOOS != "windows" {
		if got := lines[6]; !strings.Contains(got, "acme-odd folder?[31m-eeee5555") {
			t.Errorf("a folder name with control characters = %q", got)
		}
	}
	if fields := strings.Fields(lines[3]); !strings.Contains(strings.Join(fields[4:len(fields)-1], " "), "red@1.0.0 next") || !strings.Contains(lines[3], "?[31m red@1.0.0 next ") {
		t.Errorf("the hostile ref = %q: its tab and line break are single spaces inside its own cell", lines[3])
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
	stdout, _, err = runCLI(t, nil, "runs", "--dir", "elsewhere")
	if err != nil || stdout != "no runs under elsewhere\n" {
		t.Fatalf("(%q, %v)", stdout, err)
	}
	// A directory that cannot be read is no empty listing.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, nil, "runs", "--dir", file)
	wantClassified(t, err, engine.ExitCLI)
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

	// A file manager that fails is named with the path and its own error.
	openFolder = func(string) error { return errors.New("no display") }
	_, _, err = runCLI(t, nil, "open", "7b2d0e11")
	wantClassified(t, err, engine.ExitCLI)
	if want := "open " + filepath.Join(work, "agentry-out", "acme-weekly-7b2d0e11") + ": no display"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	openFolder = func(string) error { return &exec.Error{Name: "xdg-open", Err: exec.ErrNotFound} }
	stdout, _, err = runCLI(t, nil, "open", "7b2d0e11")
	wantClassified(t, err, engine.ExitCLI)
	if !strings.Contains(err.Error(), "no program to open a folder") || !strings.Contains(stdout, "acme-weekly-7b2d0e11") {
		t.Errorf("without a file manager the path is still printed: %q, %v", stdout, err)
	}
}
