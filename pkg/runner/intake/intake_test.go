package intake

import (
	"os"
	"path/filepath"
	"slices"
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

func TestCollectWithSourcePrompterAndPreset(t *testing.T) {
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

	a, err := CollectWithSource(nil, qs, nil, []string{"API_TOKEN"}, holds)
	if err != nil {
		t.Fatalf("a secret the source holds failed intake: %v", err)
	}
	if len(looked) != 1 || looked[0] != "API_TOKEN" {
		t.Errorf("source asked for %v, want [API_TOKEN]", looked)
	}
	if len(a.Secrets) != 0 || !a.FromSource["api_token"] {
		t.Errorf("answers = %+v; want api_token answered from the source with no value held here", a)
	}
	if sec, _ := a.Document("/inputs")["api_token"].(map[string]string); sec["secret"] != "API_TOKEN" {
		t.Errorf("document = %v, want the secret named", a.Document("/inputs"))
	}
	if a.Empty() {
		t.Error("an intake answered from the source reads as empty")
	}

	none := func(string) bool { return false }
	if _, err := CollectWithSource(nil, qs, nil, []string{"API_TOKEN"}, none); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("a secret the source lacks: err = %v, want the required refusal", err)
	}
	if _, err := CollectWithSource(nil, qs, nil, []string{"API_TOKEN"}, nil); err == nil {
		t.Fatal("with no source a required secret question must still fail")
	}

	// A preset wins over the source, as it wins over a terminal.
	a, err = CollectWithSource(nil, qs, map[string]string{"api_token": "given"}, []string{"API_TOKEN"}, holds)
	if err != nil || a.Secrets["api_token"] != "given" || a.FromSource["api_token"] {
		t.Fatalf("with a preset: answers = %+v, err = %v", a, err)
	}

	// An optional secret question the source holds is recorded as answered
	// from it too: the secret reaches the run either way, so intake.json
	// says so rather than leaving the question out.
	opt := []manifest.IntakeQuestion{{ID: "api_token", Prompt: "API token", Type: manifest.IntakeSecret}}
	if a, err = CollectWithSource(nil, opt, nil, []string{"API_TOKEN"}, holds); err != nil || !a.FromSource["api_token"] {
		t.Fatalf("an optional question: answers = %+v, err = %v", a, err)
	}

	// A terminal still asks: the person may mean another value than the
	// source holds, and the prompt is how they say so.
	p := &scriptPrompter{answers: map[string]string{"API token": "typed"}}
	a, err = CollectWithSource(p, qs, nil, []string{"API_TOKEN"}, holds)
	if err != nil || a.Secrets["api_token"] != "typed" {
		t.Fatalf("with a terminal: answers = %+v, err = %v", a, err)
	}
}

// TestABlankSecretQuestionThatIsNotRequiredIsNoAnswer pins what DR-70
// builds on rather than anything it changed: a secret question that is not
// required, left blank with no terminal and no source holding it, is no
// answer and no error. The refusal such a run used to meet came from the
// engine, and with the secret optional the engine now starts the run
// without it (its TestAMissingOptionalSecretStartsTheRun).
func TestABlankSecretQuestionThatIsNotRequiredIsNoAnswer(t *testing.T) {
	qs := []manifest.IntakeQuestion{{ID: "github_token", Prompt: "A GitHub token, for private repositories", Type: manifest.IntakeSecret}}
	none := func(string) bool { return false }
	a, err := CollectWithSource(nil, qs, nil, []string{"GITHUB_TOKEN"}, none)
	if err != nil {
		t.Fatalf("a blank optional secret question refused: %v", err)
	}
	if len(a.Secrets) != 0 || a.FromSource["github_token"] {
		t.Fatalf("answers = %+v; want none", a)
	}
}

