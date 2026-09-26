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
	b.WriteString("| Fixture | Exit | validate | Shape | Turns | Input tokens | Output tokens | Wall | Pages | Registry | Searches | Read | Withheld | Scan requests | Scan input tokens |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range fx {
		dir := filepath.Join(root, f.ID)
		var env receipt.Envelope
		raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
		if err != nil {
			fmt.Fprintf(&b, "| %s | not run | | | | | | | | | | | | | |\n", f.ID)
			continue
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: %v", f.ID, err)
		}
		valid, shape := "no draft", ""
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
		u := env.Usage
		fmt.Fprintf(&b, "| %s | %d %s | %s | %s | %d | %d | %d | %ds | %d | %d | %d | %d | %d | %d | %d |\n",
			f.ID, env.ExitCode, env.ExitClass, valid, shape, u.Turns, u.InputTokens, u.OutputTokens, u.WallMS/1000,
			w.Pages, w.RegistryCalls, w.Searches, w.Read, w.Withheld+w.NotScanned, w.ScanRequests, w.ScanInputTokens)
	}
	t.Log("\n" + b.String())
}
