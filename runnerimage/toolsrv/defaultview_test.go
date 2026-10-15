// The release's default view: the manifest's inputs.view, written into the
// inputs index as defaultView, is the view read_input and search_inputs
// serve when the model asks for none, for every input that has it. An
// input without it keeps its own default, a view asked for always wins, and
// an index without a defaultView reads exactly as before.

package toolsrv

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDefaultView rewrites s's index with the release default view.
func withDefaultView(t *testing.T, s *Spec, view string) *Spec {
	t.Helper()
	p := filepath.Join(s.InputsTextDir, InputsIndexFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var idx InputsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	idx.DefaultView = view
	writeIndexOf(t, s, idx)
	return s
}

func writeIndexOf(t *testing.T, s *Spec, idx InputsIndex) {
	t.Helper()
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.InputsTextDir, InputsIndexFile), b)
}

const docxOriginal = "PK\x03\x04\x00\xff binary zip"

func TestTheReleasesDefaultView(t *testing.T) {
	docText := "# Report\n\nThe quarterly review is on Friday.\n"
	docOriginal := "binary file; 17 bytes, detected as application/vnd.openxmlformats-officedocument.wordprocessingml.document; bytes 0 to 17 as base64:\n" +
		base64.StdEncoding.EncodeToString([]byte(docxOriginal))
	notes := "Remember the Quarterly Review.\nnothing else\n"
	for _, tc := range []struct {
		name, release, path, asked, want string
	}{
		// No release default: each input's own.
		{"own: html", "", "site/index.html", "", pageHTML},
		{"own: document", "", "report.docx", "", docText},
		// text: HTML's Markdown, a document's text, plain text as is.
		{"text: html", ViewText, "site/index.html", "", pageMarkdown},
		{"text: document", ViewText, "report.docx", "", docText},
		{"text: plain", ViewText, "notes.txt", "", notes},
		// original: the upload, a binary document as base64.
		{"original: html", ViewOriginal, "site/index.html", "", pageHTML},
		{"original: document", ViewOriginal, "report.docx", "", docOriginal},
		{"original: plain", ViewOriginal, "notes.txt", "", notes},
		// A view asked for wins over the release's.
		{"text, asked original", ViewText, "site/index.html", ViewOriginal, pageHTML},
		{"original, asked text", ViewOriginal, "report.docx", ViewText, docText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, withDefaultView(t, viewsSpec(t), tc.release))
			got, isErr := s.toolReadInput(rawArgs(t, map[string]any{"path": tc.path, "view": tc.asked}))
			if isErr || got != tc.want {
				t.Fatalf("read_input %s = (%q, %v), want %q", tc.path, got, isErr, tc.want)
			}
		})
	}
}

// TestADefaultAnInputLacksFallsBackToItsOwn: text on a file with no text
// reads it as uploaded; original on a member decoded out of a document,
// which has no original, reads its text.
func TestADefaultAnInputLacksFallsBackToItsOwn(t *testing.T) {
	s := decodedSpec(t)
	mustWrite(t, filepath.Join(s.InputsTextDir, InputsTextSubdir, "bundle.zip", "letter.docx"), []byte("Dear reader.\n"))
	writeIndexOf(t, s, InputsIndex{Schema: InputsIndexSchema, DefaultView: ViewText, Files: []InputEntry{
		{Path: "blob.bin", Type: "unknown", Bytes: 4, Unread: "not text, and no decoder reads this kind of file", Original: true, MIME: "application/octet-stream"},
	}})
	srv := newTestServer(t, s)
	want := "binary file; 4 bytes, detected as application/octet-stream; bytes 0 to 4 as base64:\n" + base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff})
	if got, isErr := srv.toolReadInput(rawArgs(t, map[string]any{"path": "blob.bin"})); isErr || got != want {
		t.Fatalf("text default on a file with no text = (%q, %v), want its original", got, isErr)
	}
	writeIndexOf(t, s, InputsIndex{Schema: InputsIndexSchema, DefaultView: ViewOriginal, Files: []InputEntry{
		{Path: "bundle.zip/letter.docx", Type: "document", Bytes: 30, InArchive: "bundle.zip", Decoded: true, DecodedBy: "pandoc", TextBytes: 13},
	}})
	if got, isErr := srv.toolReadInput(rawArgs(t, map[string]any{"path": "bundle.zip/letter.docx"})); isErr || got != "Dear reader.\n" {
		t.Fatalf("original default on a member with no original = (%q, %v), want its text", got, isErr)
	}
}

// TestListInputsMarksTheReleasesDefault: the views column puts the default
// the release chose first.
func TestListInputsMarksTheReleasesDefault(t *testing.T) {
	for _, tc := range []struct {
		release, doc, html string
	}{
		{"", "text (default), original", "original (default), text (Markdown)"},
		{ViewText, "text (default), original", "text (Markdown; default), original"},
		{ViewOriginal, "original (default), text", "original (default), text (Markdown)"},
	} {
		s := newTestServer(t, withDefaultView(t, viewsSpec(t), tc.release))
		ls, isErr := s.toolListInputs(rawArgs(t, map[string]any{}))
		if isErr || !strings.Contains(ls, "report.docx\tdocument\t20\tread\t44\t"+tc.doc+"\n") || !strings.HasSuffix(ls, "\t"+tc.html) {
			t.Errorf("release %q: want the document's views %q and the page's %q:\n%s", tc.release, tc.doc, tc.html, ls)
		}
	}
}

// TestSearchInputsFollowsTheReleasesDefault: with no view asked, search
// reads each input in the release's view, so the original default finds
// HTML's markup and the text default does not.
func TestSearchInputsFollowsTheReleasesDefault(t *testing.T) {
	text := newTestServer(t, withDefaultView(t, viewsSpec(t), ViewText))
	if got, _ := text.toolSearchInputs(rawArgs(t, map[string]any{"query": "innerHTML"})); !strings.HasPrefix(got, "no input text matches") {
		t.Errorf("text default searched HTML's original: %q", got)
	}
	orig := newTestServer(t, withDefaultView(t, viewsSpec(t), ViewOriginal))
	if got, _ := orig.toolSearchInputs(rawArgs(t, map[string]any{"query": "innerHTML"})); !strings.Contains(got, "site/index.html:4:") {
		t.Errorf("original default did not search HTML's original: %q", got)
	}
	// A decoded document's original is binary, so its text is searched.
	if got, _ := orig.toolSearchInputs(rawArgs(t, map[string]any{"query": "Friday"})); !strings.Contains(got, "report.docx:3:") {
		t.Errorf("original default did not search a document's text: %q", got)
	}
}

// TestAnUnknownDefaultViewIsRefused: the index is the engine's, and a value
// outside the two views is a fault, never read as one of them.
func TestAnUnknownDefaultViewIsRefused(t *testing.T) {
	s := newTestServer(t, withDefaultView(t, viewsSpec(t), "both"))
	if got, isErr := s.toolReadInput(rawArgs(t, map[string]any{"path": "notes.txt"})); !isErr || !strings.Contains(got, `unknown default view "both"`) {
		t.Fatalf("read_input = (%q, %v)", got, isErr)
	}
}

// TestAnIndexWithoutADefaultViewIsUnchanged: the field is omitted when
// empty, so every run of a release without the block writes the index it
// wrote before.
func TestAnIndexWithoutADefaultViewIsUnchanged(t *testing.T) {
	b, err := json.Marshal(InputsIndex{Schema: InputsIndexSchema, Files: []InputEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "defaultView") || strings.Contains(string(b), "releaseView") {
		t.Fatalf("index = %s", b)
	}
}
