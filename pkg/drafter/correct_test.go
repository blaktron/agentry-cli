package drafter

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"
)

// TestCorrect holds the post-step's mechanical corrections (plan
// builder-simplify.md §5.2 step 1): each slip is corrected as a data edit
// and named, the Drafter's key order is kept, and a clean manifest, or one
// that is not an object, comes back byte for byte.
func TestCorrect(t *testing.T) {
	long := strings.Repeat("word ", 40) // 200 characters
	m := `{"schemaVersion":"2","name":"x","summary":"` + long + `","modes":["autonomous","autonomous"],` +
		`"tags":["a","b","a","c","d","e","f","g","h","i","j","k"],` +
		`"intake":[{"id":"fmt","prompt":"Format?","type":"choice","choices":["md","json","md",""]},{"id":"t","prompt":"T","choices":[]}],` +
		`"guide":{"overview":"Reads.","prepare":["` + strings.Repeat("p", 250) + `","","n/a","two","3","4","5","6","7","8","9","10","11"],` +
		`"files":"None.","task":"","connect":{"GITHUB_TOKEN":"` + strings.Repeat("Make a token. ", 200) + `","EMPTY":"  "}},` +
		`"interactive":{"opening":null},"capabilities":{"exec":"none","secrets":["GITHUB_TOKEN"]}}`
	out, did := Correct(json.RawMessage(m))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, want := range []string{
		`modes: "autonomous" was named twice, so the second was dropped.`,
		`tags: "a" was named twice, so the second was dropped.`,
		`tags: 11 tags, over the limit of 10, so the first 10 were kept.`,
		`summary: 200 characters, over the limit of 140, so it was shortened at a word to`,
		`intake[0].choices: "md" was named twice, so the second was dropped.`,
		`intake[0].choices: the empty choice "" was removed.`,
		`intake[1].choices: the empty value [] was removed.`,
		`guide.prepare: the empty prepare item "" was removed.`,
		`guide.prepare: the empty prepare item "n/a" was removed.`,
		`prepare: 11 guide.prepare items, over the limit of 10, so the first 10 were kept.`,
		`guide.prepare[0]: 250 characters, over the limit of 200`,
		`guide.files: the placeholder value "None." was removed.`,
		`guide.task: the empty value "" was removed.`,
		`guide.connect.EMPTY: the empty help "  " was removed.`,
		`guide.connect.GITHUB_TOKEN: 2800 characters, over the limit of 2000`,
		`interactive.opening: the empty value null was removed.`,
		`interactive: the empty value {} was removed.`,
	} {
		if !containsPrefix(did, want) {
			t.Errorf("no correction %q in\n%s", want, strings.Join(did, "\n"))
		}
	}
	yml, err := manifestYAML(out)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := manifest.Parse(yml)
	if err != nil {
		t.Fatalf("the corrected manifest does not parse: %v\n%s", err, yml)
	}
	for _, is := range manifest.Validate(pm) {
		if s := is.Error(); strings.Contains(s, "summary") || strings.Contains(s, "duplicate") || strings.Contains(s, "guide") ||
			strings.Contains(s, "tags") || strings.Contains(s, "choice") {
			t.Errorf("a slip the post-step corrects is still refused: %s", s)
		}
	}
	if n := utf8.RuneCountInString(pm.Summary); n > manifest.MaxSummaryChars || !strings.HasSuffix(pm.Summary, "word…") {
		t.Errorf("summary %q (%d)", pm.Summary, n)
	}
	if n := utf8.RuneCountInString(pm.Guide.Connect["GITHUB_TOKEN"]); n > manifest.MaxGuideConnectChars || !strings.HasSuffix(pm.Guide.Connect["GITHUB_TOKEN"], " a…") {
		t.Errorf("connect help ends %q (%d)", pm.Guide.Connect["GITHUB_TOKEN"][n-20:], n)
	}
	if pm.Guide.Files != "" || pm.Guide.Task != "" || len(pm.Guide.Prepare) != 10 || pm.Interactive != nil || len(pm.Tags) != 10 {
		t.Errorf("guide %+v interactive %+v tags %v", pm.Guide, pm.Interactive, pm.Tags)
	}
	if !strings.HasPrefix(string(out), `{"schemaVersion":"2","name":"x","summary":`) {
		t.Errorf("the key order changed: %s", out[:60])
	}

	for _, same := range []string{
		`{"schemaVersion": "2", "name": "x", "summary": "Fine.", "modes": ["autonomous"], "n": 1.5, "b": true, "z": null}`,
		`"not an object"`, `[1,2]`, `{"a": 1, "a": 2}`,
	} {
		out, did := Correct(json.RawMessage(same))
		if string(out) != same || len(did) != 0 {
			t.Errorf("%s came back as %s (%v)", same, out, did)
		}
	}
}

func containsPrefix(did []string, want string) bool {
	for _, d := range did {
		if strings.HasPrefix(d, want) {
			return true
		}
	}
	return false
}

// TestShortenAtWord: a text is cut at a space near its bound, never past
// it, with the ellipsis inside the bound; a text with no space is cut hard.
func TestShortenAtWord(t *testing.T) {
	fits := func(max int) func(string) bool {
		return func(s string) bool { return utf8.RuneCountInString(s) <= max }
	}
	if got := shortenAtWord("one two three four", fits(12)); got != "one two…" {
		t.Errorf("got %q", got)
	}
	if got := shortenAtWord("one, two, three", fits(10)); got != "one, two…" {
		t.Errorf("got %q", got)
	}
	if got := shortenAtWord(strings.Repeat("x", 50), fits(10)); got != strings.Repeat("x", 9)+"…" {
		t.Errorf("got %q", got)
	}
	if got := shortenAtWord("é é é é é é", func(s string) bool { return len(s) <= 8 }); len(got) > 8 || !utf8.ValidString(got) {
		t.Errorf("got %q", got)
	}
}
