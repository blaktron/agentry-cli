package intake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
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
			_, err := Collect(tc.p, questions(), tc.preset, tc.secrets)
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
