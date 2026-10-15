package drafter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"
)

// TestCorrect holds the post-step's mechanical corrections: each slip is
// corrected as a data edit and named, the Drafter's key order is kept, and a
// clean manifest, or one that is not an object, comes back byte for byte.
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
		`guide.prepare: 11 prepare items, over the limit of 10, so the first 10 were kept.`,
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

	// A corrected manifest keeps its characters as they came.
	if out, _ := Correct(json.RawMessage(`{"summary": "a <b> & c", "tags": ["x", "x"]}`)); string(out) != `{"summary":"a <b> & c","tags":["x"]}` {
		t.Errorf("got %s", out)
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

// TestADraftNeverShipsAPack: knowledge.ships is removed from a draft and
// named, what it accepts is kept, and a block left with nothing is
// removed.
func TestADraftNeverShipsAPack(t *testing.T) {
	m := `{"schemaVersion":"2","name":"x","knowledge":{"ships":[{"name":"runbooks"}],"accepts":{"prompt":"Your runbooks","required":true}}}`
	out, did := Correct(json.RawMessage(m))
	var got struct {
		Knowledge map[string]json.RawMessage `json:"knowledge"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Knowledge["ships"]; ok || string(got.Knowledge["accepts"]) != `{"prompt":"Your runbooks","required":true}` {
		t.Fatalf("knowledge %s", out)
	}
	if len(did) != 1 || !strings.HasPrefix(did[0], "knowledge.ships was removed: a draft never ships a knowledge pack.") {
		t.Fatalf("did %q", did)
	}
	out, _ = Correct(json.RawMessage(`{"schemaVersion":"2","name":"x","knowledge":{"ships":[{"name":"runbooks"}]}}`))
	if strings.Contains(string(out), "knowledge") {
		t.Fatalf("an empty knowledge block was kept: %s", out)
	}
}

// TestKnowledgeUnderCapabilitiesIsLifted: a knowledge block the Drafter
// wrote under capabilities is moved to the manifest's top level and named,
// and the manifest then parses.
func TestKnowledgeUnderCapabilitiesIsLifted(t *testing.T) {
	m := `{"schemaVersion":"2","name":"x","capabilities":{"exec":"none","knowledge":{"accepts":{"prompt":"Your runbooks"}}}}`
	out, did := Correct(json.RawMessage(m))
	var got struct {
		Knowledge    *manifest.Knowledge        `json:"knowledge"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Knowledge == nil || got.Knowledge.Accepts == nil || got.Knowledge.Accepts.Prompt != "Your runbooks" {
		t.Fatalf("knowledge %s", out)
	}
	if _, ok := got.Capabilities["knowledge"]; ok || len(did) != 1 || !strings.HasPrefix(did[0], "capabilities.knowledge was moved to knowledge") {
		t.Fatalf("%s %q", out, did)
	}
	both := `{"schemaVersion":"2","name":"x","knowledge":{"accepts":{"prompt":"Mine"}},"capabilities":{"knowledge":{"accepts":{"prompt":"Other"}}}}`
	out, did = Correct(json.RawMessage(both))
	if !strings.Contains(string(out), `"Mine"`) || strings.Contains(string(out), `"Other"`) || len(did) != 1 {
		t.Fatalf("%s %q", out, did)
	}
}

// TestTheInputsBlockIsCorrected: an inputs block written under
// capabilities is lifted to the top level; a kind in the singular or
// another case gets its name; a kind named twice once; any beside named
// kinds is dropped, since it stands alone; an emptied list and block go;
// a kind outside the set is left for the rules.
func TestTheInputsBlockIsCorrected(t *testing.T) {
	kindsOf := func(t *testing.T, out json.RawMessage) (*manifest.Inputs, map[string]json.RawMessage) {
		t.Helper()
		var got struct {
			Inputs       *manifest.Inputs           `json:"inputs"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		return got.Inputs, got.Capabilities
	}
	for _, tc := range []struct {
		name, in  string
		kinds     []manifest.InputKind
		view      manifest.InputView
		dids      int
		didPrefix string
	}{
		{"lifted from capabilities", `{"capabilities":{"exec":"none","inputs":{"kinds":["code"],"view":"original"}}}`,
			[]manifest.InputKind{"code"}, "original", 1, "capabilities.inputs was moved to inputs"},
		{"singular and case", `{"inputs":{"kinds":["Document","image"],"view":"text"}}`,
			[]manifest.InputKind{"documents", "images"}, "text", 2, `inputs.kinds: "Document" was written "documents"`},
		{"named twice", `{"inputs":{"kinds":["code","code"]}}`,
			[]manifest.InputKind{"code"}, "", 1, `inputs.kinds: "code" was named twice`},
		{"any beside kinds", `{"inputs":{"kinds":["any","data"],"view":"both"}}`,
			[]manifest.InputKind{"data"}, "both", 1, "inputs.kinds: any was dropped"},
		{"a kind outside the set is kept", `{"inputs":{"kinds":["spreadsheets"]}}`,
			[]manifest.InputKind{"spreadsheets"}, "", 0, ""},
		{"nothing to correct", `{"inputs":{"kinds":["documents"],"view":"text"}}`,
			[]manifest.InputKind{"documents"}, "text", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, did := Correct(json.RawMessage(tc.in))
			in, caps := kindsOf(t, out)
			if in == nil || !slices.Equal(in.Kinds, tc.kinds) || in.View != tc.view {
				t.Fatalf("inputs %s", out)
			}
			if _, ok := caps["inputs"]; ok {
				t.Fatalf("inputs left under capabilities: %s", out)
			}
			if len(did) != tc.dids || (tc.didPrefix != "" && !strings.HasPrefix(did[0], tc.didPrefix)) {
				t.Fatalf("did %q", did)
			}
		})
	}
	out, _ := Correct(json.RawMessage(`{"name":"x","inputs":{"kinds":[]}}`))
	if strings.Contains(string(out), "inputs") {
		t.Fatalf("an empty inputs block was kept: %s", out)
	}
}
