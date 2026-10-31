package pastruns

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// fixture is an output directory with two runs, a folder that is not a
// run and a stray file.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel string, v any) {
		t.Helper()
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ok := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "3f9a1c2e9b", Ref: "acme/weekly@1.0.0",
		Usage: receipt.Usage{WallMS: 72_000}, Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/summary.md"}, {Path: "reports/data.csv"}}}}
	write("acme-weekly-3f9a1c2e9b/result.json", ok)
	write("acme-weekly-3f9a1c2e9b/receipts/meta.json", map[string]string{"started": "2026-10-31T10:00:00Z", "ended": "2026-10-31T10:01:12Z"})
	failed := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "error", ExitCode: 11, RunID: "3fa0b1c2d3", Ref: "acme/weekly@1.0.0", Error: "refused: secret missing"}
	write("acme-weekly-3fa0b1c2d3/result.json", failed)
	write("acme-weekly-3fa0b1c2d3/receipts/meta.json", map[string]string{"started": "2026-10-31T11:00:00Z"})
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "result.json"), []byte(`{"not":"an envelope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestListReadsTheRunsNewestFirst(t *testing.T) {
	dir := fixture(t)
	runs, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2: %+v", len(runs), runs)
	}
	newest, oldest := runs[0], runs[1]
	if newest.RunID != "3fa0b1c2d3" || newest.Outcome() != "policy" || newest.ExitClassMeaning == "" || newest.Error != "refused: secret missing" {
		t.Errorf("newest: %+v", newest)
	}
	if !newest.Started.Equal(time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)) || !newest.Ended.Equal(newest.Started) {
		t.Errorf("a meta.json without an end reads the start for both: %v %v", newest.Started, newest.Ended)
	}
	if oldest.RunID != "3f9a1c2e9b" || oldest.Outcome() != "ok" || oldest.WallMS != 72_000 || strings.Join(oldest.Deliverables, ",") != "reports/summary.md,reports/data.csv" {
		t.Errorf("oldest: %+v", oldest)
	}
	if oldest.Dir != filepath.Join(dir, "acme-weekly-3f9a1c2e9b") {
		t.Errorf("dir = %q", oldest.Dir)
	}
}

func TestAMissingDirectoryListsNothing(t *testing.T) {
	runs, err := List(filepath.Join(t.TempDir(), "none"))
	if err != nil || runs != nil {
		t.Fatalf("(%v, %v)", runs, err)
	}
}

func TestARunWithoutMetaReadsTheEnvelopeFilesTime(t *testing.T) {
	dir := t.TempDir()
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "abcd1234"}
	b, _ := json.Marshal(env)
	run := filepath.Join(dir, "x-y-abcd1234")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Read(run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Started.IsZero() || !r.Started.Equal(r.Ended) || time.Since(r.Started) > time.Minute {
		t.Errorf("times: %v %v", r.Started, r.Ended)
	}
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "not a run folder") {
		t.Errorf("a folder without an envelope: %v", err)
	}
}

func TestFindByPathNameIdAndPrefix(t *testing.T) {
	dir := fixture(t)
	for _, c := range []struct{ name, want string }{
		{filepath.Join(dir, "acme-weekly-3f9a1c2e9b"), "3f9a1c2e9b"},
		{"acme-weekly-3fa0b1c2d3", "3fa0b1c2d3"},
		{"3fa0b1c2d3", "3fa0b1c2d3"},
		{"3f9a", "3f9a1c2e9b"},
		{"c2e9b", "3f9a1c2e9b"},
	} {
		r, err := Find(dir, c.name)
		if err != nil || r.RunID != c.want {
			t.Errorf("Find(%q) = %q, %v; want %q", c.name, r.RunID, err, c.want)
		}
	}
	for name, want := range map[string]string{
		"":      "name the run",
		"3f":    "no run",
		"3fa":   "no run",
		"zzzz":  "no run",
		"notes": "no run",
	} {
		if _, err := Find(dir, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Find(%q) = %v, want %q", name, err, want)
		}
	}
	// Two runs share the id prefix 3f: the first four characters must
	// decide, and here they do not.
	if _, err := Find(dir, "3f"); err == nil {
		t.Error("a two-character prefix found a run")
	}
	both := fixture(t)
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "3f9a9999"}
	b, _ := json.Marshal(env)
	run := filepath.Join(both, "acme-weekly-3f9a9999")
	_ = os.MkdirAll(run, 0o755)
	_ = os.WriteFile(filepath.Join(run, "result.json"), b, 0o644)
	if _, err := Find(both, "3f9a"); err == nil || !strings.Contains(err.Error(), "names 2 runs") {
		t.Errorf("an ambiguous prefix: %v", err)
	}
}
