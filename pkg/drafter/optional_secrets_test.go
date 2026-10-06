package drafter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/validate"
	"github.com/blaktron/agentry/pkg/manifest"
)

// question is the secret question a keyedDraft asks for its key.
type question int

const (
	noQuestion  question = iota
	notRequired          // a secret question with no required
	required             // a secret question with required: true
)

// keyedDraft is a draft whose QuickAgent fetches from GitHub with
// GITHUB_TOKEN, the key marked optional or not, and asking q for it
// (optional secrets M6: plan optional-secrets.md D3, D7; agentry-cli#652).
func keyedDraft(optional bool, q question) *Draft {
	caps := `"network": {"egress": ["api.github.com"]}, "filesystem": {"read": "none", "write": "none"}, "exec": "none", "secrets": ["GITHUB_TOKEN"]`
	if optional {
		caps += `, "optionalSecrets": ["GITHUB_TOKEN"]`
	}
	intake := ""
	switch q {
	case notRequired:
		intake = `"intake": [{"id": "github_token", "prompt": "A GitHub token for private repositories", "type": "secret"}],`
	case required:
		intake = `"intake": [{"id": "github_token", "prompt": "A GitHub token for private repositories", "type": "secret", "required": true}],`
	}
	return &Draft{
		Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "issue-summary", "namespace": "draft", "version": "0.1.0",
			"summary": "Summarises a repository's open issues.", "license": "Apache-2.0",
			"modes": ["autonomous"], "entrypoints": {"instruction": "AGENT.md"}, ` + intake + `
			"capabilities": {` + caps + `},
			"guide": {"connect": {"GITHUB_TOKEN": "A fine-grained token with read access to issues."}},
			"publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}}`),
		Instructions: "# Issue summary\n\n## Role\n\nSummarise a repository's open issues.\n\n## Inputs\n\nThe repository named in the task.\n\n" +
			"## Steps\n\n1. Read the open issues from api.github.com. If GITHUB_TOKEN was not given, read the repository only if it is public, and say so.\n\n" +
			"## Output\n\nA report of each issue's title, labels and age.\n\n## Background\n\nThe GitHub REST API lists issues per repository.\n\n## Checks\n\nEvery issue listed is open.\n",
	}
}

// checkMarking runs the post-step's own check on d: whether it passed, and
// whether its report refuses the intake question under rule V10.
func checkMarking(t *testing.T, d *Draft) (passed bool, refusedOnIntake bool) {
	t.Helper()
	msg, raw := PostStep{OutDir: t.TempDir()}.renderAndValidate(d)
	if msg != "" {
		t.Fatalf("the draft could not be checked: %s", msg)
	}
	var r validate.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r.Passed, strings.Contains(string(raw), "V10") && strings.Contains(string(raw), "intake[0].required")
}

func TestThePostStepChecksAKeysMarking(t *testing.T) {
	for _, tc := range []struct {
		name      string
		d         *Draft
		passes    bool
		v10Refuse bool
	}{
		{"an optional key", keyedDraft(true, noQuestion), true, false},
		{"a required key", keyedDraft(false, noQuestion), true, false},
		{"an optional key's question, not required", keyedDraft(true, notRequired), true, false},
		{"a required key's question, required", keyedDraft(false, required), true, false},
		{"an optional key's question marked required (D7)", keyedDraft(true, required), false, true},
		{"a required key's question not marked required (D7)", keyedDraft(false, notRequired), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			passed, refused := checkMarking(t, tc.d)
			if passed != tc.passes || refused != tc.v10Refuse {
				t.Fatalf("passed %v (want %v), refused by V10 on the intake %v (want %v)", passed, tc.passes, refused, tc.v10Refuse)
			}
		})
	}
}

func TestTheBenchHoldsADraftToItsDescriptionsMarking(t *testing.T) {
	shape := func(d *Draft, e Expect) string {
		t.Helper()
		yml, err := manifestYAML(d.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		m, err := manifest.Parse(yml)
		if err != nil {
			t.Fatal(err)
		}
		e.Modes, e.Hosts, e.MaxSecrets = []string{"autonomous"}, []string{"api.github.com"}, 1
		return strings.Join(CheckShape(m, d, e), "; ")
	}
	if got := shape(keyedDraft(true, noQuestion), Expect{Keys: "optional"}); got != "" {
		t.Fatalf("an optional key where the description makes it optional: %s", got)
	}
	if got := shape(keyedDraft(false, noQuestion), Expect{Keys: "optional"}); !strings.Contains(got, "key GITHUB_TOKEN is required") {
		t.Fatalf("a required key where the description makes it optional: %q", got)
	}
	if got := shape(keyedDraft(true, noQuestion), Expect{Keys: "required"}); !strings.Contains(got, "key GITHUB_TOKEN is optional") {
		t.Fatalf("an optional key where the task cannot run without it: %q", got)
	}
	if got := shape(keyedDraft(false, noQuestion), Expect{}); got != "" {
		t.Fatalf("a fixture that says nothing of the marking: %s", got)
	}
	if got := shape(keyedDraft(true, noQuestion), Expect{Keys: "optional", MinSecrets: 2}); !strings.Contains(got, "1 keys, want at least 2") {
		t.Fatalf("a draft short of the keys its description names: %q", got)
	}
}

// A repair's second answer that moves a key between optional and required
// keeps every key it declared, so it drops nothing (agentry-cli#633's
// rule reads the declared names, never the marking).
func TestMovingAKeyBetweenOptionalAndRequiredIsNoDrop(t *testing.T) {
	first, second := keyedDraft(false, noQuestion), keyedDraft(true, noQuestion)
	second.Validation = json.RawMessage(`{"passed":true}`)
	if got := droppedUnnamed(first, second, RepairReport{}); len(got) != 0 {
		t.Fatalf("required → optional dropped %v", got)
	}
	if got := droppedUnnamed(second, first, RepairReport{}); len(got) != 0 {
		t.Fatalf("optional → required dropped %v", got)
	}
}
