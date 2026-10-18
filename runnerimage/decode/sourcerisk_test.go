package decode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestASourceChecksOnlyItsRiskyFiles: in an input source's folder ClamAV
// reads only the executable and binary risky files; every other file
// there says it was not checked and why, and an upload beside it is
// checked as before. An EICAR executable in the source is found.
func TestASourceChecksOnlyItsRiskyFiles(t *testing.T) {
	tools, logDir := clamTools(t)
	in, out := t.TempDir(), t.TempDir()
	for name, data := range map[string]string{
		"repo/main.go":           "package main\n",
		"repo/notes.txt":         "EICAR in a text file is not this rule's\n",
		"repo/bin/tool.exe":      "MZ X5O EICAR test",
		"repo/docs/report.pdf":   "%PDF clean",
		"repo/lib/libx.so.1.2.3": "ELF clean",
		"upload.md":              "# an upload\n",
	} {
		write(t, in, name, []byte(data))
	}
	rep, err := Decode(context.Background(), in, out, tools, Limits{ClamDB: "/clamav", Sources: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	for path, want := range map[string]Check{
		"repo/main.go": CheckUnchecked, "repo/notes.txt": CheckUnchecked,
		"repo/bin/tool.exe": CheckFound, "repo/docs/report.pdf": CheckClean, "repo/lib/libx.so.1.2.3": CheckClean,
		"upload.md": CheckClean,
	} {
		if got[path].Check != want {
			t.Errorf("%s: check %q (%q), want %q", path, got[path].Check, got[path].Unchecked, want)
		}
	}
	for _, path := range []string{"repo/main.go", "repo/notes.txt"} {
		if got[path].Unchecked != UncheckedNotRisky {
			t.Errorf("%s: unchecked %q, want %q", path, got[path].Unchecked, UncheckedNotRisky)
		}
		if got[path].Outcome != OutcomeText {
			t.Errorf("%s: outcome %q, a file left unchecked is still read", path, got[path].Outcome)
		}
	}
	if got["repo/bin/tool.exe"].Outcome != OutcomeRefused || got["repo/bin/tool.exe"].Why != WhyFound {
		t.Errorf("the EICAR executable was not refused: %+v", got["repo/bin/tool.exe"])
	}
	list, _ := os.ReadFile(filepath.Join(logDir, "list"))
	if strings.Contains(string(list), "main.go") || strings.Contains(string(list), "notes.txt") {
		t.Errorf("clamscan was handed a source's text files:\n%s", list)
	}
}

// TestASourceHasACapOfItsOwn: a source's files count against its own cap,
// so a repository does not take the uploads' 2,000, nor they its.
func TestASourceHasACapOfItsOwn(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	for _, name := range []string{"repo/a.txt", "repo/b.txt", "repo/c.txt", "one.txt", "two.txt"} {
		write(t, in, name, []byte("text\n"))
	}
	rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{MaxFiles: 2, SourceMaxFiles: 2, Sources: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{}
	for _, f := range rep.Files {
		if f.Outcome == OutcomeRefused {
			refused[f.Path] = f.Reason
		}
	}
	if len(refused) != 1 || refused["repo/c.txt"] != "past the 2-file cap" {
		t.Errorf("refused %v, want only repo/c.txt past the source's cap", refused)
	}
}

func TestSourceRisky(t *testing.T) {
	for _, c := range []struct {
		mime, path string
		want       bool
	}{
		{"application/x-dosexec", "a/b", true},
		{"application/x-sharedlib", "lib", true},
		{"application/zip", "app.jar", true},
		{"application/zip", "app.apk", true},
		{"application/octet-stream", "libz.so.1", true},
		{"application/pdf", "doc", true},
		{"application/vnd.ms-excel.sheet.macroEnabled.12; charset=binary", "x", true},
		{"text/plain", "main.go", false},
		{"text/javascript", "app.js", false},
		{"text/x-shellscript", "run.sh", false},
		{"application/zip", "bundle.zip", false},
		{"image/png", "logo.png", false},
	} {
		if got := SourceRisky(c.mime, c.path); got != c.want {
			t.Errorf("SourceRisky(%q, %q) = %v, want %v", c.mime, c.path, got, c.want)
		}
	}
}

// TestASourcesImageIsNotReadWithOCR: an image in a source is listed and
// kept as its original, never handed to tesseract; an uploaded image is.
func TestASourcesImageIsNotReadWithOCR(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	write(t, in, "repo/shots/scan.png", []byte("\x89PNG"))
	write(t, in, "scan.png", []byte("\x89PNG"))
	rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{Sources: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	if f := got["repo/shots/scan.png"]; f.Outcome != OutcomeUndecodable || f.Why != WhySourceImage || f.Tool != "" || f.Family != FamilyImage {
		t.Errorf("the source's image: %+v", f)
	}
	if f := got["scan.png"]; f.Outcome != OutcomeDecoded || f.Tool != "tesseract" {
		t.Errorf("the uploaded image: %+v", f)
	}
}

// TestASourcesArchiveIsCheckedWhole: an archive in a source goes to ClamAV
// itself, as an upload's does, so what it holds past the unpacking's depth
// and caps is not left unchecked; its text members follow the source rule.
func TestASourcesArchiveIsCheckedWhole(t *testing.T) {
	tools, _ := clamTools(t)
	in, out := t.TempDir(), t.TempDir()
	write(t, in, "repo/vendor/bundle.zip", zipOf(t, zipEntry{name: "notes.txt", body: "plain"}))
	rep, err := Decode(context.Background(), in, out, tools, Limits{ClamDB: "/clamav", Sources: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Files {
		switch f.Path {
		case "repo/vendor/bundle.zip":
			if f.Check != CheckClean {
				t.Errorf("the source's archive: check %q (%q)", f.Check, f.Unchecked)
			}
		case "repo/vendor/bundle.zip/notes.txt":
			if f.Unchecked != UncheckedNotRisky {
				t.Errorf("the archive's text member: %q", f.Unchecked)
			}
		}
	}
}

// TestASourceFileIsJudgedByItsFirstBytesToo: a source file whose detected
// type and name are not on the risky lists, but which opens as an OLE
// container or a PDF, is checked as the host will hold it to be. An
// Outlook message is an OLE container detected under a type of its own.
func TestASourceFileIsJudgedByItsFirstBytesToo(t *testing.T) {
	tools, _ := clamTools(t)
	in, out := t.TempDir(), t.TempDir()
	ole := []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0, 0, 0, 0}
	write(t, in, "repo/testdata/message.msg", ole)
	write(t, in, "repo/notes.txt", []byte("%PDF-1.7 a PDF under a text name"))
	write(t, in, "repo/main.go", []byte("package main\n"))
	rep, err := Decode(context.Background(), in, out, tools, Limits{ClamDB: "/clamav", Sources: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	for _, p := range []string{"repo/testdata/message.msg", "repo/notes.txt"} {
		if got[p].Check != CheckClean || got[p].Unchecked != "" {
			t.Errorf("%s: check %q (%q), want it checked", p, got[p].Check, got[p].Unchecked)
		}
	}
	if got["repo/main.go"].Unchecked != UncheckedNotRisky {
		t.Errorf("repo/main.go: unchecked %q, want %q", got["repo/main.go"].Unchecked, UncheckedNotRisky)
	}
}

// TestBinaryMagic: the first bytes the decoder and the host both hold a
// source file to.
func TestBinaryMagic(t *testing.T) {
	for _, c := range []struct {
		head string
		want bool
	}{
		{"MZ\x90\x00", true}, {"\x7fELF\x02", true}, {"%PDF-1.7", true}, {"\x00asm\x01", true},
		{"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1", true}, {"\xca\xfe\xba\xbe", true}, {"\xcf\xfa\xed\xfe", true},
		{"package main", false}, {"PK\x03\x04", false}, {"", false}, {"M", false},
	} {
		if got := BinaryMagic([]byte(c.head)); got != c.want {
			t.Errorf("%q: %v, want %v", c.head, got, c.want)
		}
	}
}