func TestASourceQuestionIsParsedAndNeverText(t *testing.T) {
	qs := []manifest.IntakeQuestion{
		{ID: "repo", Prompt: "Repository", Type: manifest.IntakeRepository, Required: true},
	}
	a, err := Collect(nil, qs, map[string]string{"repo": "https://github.com/o/r#v2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Sources["repo"]
	if s == nil || s.URL != "https://github.com/o/r" || s.Ref != "v2" || s.ID != "repo" || len(a.Values) != 0 {
		t.Fatalf("answers %+v (source %+v)", a, s)
	}
	if _, err := Collect(nil, qs, map[string]string{"repo": "http://github.com/o/r"}, nil); err == nil {
		t.Error("an http repository passed")
	}
	url := []manifest.IntakeQuestion{{ID: "doc", Prompt: "Doc", Type: manifest.IntakeURL}}
	a, err = Collect(nil, url, map[string]string{"doc": "https://example.com/a.pdf"}, nil)
	if err != nil || a.Sources["doc"] == nil || a.Sources["doc"].Kind != manifest.IntakeURL || len(a.Values) != 0 {
		t.Errorf("a url question: %+v %v", a, err)
	}
	if _, err := Collect(nil, url, map[string]string{"doc": "http://example.com/a.pdf"}, nil); err == nil {
		t.Error("an http url passed")
	}
	odd := []manifest.IntakeQuestion{{ID: "x", Prompt: "X", Type: "sharepoint-library"}}
	if _, err := Collect(nil, odd, map[string]string{"x": "anything"}, nil); err == nil {
		t.Error("an unknown type was taken as text")
	}
}

// TestSourceHosts (agentry-cli#854): the hosts a scheduled run's source
// answers fetch from, each once; an unanswered question names none, and
// an answer a run would refuse is refused here too.
func TestSourceHosts(t *testing.T) {
	qs := []manifest.IntakeQuestion{
		{ID: "repo", Prompt: "Repository", Type: manifest.IntakeRepository},
		{ID: "repo2", Prompt: "Another", Type: manifest.IntakeRepository},
		{ID: "doc", Prompt: "Doc", Type: manifest.IntakeURL},
		{ID: "skip", Prompt: "Unanswered", Type: manifest.IntakeURL},
		{ID: "note", Prompt: "Note"},
	}
	hosts, err := SourceHosts(qs, map[string]string{
		"repo": "https://GitHub.com/o/r", "repo2": "https://github.com/o/s#v1",
		"doc": "https://files.example.com/a.pdf", "note": "https://text.example.com/not-a-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github.com", "files.example.com"}; !slices.Equal(hosts, want) {
		t.Fatalf("hosts = %q, want %q", hosts, want)
	}
	if _, err := SourceHosts(qs, map[string]string{"repo": "http://github.com/o/r"}); err == nil {
		t.Error("an http repository passed")
	}
}

// A host question's answer is a target host, never text, and its document
// names the host and the account and never a sign-in (agentry-cli#861).
func TestAHostQuestionIsATargetHostNeverText(t *testing.T) {
	qs := []manifest.IntakeQuestion{{ID: "server", Prompt: "The server to set up", Type: manifest.IntakeHost, Required: true}}
	a, err := Collect(nil, qs, map[string]string{"server": "admin@box.example:2222"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := a.Hosts["server"]
	if h == nil || h.Answer.User != "admin" || h.Answer.Host != "box.example" || h.Answer.Port != 2222 || len(a.Values) != 0 || a.Empty() {
		t.Fatalf("answers %+v, host %+v", a, h)
	}
	doc := a.Document("/inputs")["server"].(map[string]any)["host"].(map[string]any)
	if doc["named"] != "box.example" || doc["user"] != "admin" || doc["port"] != 2222 {
		t.Errorf("document %v", doc)
	}
	p := &scriptPrompter{answers: map[string]string{"server": "box"}}
	if a, err = Collect(p, qs, nil, nil); err != nil || a.Hosts["server"] == nil || !strings.Contains(p.asked[0], "[user@]host[:port]") {
		t.Fatalf("asked %q: %+v, %v", p.asked, a, err)
	}
	if _, err := Collect(nil, qs, map[string]string{"server": "admin:pw@box"}, nil); err == nil {
		t.Error("an answer that holds a password was taken")
	}
}

// TestABrowsersSiteIsAnAddressNeverASource (agentry-cli#915): a url
// question the browser names as a site is answered as an http or https
// address, kept in Values for intake.json, and never fetched; another url
// question of the same release is still a source.
func TestABrowsersSiteIsAnAddressNeverASource(t *testing.T) {
	m := &manifest.Manifest{
		Intake: []manifest.IntakeQuestion{
			{ID: "site", Prompt: "Which site should I test?", Type: manifest.IntakeURL, Required: true},
			{ID: "doc", Prompt: "A document", Type: manifest.IntakeURL},
		},
		Capabilities: manifest.Capabilities{Browser: &manifest.Browser{Sites: []string{"site"}}},
	}
	a, err := CollectRun(nil, m, map[string]string{"site": "http://staging.lan:8080/login", "doc": "https://example.com/a.pdf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Values["site"] != "http://staging.lan:8080/login" || a.Sources["site"] != nil {
		t.Fatalf("the site: values %v, sources %v", a.Values, a.Sources)
	}
	if a.Sources["doc"] == nil || a.Values["doc"] != "" {
		t.Fatalf("the other url question is no longer a source: %+v", a)
	}
	for _, bad := range []string{"shop.example", "ftp://shop.example/", "https://u:p@shop.example/"} {
		if _, err := CollectRun(nil, m, map[string]string{"site": bad}, nil); err == nil || !strings.Contains(err.Error(), "intake site") {
			t.Errorf("site %q: %v", bad, err)
		}
	}
	if _, err := CollectRun(nil, m, nil, nil); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("a required site unanswered: %v", err)
	}
	// Asked on a terminal, it is asked as an address.
	p := &scriptPrompter{answers: map[string]string{"Which site": "https://shop.example/"}}
	if a, err := CollectRun(p, m, nil, nil); err != nil || a.Values["site"] != "https://shop.example/" {
		t.Fatalf("asked: %+v %v (asked %v)", a, err, p.asked)
	}
	// Without the browser line the same question is a source, as before.
	m.Capabilities.Browser = nil
	if a, err := CollectRun(nil, m, map[string]string{"site": "https://shop.example/"}, nil); err != nil || a.Sources["site"] == nil {
		t.Fatalf("no browser: %+v %v", a, err)
	}
}
