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
	docs, _, err := res.StageWith(nil, dst, StageOptions{Members: true, Originals: true})
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
	if d := got["docs.zip/b.md"]; d.Read != "docs.zip/b.md.txt" || d.Unread != "" {
		t.Fatalf("a text member: %+v", d)
	}
	if d := got["docs.zip/c.bin"]; d.Unread == "" || d.Read != "" {
		t.Fatalf("an undecodable member: %+v", d)
	}
	if _, kept := res.OriginalSize("docs.zip/d.pdf"); got["docs.zip/d.pdf"].Unread != memberNotKept || kept {
		t.Fatalf("a member whose copy is not the checked bytes: %+v", got["docs.zip/d.pdf"])
	}
	if b, err := os.ReadFile(filepath.Join(dst, "docs.zip", "b.md.txt")); err != nil || string(b) != "text of docs.zip/b.md\n" {
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

// Stage alone leaves an archive unread and its members out, and with
// members an archive none of whose members was kept is listed, unread.
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
	for name, opt := range map[string]StageOptions{"no members": {}, "members, none kept": {Members: true, Originals: true}} {
		docs, _, err := res.StageWith(nil, t.TempDir(), opt)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, d := range docs {
			names = append(names, d.Name)
			if d.Name == "docs.zip" && d.Unread != archiveNotRead {
				t.Fatalf("%s: the archive: %+v", name, d)
			}
		}
		if strings.Join(names, ",") != "docs.zip,notes.md" {
			t.Fatalf("%s: documents %v", name, names)
		}
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

// The scan record's digest, stated before any decoder ran, is what a kept
// copy is held to, not the report's: a report that names the copy's digest
// does not keep a copy the record says other bytes of.
func TestAKeptCopyIsHeldToTheRecordsDigest(t *testing.T) {
	print := func(r dockerx.RunSpec) string {
		var rep runnerdecode.Report
		b, _ := os.ReadFile(filepath.Join(mountSource(r, "/out"), runnerdecode.ReportFile))
		_ = json.Unmarshal(b, &rep)
		for i := range rep.Files {
			if rep.Files[i].Path == "docs.zip/b.md" {
				rep.Files[i].SHA256 = digestOf("the bytes the check read")
			}
		}
		return string(runnerdecode.RecordOf(rep.Files, true))
	}
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: print, Before: playDecode(t, keptMembers())}}}
	res, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := res.OriginalSize("docs.zip/b.md"); kept {
		t.Fatal("a copy the record's digest does not match was kept")
	}
	if _, kept := res.OriginalSize("docs.zip/a.pdf"); !kept {
		t.Fatal("a copy whose digest matches was not kept")
	}
}

// A kept copy that is a link, or past the cap a decode copies out, is
// refused with the report.
func TestAKeptCopyIsARegularFileWithinTheCap(t *testing.T) {
	for name, plant := range map[string]func(out string){
		"a link": func(out string) {
			_ = os.Remove(filepath.Join(out, "originals", "docs.zip", "a.pdf"))
			_ = os.Symlink("/etc/hostname", filepath.Join(out, "originals", "docs.zip", "a.pdf"))
		},
		"past the cap": func(out string) {
			_ = os.WriteFile(filepath.Join(out, "originals", "docs.zip", "a.pdf"), make([]byte, MaxTextBytes+1), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			play := playDecode(t, keptMembers())
			f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: func(r dockerx.RunSpec) {
				play(r)
				plant(mountSource(r, "/out"))
			}}}}
			if _, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr); err == nil {
				t.Fatal("the report was accepted")
			}
		})
	}
}

// A member is listed under the archive as staged, however deep and
// wherever that archive is: an archive in a zip has its own members listed
// and is not listed itself, and an archive inside a staged folder is the
// one its members belong to. A member whose name the caller cannot take is
// unread.
func TestMembersBelongToTheArchiveAsStaged(t *testing.T) {
	in := t.TempDir()
	if err := os.MkdirAll(filepath.Join(in, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"outer.zip", "folder/inner.zip"} {
		if err := os.WriteFile(filepath.Join(in, filepath.FromSlash(n)), []byte("document "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(path, in string) runnerdecode.File {
		return runnerdecode.File{Path: path, In: in, Family: "text", Outcome: runnerdecode.OutcomeText, Output: path + ".txt", Bytes: 10,
			Original: "originals/" + path, SHA256: digestOf(originalOf(path))}
	}
	files := []runnerdecode.File{
		{Path: "folder/inner.zip", Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked},
		kept("folder/inner.zip/a.md", "folder/inner.zip"),
		{Path: "outer.zip", Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked},
		{Path: "outer.zip/nested.zip", In: "outer.zip", Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked},
		kept("outer.zip/nested.zip/b.md", "outer.zip/nested.zip"),
		kept("outer.zip/odd name.md", "outer.zip"),
	}
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, files)}}}
	res, err := RunWith(context.Background(), f, signedImage(), in, t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	plain := func(p string) bool { return !strings.Contains(p, " ") }
	docs, _, err := res.StageWith(nil, t.TempDir(), StageOptions{Members: true, Originals: true, Named: plain})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Document{}
	for _, d := range docs {
		got[d.Name] = d
	}
	for _, name := range []string{"folder/inner.zip/a.md", "outer.zip/nested.zip/b.md"} {
		if d, ok := got[name]; !ok || d.Read == "" {
			t.Errorf("%s: %+v (listed %v)", name, d, ok)
		}
	}
	for _, name := range []string{"folder/inner.zip", "outer.zip", "outer.zip/nested.zip"} {
		if _, ok := got[name]; ok {
			t.Errorf("the archive %s is listed beside its members", name)
		}
	}
	if d := got["outer.zip/odd name.md"]; d.Unread != memberNotNamed || d.Read != "" {
		t.Errorf("a member whose name the caller cannot take: %+v", d)
	}
}

// Two members whose names differ only in case are refused, as they would be
// one file on a computer whose names ignore case.
func TestMembersThatDifferOnlyInCaseAreRefused(t *testing.T) {
	files := keptMembers()[:3]
	clash := files[2]
	clash.Path, clash.Output, clash.Original = "docs.zip/B.md", "docs.zip/B.md.txt", "originals/docs.zip/B.md"
	clash.SHA256 = digestOf(originalOf(clash.Path))
	files = append(files, clash)
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Print: printScan, Before: playDecode(t, files)}}}
	res, err := RunWith(context.Background(), f, signedImage(), stagedArchive(t), t.TempDir(), policy.Limits{}.WithDefaults(), Options{KeepMembers: true}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.StageWith(nil, t.TempDir(), StageOptions{Members: true, Originals: true}); err == nil || !strings.Contains(err.Error(), "differ only in case") {
		t.Fatalf("err %v", err)
	}
}
