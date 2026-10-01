package drafter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheFixtureRuns reads a fixture run's results and prints the
// measurement table of measurements/drafter.md (plan D10): per fixture, the
// run's exit, whether the rendered package passes validate, how the draft
// departs from the shape expected, the model's tokens and turns, the web
// tools' calls and AI Review's counts. It runs only when
// AGENTRY_DRAFTER_RESULTS names a directory holding one `agentry draft --out`
// folder per fixture id; a fixture with no folder is reported as not run.
func TestTheFixtureRuns(t *testing.T) {
	root := os.Getenv("AGENTRY_DRAFTER_RESULTS")
	if root == "" {
		t.Skip("AGENTRY_DRAFTER_RESULTS is not set")
	}
	fx, err := LoadFixtures()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("| Fixture | Exit | validate | Corrected | Repaired | Shape | Instruction | off_task | Rubric | Turns | Input tokens | Output tokens | Credits | Wall | Pages | Registry | Searches | Read | Withheld | Scan requests | Scan input tokens |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range fx {
		dir := filepath.Join(root, f.ID)
		var env receipt.Envelope
		raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
		if err != nil {
			fmt.Fprintf(&b, "| %s | not run | | | | | | | | | | | | | | | | | | | |\n", f.ID)
			continue
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: %v", f.ID, err)
		}
		valid, shape, corrected, repaired := "no draft", "", 0, "no"
		instBytes, offTask, rubric := 0, "", ""
		if db, err := os.ReadFile(filepath.Join(dir, "draft.json")); err == nil {
			var d Draft
			if err := json.Unmarshal(db, &d); err != nil {
				t.Fatalf("%s: %v", f.ID, err)
			}
			var v struct {
				Passed bool `json:"passed"`
				Counts struct {
					Errors   int `json:"errors"`
					Findings int `json:"findings"`
				} `json:"counts"`
			}
			switch {
			case d.ManifestError != "":
				valid = "does not parse"
			case json.Unmarshal(d.Validation, &v) == nil && v.Passed:
				valid = "passes"
			default:
				valid = fmt.Sprintf("%d error(s), %d finding(s)", v.Counts.Errors, v.Counts.Findings)
			}
			corrected = sectionBullets(d.Notes, "## Corrected after drafting")
			instBytes = len(d.Instructions)
			offTask, rubric = checkColumns(d.AIReview)
			switch {
			case strings.Contains(d.Notes, "## Repaired once"):
				repaired = "yes"
			case strings.Contains(d.Notes, "## Not repaired"):
				repaired = "no draft"
			}
			if yml, err := manifestYAML(d.Manifest); err == nil {
				if m, err := manifest.Parse(yml); err == nil {
					if dev := CheckShape(m, &d, f.Expect); len(dev) > 0 {
						shape = strings.Join(dev, "; ")
					} else {
						shape = "as expected"
					}
				}
			}
		}
		w := env.Web
		if w == nil {
			w = &receipt.Web{}
		}
		// The repair turn's usage is the draft's too (its own result.json).
		u := env.Usage
		if raw, err := os.ReadFile(filepath.Join(dir, "repair", "result.json")); err == nil {
			var r receipt.Envelope
			if json.Unmarshal(raw, &r) == nil {
				u.Turns += r.Usage.Turns
				u.InputTokens += r.Usage.InputTokens
				u.OutputTokens += r.Usage.OutputTokens
				u.WallMS += r.Usage.WallMS
			}
		}
		fmt.Fprintf(&b, "| %s | %d %s | %s | %d | %s | %s | %d | %s | %s | %d | %d | %d | %d | %ds | %d | %d | %d | %d | %d | %d | %d |\n",
			f.ID, env.ExitCode, env.ExitClass, valid, corrected, repaired, shape, instBytes, offTask, rubric,
			u.Turns, u.InputTokens, u.OutputTokens, tokenCredits(u.InputTokens, u.OutputTokens), u.WallMS/1000,
			w.Pages, w.RegistryCalls, w.Searches, w.Read, w.Withheld+w.NotScanned, w.ScanRequests, w.ScanInputTokens)
	}
	t.Log("\n" + b.String())
}

// checkColumns are AI Review's check of the finished draft as the table
// shows it: off_task's score (or why it was not asked) and the rubric's
// verdict with its findings.
func checkColumns(a *AIReview) (offTask, rubric string) {
	if a == nil || a.Check == nil {
		return "no check", "no check"
	}
	c := a.Check
	switch {
	case c.Draft == nil:
		offTask = "unanswered"
	case c.Draft.Failure != "":
		offTask = c.Draft.Verdict + " (" + c.Draft.Failure + ")"
	default:
		offTask = fmt.Sprint(c.Draft.Scores["off_task"])
	}
	switch {
	case c.Rubric == nil:
		rubric = "unanswered"
	case len(c.Rubric.Findings) == 0:
		rubric = c.Rubric.Verdict
	default:
		var ids []string
		for _, f := range c.Rubric.Findings {
			// A rubric id opens with its axis (C1–C6), what the table shows.
			id, _, _ := strings.Cut(f.ID, "_")
			ids = append(ids, fmt.Sprintf("%s %d", id, f.Score))
		}
		rubric = c.Rubric.Verdict + ": " + strings.Join(ids, ", ")
	}
	return offTask, rubric
}

// tokenCredits is the model's tokens at the hosted draft's rate (plan
// builder-rework.md D29): max(1, ceil(input ÷ 50,000 + output ÷ 25,000)),
// for measurements/drafter.md's table alone. The rate itself is the
// enterprise build's, which settles a hosted draft; a draft on this
// computer is never worded in credits. AI Review's one credit (DEV-150) is
// not in it.
func tokenCredits(in, out int64) int64 {
	return max(1, (in+2*out+49_999)/50_000)
}

// sectionBullets counts the list items of one section of the notes.
func sectionBullets(notes, heading string) int {
	i := strings.Index(notes, heading)
	if i < 0 {
		return 0
	}
	rest := notes[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return strings.Count(rest, "\n- ")
}
