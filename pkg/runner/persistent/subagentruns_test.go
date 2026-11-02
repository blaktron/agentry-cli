package persistent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeSubagentLog writes lines as a segment's receipts/subagents.ndjson.
func writeSubagentLog(t *testing.T, home, name, segment string, lines ...string) {
	t.Helper()
	dir := filepath.Join(Dir(home, name), segmentsDir, segment, "receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, subagentsLog), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentRunsReadsEverySegmentNewestFirst(t *testing.T) {
	home := t.TempDir()
	writeSubagentLog(t, home, "acme-helper", "run-1",
		`{"time":"2026-11-01T10:00:00Z","event":"started","run":"sa-000000000001","subagent":"acme/digest@1.0.0","grantedBy":"release","files":["notes.md"]}`,
		`{"time":"2026-11-01T10:02:00Z","event":"ended","run":"sa-000000000001","subagent":"acme/digest@1.0.0","outcome":"ok","runId":"r-1","outDir":"/h/out-1","copied":["subagents/sa-000000000001/digest.md"]}`,
		`not a record`,
		`{"time":"2026-11-01T11:00:00Z","event":"started","run":"sa-000000000002","subagent":"acme/fetch@0.2.0","grantedBy":"you"}`,
	)
	writeSubagentLog(t, home, "acme-helper", "run-2",
		`{"time":"2026-11-02T09:00:00Z","event":"ended","run":"sa-000000000003","subagent":"acme/digest@1.0.0","outcome":"error","error":"acme/digest@1.0.0 is quarantined"}`,
		`{"time":"2026-11-02T08:00:00Z","event":"started","run":"sa-000000000004","subagent":"acme/digest@1.0.0","grantedBy":"helper"}`,
		`{"time":"2026-11-02T08:05:00Z","event":"ended","run":"sa-000000000004","subagent":"acme/digest@1.0.0","outcome":"failed","runId":"r-4"}`,
	)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	want := []SubagentRunRecord{
		{ID: "sa-000000000003", Subagent: "acme/digest@1.0.0", Started: at("2026-11-02T09:00:00Z"), Ended: at("2026-11-02T09:00:00Z"), Outcome: "error", Error: "acme/digest@1.0.0 is quarantined", Segment: "run-2"},
		{ID: "sa-000000000004", Subagent: "acme/digest@1.0.0", GrantedBy: "helper", Started: at("2026-11-02T08:00:00Z"), Ended: at("2026-11-02T08:05:00Z"), Outcome: "failed", RunID: "r-4", Segment: "run-2"},
		{ID: "sa-000000000002", Subagent: "acme/fetch@0.2.0", GrantedBy: "you", Started: at("2026-11-01T11:00:00Z"), Segment: "run-1"},
		{ID: "sa-000000000001", Subagent: "acme/digest@1.0.0", GrantedBy: "release", Files: []string{"notes.md"}, Started: at("2026-11-01T10:00:00Z"), Ended: at("2026-11-01T10:02:00Z"), Outcome: "ok", RunID: "r-1", OutDir: "/h/out-1", Copied: []string{"subagents/sa-000000000001/digest.md"}, Segment: "run-1"},
	}
	if got := SubagentRuns(home, "acme-helper", 50); !reflect.DeepEqual(got, want) {
		t.Fatalf("runs:\n got %+v\nwant %+v", got, want)
	}
	if got := SubagentRuns(home, "acme-helper", 2); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("bounded to 2:\n got %+v\nwant %+v", got, want[:2])
	}
	if got := SubagentRuns(home, "nobody", 50); len(got) != 0 {
		t.Fatalf("an agent with no segments has runs: %+v", got)
	}
}
