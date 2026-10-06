package compile

import (
	"encoding/json"
	"strings"
	"testing"
)

// The release's default view (agentry-cli#693; core DEV-218): a manifest's
// inputs.view of text or original reaches the plan as InputView and the
// System Prompt's inputs paragraph says it; both, or no block, adds
// nothing, so every plan compiled before the block is the same bytes.

func TestTheInputViewReachesThePlan(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"no block", "", ""},
		{"kinds only", "inputs:\n  kinds: [code]\n", ""},
		{"both", "inputs:\n  view: both\n", ""},
		{"text", "inputs:\n  kinds: [documents]\n  view: text\n", "text"},
		{"original", "inputs:\n  kinds: [code, data]\n  view: original\n", "original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseManifest(t, pureTextYAML+tc.block)
			plan, _ := compileFixture(t, m, workspaceFor(t, m, nil))
			if plan.InputView != tc.want {
				t.Fatalf("InputView = %q, want %q", plan.InputView, tc.want)
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(string(raw), `"inputView"`); has != (tc.want != "") {
				t.Fatalf("the plan's JSON carries inputView: %v, want %v", has, tc.want != "")
			}
		})
	}
}

// TestNoOrBothViewCompilesTheSamePlan: a release that declares no view, or
// both, compiles to the plan it compiled before the block existed: same
// hash, same rendered instruction.
func TestNoOrBothViewCompilesTheSamePlan(t *testing.T) {
	compileHash := func(yaml string) (string, string) {
		t.Helper()
		m := parseManifest(t, yaml)
		plan, _ := compileFixture(t, m, workspaceFor(t, m, nil))
		h, err := plan.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return h, plan.Instruction.SHA256
	}
	plainHash, plainInstr := compileHash(pureTextYAML)
	_, bothInstr := compileHash(pureTextYAML + "inputs:\n  view: both\n")
	if bothInstr != plainInstr {
		t.Fatal("view: both changed the rendered instruction")
	}
	again, _ := compileHash(pureTextYAML)
	if again != plainHash {
		t.Fatal("compiling the same manifest twice gave two hashes")
	}
	_, textInstr := compileHash(pureTextYAML + "inputs:\n  view: text\n")
	if textInstr == plainInstr {
		t.Fatal("view: text left the rendered instruction unchanged")
	}
}

// TestTheInputsParagraphSaysTheReleasesView: the paragraph's default
// sentence follows the release's view, and the rest of the paragraph,
// how to ask for the other view, stays.
func TestTheInputsParagraphSaysTheReleasesView(t *testing.T) {
	own := systemPrompt(promptFacts{caps: capsNone(false)})
	text := systemPrompt(promptFacts{caps: capsNone(false), inputView: "text"})
	orig := systemPrompt(promptFacts{caps: capsNone(false), inputView: "original"})
	for name, tc := range map[string]struct {
		prompt string
		want   string
		not    string
	}{
		"own":      {own, "code, markup, configuration and data are read as uploaded", "this QuickAgent reads its inputs"},
		"text":     {text, "this QuickAgent reads its inputs as text: read_input gives you each file's text (a web page as Markdown), and a file with no text as uploaded", "code, markup, configuration and data are read as uploaded"},
		"original": {orig, `this QuickAgent reads its inputs as uploaded: read_input gives you each file as it was uploaded, a binary one as base64 pages; ask for view "text" when only a document's words matter`, "code, markup, configuration and data are read as uploaded"},
	} {
		if !strings.Contains(tc.prompt, tc.want) {
			t.Errorf("%s: want %q", name, tc.want)
		}
		if strings.Contains(tc.prompt, tc.not) {
			t.Errorf("%s: should not say %q", name, tc.not)
		}
		if !strings.Contains(tc.prompt, `read it with view "original"`) || !strings.Contains(tc.prompt, `view "text" reads HTML as Markdown`) {
			t.Errorf("%s: the paragraph lost how to ask for each view", name)
		}
	}
}
