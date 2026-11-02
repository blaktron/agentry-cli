package persistent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// MaxSubagentRunsShown is how many of an agent's subagent runs a client
// lists.
const MaxSubagentRunsShown = 50

// SubagentRunRecord is one run an agent started of a subagent, as its
// segment's receipts/subagents.ndjson records it: the subagent's exact
// reference, who granted it, when the run started and ended, how it ended,
// and the run's own id and output folder.
type SubagentRunRecord struct {
	ID       string `json:"id"`
	Subagent string `json:"subagent"`
	// GrantedBy is the roster's grant the run was started under: the
	// release's pin, one the person added, or a helper run.
	GrantedBy string    `json:"grantedBy,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
	// Outcome is the run's status (ok, failed, …), or error when it did
	// not run, with Error saying why; empty while it runs, or when its
	// segment ended before it did.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// RunID is the run's own id, and OutDir its output folder, which
	// holds its receipts.
	RunID  string `json:"runId,omitempty"`
	OutDir string `json:"outDir,omitempty"`
	// Files are the agent's files it was given, and Copied the files it
	// delivered, as copied into the agent's files.
	Files  []string `json:"files,omitempty"`
	Copied []string `json:"copied,omitempty"`
	// Segment is the segment that started it.
	Segment string `json:"segment"`
}

// SubagentRuns is the agent's subagent runs across every segment's record,
// newest first, at most limit. What cannot be read (a file, a line) is
// skipped: the records are a report, and a damaged one hides nothing else.
func SubagentRuns(home, name string, limit int) []SubagentRunRecord {
	dir := filepath.Join(Dir(home, name), segmentsDir)
	segments, _ := os.ReadDir(dir)
	var runs []*SubagentRunRecord
	byID := map[string]*SubagentRunRecord{}
	for _, seg := range segments {
		if !seg.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, seg.Name(), "receipts", subagentsLog)) //nolint:gosec // the agent's own receipts
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var l subagentLogLine
			if json.Unmarshal(sc.Bytes(), &l) != nil || l.Run == "" || l.Time.IsZero() {
				continue
			}
			r := byID[l.Run]
			if r == nil {
				r = &SubagentRunRecord{ID: l.Run, Subagent: l.Subagent, Segment: seg.Name()}
				byID[l.Run] = r
				runs = append(runs, r)
			}
			switch l.Event {
			case "started":
				r.Started, r.GrantedBy, r.Files = l.Time, l.GrantedBy, l.Files
			case "ended":
				r.Ended, r.Outcome, r.Error, r.RunID, r.Copied = l.Time, l.Outcome, l.Error, l.RunID, l.Copied
				r.OutDir = l.OutDir
				// An end whose start did not read starts at its end.
				if r.Started.IsZero() {
					r.Started = l.Time
				}
			}
		}
		_ = f.Close()
	}
	slices.SortStableFunc(runs, func(a, b *SubagentRunRecord) int { return b.Started.Compare(a.Started) })
	out := make([]SubagentRunRecord, 0, min(len(runs), max(limit, 0)))
	for _, r := range runs {
		if len(out) == limit {
			break
		}
		out = append(out, *r)
	}
	return out
}
