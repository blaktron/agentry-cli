package decode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// originalOf is the bytes playDecode keeps as a member's original.
func originalOf(path string) string { return "original of " + path + "\n" }

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// stagedArchive stages one archive, docs.zip, and a note beside it.
func stagedArchive(t *testing.T) string {
	t.Helper()
	in := t.TempDir()
	for _, n := range []string{"docs.zip", "notes.md"} {
		if err := os.WriteFile(filepath.Join(in, n), []byte("document "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// keptMembers is a decode that kept members: docs.zip holds a decoded PDF,
// a text file, a file no decoder reads, and a PDF whose kept copy is not
// the bytes the check read.
func keptMembers() []runnerdecode.File {
	member := func(f runnerdecode.File) runnerdecode.File {
		f.In = "docs.zip"
		if f.SHA256 == "" {
			f.SHA256 = digestOf(originalOf(f.Path))
		}
		return f
	}
	return []runnerdecode.File{
		{Path: "docs.zip", Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked},
		member(runnerdecode.File{Path: "docs.zip/a.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "docs.zip/a.pdf.txt", Bytes: 20, Original: "originals/docs.zip/a.pdf"}),
		member(runnerdecode.File{Path: "docs.zip/b.md", Family: "text", Outcome: runnerdecode.OutcomeText, Output: "docs.zip/b.md.txt", Bytes: 20, Original: "originals/docs.zip/b.md"}),
		member(runnerdecode.File{Path: "docs.zip/c.bin", Family: "unknown", Outcome: runnerdecode.OutcomeUndecodable, Reason: "not text"}),
		member(runnerdecode.File{Path: "docs.zip/d.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "docs.zip/d.pdf.txt", Bytes: 20, Original: "originals/docs.zip/d.pdf", SHA256: digestOf("other bytes")}),
		{Path: "notes.md", Family: "text", Outcome: runnerdecode.OutcomeText},
	}
}

// TestABuiltInsStageListsAnArchivesMembers: with kept members, each member
// is a document of its own by <archive>/<member>, the archive is not one, a
// member whose kept copy is not the bytes the check read is unread and not
// keepable, and an original is copied out as it was kept.
func TestABuiltInsStageListsAnArchivesMembers(t *testing.T) {
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, keptMembers())}}}
	res, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.FindRun("agentry-decode-")
	if !strings.Contains(strings.Join(r.Argv, " "), "--keep-members") {
		t.Fatalf("argv %v", r.Argv)
	}
	dst := t.TempDir()
	docs, _, err := res.Stage(nil, dst)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Document{}
	for _, d := range docs {
		got[d.Name] = d
	}
	if _, ok := got["docs.zip"]; ok {
		t.Fatal("the archive is a document beside its members")
	}
	if d := got["docs.zip/a.pdf"]; d.Read != "docs.zip/a.pdf.txt" || d.DecodedBy != "pdftotext" || d.Unread != "" {
		t.Fatalf("a decoded member: %+v", d)
	}
	if d := got["docs.zip/b.md"]; d.Read != "docs.zip/b.md" || d.Unread != "" {
		t.Fatalf("a text member: %+v", d)
	}
	if d := got["docs.zip/c.bin"]; d.Unread == "" || d.Read != "" {
		t.Fatalf("an undecodable member: %+v", d)
	}
	if d := got["docs.zip/d.pdf"]; d.Unread != memberNotKept || res.Keepable("docs.zip/d.pdf") {
		t.Fatalf("a member whose copy is not the checked bytes: %+v", d)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "docs.zip", "b.md")); err != nil || string(b) != originalOf("docs.zip/b.md") {
		t.Fatalf("the text member as staged: %q %v", b, err)
	}
	out := filepath.Join(t.TempDir(), "a.pdf")
	if err := res.CopyOriginal("docs.zip/a.pdf", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != originalOf("docs.zip/a.pdf") {
		t.Fatalf("the kept original %q", b)
	}
	if err := res.CopyOriginal("docs.zip/c.bin", out); err == nil {
		t.Fatal("a member that was not kept was copied out")
	}
	raw, _ := os.ReadFile(filepath.Join(dst, DocumentsFile))
	var listed struct{ Documents []Document }
	if err := json.Unmarshal(raw, &listed); err != nil || len(listed.Documents) != 5 {
		t.Fatalf("documents.json: %s (%v)", raw, err)
	}
}

// Without kept members the archive is unread and its members left out, as
// before; and an archive none of whose members was kept is listed, unread.
func TestAnArchiveIsUnreadWithoutKeptMembers(t *testing.T) {
	plain := keptMembers()
	for i := range plain {
		plain[i].Original = ""
	}
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, plain)}}}
	res, err := Run(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	docs, _, err := res.Stage(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range docs {
		names = append(names, d.Name)
		if d.Name == "docs.zip" && d.Unread != archiveNotRead {
			t.Fatalf("the archive: %+v", d)
		}
	}
	if strings.Join(names, ",") != "docs.zip,notes.md" {
		t.Fatalf("documents %v", names)
	}
}

// The host holds a kept original to its rule: only a decode that keeps
// members, only a member read, only at exactly originals/<path>.
func TestAKeptOriginalIsHeldToItsRule(t *testing.T) {
	for name, tamper := range map[string]func([]runnerdecode.File) []runnerdecode.File{
		"on a file as staged": func(fs []runnerdecode.File) []runnerdecode.File {
			fs[5].Original = "originals/notes.md"
			return fs
		},
		"on an undecodable member": func(fs []runnerdecode.File) []runnerdecode.File {
			fs[3].Original = "originals/docs.zip/c.bin"
			return fs
		},
		"at another path": func(fs []runnerdecode.File) []runnerdecode.File {
			fs[1].Original = "originals/docs.zip/b.md"
			return fs
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, tamper(keptMembers()))}}}
			if _, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr); err == nil {
				t.Fatal("the report was accepted")
			}
		})
	}
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, keptMembers())}}}
	if _, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{}, os.Stderr); err == nil {
		t.Fatal("a decode that was not asked to keep members was accepted with kept originals")
	}
}
