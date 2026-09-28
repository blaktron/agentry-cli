package intake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

type scriptPrompter struct {
	answers map[string]string // prompt substring → answer
	asked   []string
}

func (s *scriptPrompter) Ask(prompt, def string) (string, error) {
	s.asked = append(s.asked, prompt)
	for k, v := range s.answers {
		if strings.Contains(prompt, k) {
			return v, nil
		}
	}
	return def, nil
}
func (s *scriptPrompter) AskSecret(prompt string) (string, error) { return s.Ask(prompt, "") }
func (s *scriptPrompter) Confirm(string, bool) (bool, error)      { return true, nil }
func (s *scriptPrompter) Say(string)                              {}

func questions() []manifest.IntakeQuestion {
	return []manifest.IntakeQuestion{
		{ID: "dataset", Prompt: "Path to the dataset", Type: manifest.IntakeFile, Required: true},
		{ID: "region", Prompt: "Which region?", Type: manifest.IntakeChoice, Choices: []string{"emea", "apac"}},
		{ID: "note", Prompt: "Anything else?"},
		{ID: "api_token", Prompt: "API token", Type: manifest.IntakeSecret},
	}
}

func TestCollectFromPrompterAndPreset(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data.csv")
	if err := os.WriteFile(data, []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &scriptPrompter{answers: map[string]string{"dataset": data, "region": "apac", "API token": "s3cret"}}
	a, err := Collect(p, questions(), map[string]string{"note": "preset wins"}, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Files["dataset"] != data || a.Values["region"] != "apac" || a.Values["note"] != "preset wins" || a.Secrets["api_token"] != "s3cret" {
		t.Errorf("answers = %+v", a)
	}
	for _, asked := range p.asked {
		if strings.Contains(asked, "Anything else") {
			t.Error("a preset answer was still asked")
		}
	}
	doc := a.Document("/inputs")
	if doc["dataset"] != "/inputs/data.csv" || doc["region"] != "apac" {
		t.Errorf("document = %v", doc)
	}
	if sec, _ := doc["api_token"].(map[string]string); sec["secret"] != "API_TOKEN" {
		t.Errorf("secret answer in document = %v", doc["api_token"])
	}
	for _, v := range doc {
		if s, ok := v.(string); ok && s == "s3cret" {
			t.Error("secret value leaked into the document")
		}
	}
	if got := a.FilePaths(); len(got) != 1 || got[0] != data {
		t.Errorf("file paths = %v", got)
	}
}

func TestCollectRefusals(t *testing.T) {
	data := filepath.Join(t.TempDir(), "d.csv")
	_ = os.WriteFile(data, []byte("x"), 0o644)
	cases := []struct {
		name    string
		preset  map[string]string
		secrets []string
		p       *scriptPrompter
		want    string
	}{
		{"required without terminal", map[string]string{}, []string{"API_TOKEN"}, nil, "is required"},
		{"unknown preset id", map[string]string{"nope": "x"}, []string{"API_TOKEN"}, nil, "no question with that id"},
		{"bad choice", map[string]string{"dataset": data, "region": "mars"}, []string{"API_TOKEN"}, nil, "is not one of"},
		{"missing file", map[string]string{"dataset": "/no/such/file"}, []string{"API_TOKEN"}, nil, "dataset"},
		{"secret without declared secret", map[string]string{"dataset": data}, nil, nil, "declares no capability secret named API_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p setup.Prompter // a nil *scriptPrompter must not become a non-nil interface
			if tc.p != nil {
				p = tc.p
			}
			_, err := Collect(p, questions(), tc.preset, tc.secrets)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
	if _, err := ParsePreset([]string{"novalue"}); err == nil {
		t.Error("bare --intake accepted")
	}
	if _, err := ParsePreset([]string{"a=1", "a=2"}); err == nil {
		t.Error("duplicate --intake accepted")
	}
	got, err := ParsePreset([]string{"a=1", "b=x=y"})
	if err != nil || got["a"] != "1" || got["b"] != "x=y" {
		t.Errorf("parse = %v, %v", got, err)
	}
}

// TestCollectAnswersASecretFromTheSource (agentry-cli#402): with no
// prompter and no preset, a secret question whose declared capability
// secret the run's source holds is answered from it — a hosted run's
// vault-filled envFile — and named, never valued; one the source does not
// hold still fails as required.
func TestCollectAnswersASecretFromTheSource(t *testing.T) {
	qs := []manifest.IntakeQuestion{{ID: "api_token", Prompt: "API token", Type: manifest.IntakeSecret, Required: true}}
	var looked []string
	holds := func(name string) bool { looked = append(looked, name); return name == "API_TOKEN" }

	a, err := CollectFrom(nil, qs, nil, []string{"API_TOKEN"}, holds)
	if err != nil {
		t.Fatalf("a secret the source holds failed intake: %v", err)
	}
	if len(looked) != 1 || looked[0] != "API_TOKEN" {
		t.Errorf("source asked for %v, want [API_TOKEN]", looked)
	}
	if _, has := a.Secrets["api_token"]; has {
		t.Error("the source's value was copied into the answers; it must stay in the source")
	}
	if sec, _ := a.Document("/inputs")["api_token"].(map[string]string); sec["secret"] != "API_TOKEN" {
		t.Errorf("document = %v, want the secret named", a.Document("/inputs"))
	}
	if a.Empty() {
		t.Error("an intake answered from the source reads as empty")
	}

	none := func(string) bool { return false }
	if _, err := CollectFrom(nil, qs, nil, []string{"API_TOKEN"}, none); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("a secret the source lacks: err = %v, want the required refusal", err)
	}
	if _, err := CollectFrom(nil, qs, nil, []string{"API_TOKEN"}, nil); err == nil {
		t.Fatal("with no source a required secret question must still fail")
	}

	// A terminal still asks: the person may mean another value than the
	// source holds, and the prompt is how they say so.
	p := &scriptPrompter{answers: map[string]string{"API token": "typed"}}
	a, err = CollectFrom(p, qs, nil, []string{"API_TOKEN"}, holds)
	if err != nil || a.Secrets["api_token"] != "typed" {
		t.Fatalf("with a terminal: answers = %+v, err = %v", a, err)
	}
}
