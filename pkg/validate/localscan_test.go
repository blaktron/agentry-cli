package validate

// Parity for the S1 subset: the local checks must agree with the ones the
// registry runs at submit, or the round-trip they exist to prevent comes
// straight back.

import (
	"slices"
	"testing"
)

// s1Fixtures are the files both scanners see. Names are deliberately
// innocuous where the content is not: detection is by magic bytes, and a
// scanner fooled by a rename is no scanner.
func s1Fixtures() map[string][]byte {
	return map[string][]byte{
		"notes.md":       []byte("# Notes\n\nPlain text.\n"),
		"lib/helper.md":  append([]byte{0x7f, 0x45, 0x4c, 0x46}, []byte("\x02\x01\x01 not really markdown")...),
		"vendor/deps.md": {0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00},
		"legacy.txt":     {0x48, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0xff, 0xfe, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
		"art.png":        {0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01},
	}
}

func TestLocalS1FlagsBinariesArchivesAndEncoding(t *testing.T) {
	var files []localFile
	for name, content := range s1Fixtures() {
		files = append(files, localFile{Path: name, Content: content})
	}
	got := map[string]string{}
	var order []string
	found := scanS1(files)
	for _, f := range found {
		got[f.FilePath] = f.Scanner
		order = append(order, f.FilePath)
	}
	// Findings come out by path, and each carries the severity and title the
	// registry gives it.
	if want := []string{"legacy.txt", "lib/helper.md", "vendor/deps.md"}; !slices.Equal(order, want) {
		t.Errorf("findings are in the order %v, want %v", order, want)
	}
	for _, f := range found {
		wantSeverity, wantTitle := map[string]string{
			"S1/binary-executable": "critical",
			"S1/nested-archive":    "critical",
			"S1/non-utf8":          "low",
		}[f.Scanner], map[string]string{
			"S1/binary-executable": "Executable binary in package",
			"S1/nested-archive":    "Nested archive in package",
			"S1/non-utf8":          "Non-UTF-8 text file",
		}[f.Scanner]
		if f.Severity != wantSeverity || f.Title != wantTitle {
			t.Errorf("%s: severity %q title %q, want %q %q", f.FilePath, f.Severity, f.Title, wantSeverity, wantTitle)
		}
	}

	want := map[string]string{
		"lib/helper.md":  "S1/binary-executable",
		"vendor/deps.md": "S1/nested-archive",
		"legacy.txt":     "S1/non-utf8",
	}
	for path, scanner := range want {
		if got[path] != scanner {
			t.Errorf("%s: got %q, want %q", path, got[path], scanner)
		}
	}
	// Clean text and images must not be flagged at all.
	for _, clean := range []string{"notes.md", "art.png"} {
		if s, ok := got[clean]; ok {
			t.Errorf("%s was flagged as %q", clean, s)
		}
	}
}

// TestLocalS1FailsTheCommand: critical inventory findings must make validate
// non-zero, while an "info" opaque binary must not — the pipeline routes that
// one to a reviewer rather than rejecting it, and a local check stricter than
// the server is its own parity bug.
func TestLocalS1SeverityGatesExitCode(t *testing.T) {
	critical := scanS1([]localFile{{Path: "a.md", Content: []byte{0x7f, 0x45, 0x4c, 0x46}}})
	if n := failingFindings(critical); n != 1 {
		t.Errorf("executable: failing findings = %d, want 1", n)
	}
	// A short opaque blob: not exec, not archive, not image, not text.
	opaque := scanS1([]localFile{{Path: "blob.dat", Content: []byte{0x00, 0x01, 0x02, 0x03}}})
	if len(opaque) != 1 || opaque[0].Scanner != "S1/opaque-binary" {
		t.Fatalf("opaque blob: %+v", opaque)
	}
	if n := failingFindings(opaque); n != 0 {
		t.Errorf("opaque binary: failing findings = %d, want 0 (server routes it to review)", n)
	}
}
