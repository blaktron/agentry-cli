package deliver

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeName(t *testing.T) {
	// Characters that cannot be written as a literal in a test are built.
	r := func(c rune) string { return string(c) }
	for in, want := range map[string]string{
		"summary.md":                   "summary.md",
		"Q3 report (final).pdf":        "Q3 report (final).pdf",
		"naïve résumé 日本語.md":          "naïve résumé 日本語.md",
		"":                             "deliverable",
		"   ":                          "deliverable",
		"..":                           "deliverable",
		".":                            "deliverable",
		"...":                          "deliverable",
		"../../etc/passwd":             "_.._etc_passwd",
		`..\..\windows\system32`:       "_.._windows_system32",
		"a/b/c.txt":                    "a_b_c.txt",
		".bashrc":                      "bashrc",
		"report.":                      "report",
		"report. . ":                   "report",
		"what? <now>|*":                "what_ _now___",
		"a:b.txt":                      "a_b.txt",
		"file.txt:Zone.Identifier":     "file.txt_Zone.Identifier",
		"CON":                          "_CON",
		"con.txt":                      "_con.txt",
		"Nul.tar.gz":                   "_Nul.tar.gz",
		"COM1":                         "_COM1",
		"com0":                         "_com0",
		"LPT0.txt":                     "_LPT0.txt",
		"lpt9.log":                     "_lpt9.log",
		"COM10":                        "COM10",
		"CONSOLE.md":                   "CONSOLE.md",
		"COM" + r(0xb9) + ".txt":       "_COM" + r(0xb9) + ".txt",
		"CON .txt":                     "_CON .txt",
		"txt" + r(0x202e) + "fdp.exe":  "txt_fdp.exe",
		"a" + r(0x2066) + "b":          "a_b",
		"a" + r(0x200b) + "b":          "a_b",
		"a" + r(0xfeff) + "b":          "a_b",
		"a" + r(0x00ad) + "b":          "a_b",
		"a" + r(0x2028) + "b":          "a_b",
		"a" + r(0xa0) + "b":            "a_b",
		"a" + r(0x7) + "b":             "a_b",
		"a\nb\tc":                      "a_b_c",
		"a" + r(0) + "b":               "a_b",
		"a" + r(utf8.RuneError) + "b":  "a_b",
		string([]byte{'a', 0xff, 'b'}): "a_b",
		"CONIN$":                       "_CONIN$",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeNameIsOneBoundedSegment(t *testing.T) {
	long := strings.Repeat("é", 300) + ".markdown"
	got := SafeName(long)
	if len(got) > maxNameBytes || !strings.HasSuffix(got, ".markdown") || !utf8.ValidString(got) {
		t.Errorf("a long name = %d bytes %q", len(got), got)
	}
	if got := SafeName(strings.Repeat("a", 500)); len(got) != maxNameBytes {
		t.Errorf("a long name without an extension = %d bytes", len(got))
	}
	if got := SafeName("x." + strings.Repeat("e", 300)); len(got) > maxNameBytes || got == "" {
		t.Errorf("a long extension = %q", got)
	}
	for _, in := range []string{"a/b", `a\b`, "..", "a\x00b", "CON", "a" + string(rune(0x202e)) + "b"} {
		got := SafeName(in)
		if strings.ContainsAny(got, `/\`) || got == ".." || got == "." || got == "" {
			t.Errorf("SafeName(%q) = %q", in, got)
		}
	}
	// A name already safe is the same name, so a delivery twice names a file once.
	for _, in := range []string{"a.md", "Q3 report.pdf", "deliverable"} {
		if SafeName(SafeName(in)) != SafeName(in) {
			t.Errorf("SafeName is not idempotent for %q", in)
		}
	}
}
