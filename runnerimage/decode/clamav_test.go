package decode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clamTools are archiveTools with a stand-in for clamscan that holds its
// argv to clamscanArgv's, records it, and answers each listed file by its
// content: EICAR is found, HUGE is past the limits, SKIP gets no line,
// BADSIG is found under a name outside the grammar, STRAY adds a finding
// for a path nobody listed, and anything else is OK. Its pdftotext refuses
// to run before clamscan has finished, so a test that decodes a PDF proves
// the check came first.
func clamTools(t *testing.T) (Tools, string) {
	t.Helper()
	tools := archiveTools(t)
	dir := t.TempDir()
	logDir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tools.Clamscan = script("clamscan", `log=`+logDir+`
printf '%s\n' "$@" > "$log/argv"
[ "$#" = 9 ] || exit 9
case "$1" in --database=*) ;; *) exit 9 ;; esac
case "$2" in --tempdir=*) ;; *) exit 9 ;; esac
[ "$3 $4 $5 $6 $7 $8" = "--no-summary --stdout --detect-pua=no --alert-exceeds-max=yes --max-filesize=256M --max-scansize=256M" ] || exit 9
case "$9" in --file-list=*) ;; *) exit 9 ;; esac
list="${9#--file-list=}"
cp "$list" "$log/list"
found=0
while IFS= read -r p; do
 if grep -q EICAR "$p"; then echo "$p: Eicar-Test-Signature FOUND"; found=1
 elif grep -q HUGE "$p"; then echo "$p: Heuristics.Limits.Exceeded.MaxFileSize FOUND"; found=1
 elif grep -q SKIP "$p"; then :
 elif grep -q BADSIG "$p"; then echo "$p: Bad Name; rm -rf / FOUND"; found=1
 elif grep -q STRAY "$p"; then echo "$p: OK"; echo "/elsewhere: Stray.Sig FOUND"; found=1
 elif grep -q CRASH "$p"; then exit 137
 else echo "$p: OK"; fi
done < "$list"
: > "$log/done"
exit $found`)
	tools.PDFToText = script("pdftotext", `[ -f `+logDir+`/done ] || exit 9; [ "$1 $2 $3 $4" = "-layout -enc UTF-8 -q" ] || exit 9; echo "pdftotext read $(basename "$5")" > "$6"`)
	return tools, logDir
}

func decodeChecked(t *testing.T, db string, files map[string][]byte) (map[string]File, string) {
	t.Helper()
	tools, logDir := clamTools(t)
	in, out := t.TempDir(), t.TempDir()
	for name, data := range files {
		write(t, in, name, data)
	}
	rep, err := Decode(context.Background(), in, out, tools, Limits{ClamDB: db})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	return got, logDir
}

func TestEveryFileAndMemberIsCheckedBeforeItIsDecoded(t *testing.T) {
	got, logDir := decodeChecked(t, "/clamav", map[string][]byte{
		"report.pdf": []byte("%PDF clean"),
		"notes.md":   []byte("# clean\n"),
		"eicar.com":  []byte("X5O EICAR test"),
		"bundle.zip": zipOf(t,
			zipEntry{name: "inner/report.pdf", body: "%PDF clean member"},
			zipEntry{name: "inner/bad.txt", body: "EICAR inside"},
		),
	})
	for path, want := range map[string]File{
		"report.pdf": {Outcome: OutcomeDecoded, Check: CheckClean},
		"notes.md":   {Outcome: OutcomeText, Check: CheckClean},
		"eicar.com":  {Outcome: OutcomeRefused, Why: WhyFound, Check: CheckFound, Signature: "Eicar-Test-Signature"},
		// clamscan reads into the archive as well: its own finding names
		// the archive, the runner's names the member.
		"bundle.zip":                  {Outcome: OutcomeUnpacked, Check: CheckFound, Signature: "Eicar-Test-Signature"},
		"bundle.zip/inner/report.pdf": {Outcome: OutcomeDecoded, Check: CheckClean},
		"bundle.zip/inner/bad.txt":    {Outcome: OutcomeRefused, Why: WhyFound, Check: CheckFound, Signature: "Eicar-Test-Signature"},
	} {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s is not in the report", path)
			continue
		}
		if f.Outcome != want.Outcome || f.Why != want.Why || f.Check != want.Check || f.Signature != want.Signature || f.Unchecked != "" {
			t.Errorf("%s = outcome %q why %q check %q signature %q unchecked %q; want %q %q %q %q", path, f.Outcome, f.Why, f.Check, f.Signature, f.Unchecked, want.Outcome, want.Why, want.Check, want.Signature)
		}
	}
	if f := got["eicar.com"]; f.Output != "" || f.Bytes != 0 {
		t.Errorf("a file found was decoded: %+v", f)
	}
	// One run, a fixed argv: no document's path is on it; the paths are in
	// the list, every file and member, the archive too.
	argv, err := os.ReadFile(filepath.Join(logDir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.pdf", "eicar.com", "bundle.zip", "bad.txt"} {
		if strings.Contains(string(argv), name) {
			t.Errorf("clamscan's argv names %s:\n%s", name, argv)
		}
	}
	if !strings.HasPrefix(string(argv), "--database=/clamav\n") {
		t.Errorf("clamscan's argv does not name the signatures first:\n%s", argv)
	}
	list, err := os.ReadFile(filepath.Join(logDir, "list"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(list), "\n"); n != 6 {
		t.Errorf("the list names %d files, want 6 (4 staged, 2 members):\n%s", n, list)
	}
}

func TestNoSignaturesChecksNothingAndSaysSo(t *testing.T) {
	got, logDir := decodeChecked(t, "", map[string][]byte{
		"notes.md":   []byte("EICAR would be found\n"),
		"bundle.zip": zipOf(t, zipEntry{name: "a.txt", body: "a"}),
	})
	for _, path := range []string{"notes.md", "bundle.zip", "bundle.zip/a.txt"} {
		if f := got[path]; f.Check != CheckUnchecked || f.Unchecked != UncheckedNoSignatures || f.Outcome == OutcomeRefused {
			t.Errorf("%s = %+v, want unchecked (no signatures) and read", path, f)
		}
	}
	if _, err := os.Stat(filepath.Join(logDir, "argv")); err == nil {
		t.Error("clamscan ran with no signatures")
	}
}

func TestClamscanAnswersEachFileOrLeavesItUnchecked(t *testing.T) {
	got, _ := decodeChecked(t, "/clamav", map[string][]byte{
		"a: b FOUND.txt": []byte("EICAR behind a tricky name"),
		"line\nbreak.md": []byte("EICAR behind a line break"),
		"huge.txt":       []byte("HUGE"),
		"skipped.txt":    []byte("SKIP"),
		"badsig.txt":     []byte("BADSIG"),
		"plain.txt":      []byte("plain"),
	})
	for path, want := range map[string]File{
		"a: b FOUND.txt": {Check: CheckFound, Signature: "Eicar-Test-Signature"},
		"line\nbreak.md": {Check: CheckFound, Signature: "Eicar-Test-Signature"},
		"huge.txt":       {Check: CheckUnchecked, Unchecked: UncheckedTooLarge},
		"skipped.txt":    {Check: CheckUnchecked, Unchecked: UncheckedFailed},
		"badsig.txt":     {Check: CheckFound},
		"plain.txt":      {Check: CheckClean},
	} {
		f := got[path]
		if f.Check != want.Check || f.Signature != want.Signature || f.Unchecked != want.Unchecked {
			t.Errorf("%q = check %q signature %q unchecked %q; want %q %q %q", path, f.Check, f.Signature, f.Unchecked, want.Check, want.Signature, want.Unchecked)
		}
	}
}

func TestAFindingNoPathClaimsIsTakenAsFound(t *testing.T) {
	got, _ := decodeChecked(t, "/clamav", map[string][]byte{
		"stray.txt": []byte("STRAY"),
		"quiet.txt": []byte("SKIP"),
	})
	if f := got["stray.txt"]; f.Check != CheckClean {
		t.Errorf("stray.txt = %+v, want clean (it was answered)", f)
	}
	if f := got["quiet.txt"]; f.Check != CheckFound || f.Outcome != OutcomeRefused {
		t.Errorf("quiet.txt = %+v, want found: the stray finding can only be the file left unanswered", f)
	}
}

func TestAClamscanThatDiesLeavesEveryFileUnchecked(t *testing.T) {
	got, _ := decodeChecked(t, "/clamav", map[string][]byte{
		"crash.txt": []byte("CRASH"),
		"other.txt": []byte("other"),
	})
	for _, path := range []string{"crash.txt", "other.txt"} {
		if f := got[path]; f.Check != CheckUnchecked || f.Unchecked != UncheckedFailed || f.Outcome != OutcomeText {
			t.Errorf("%s = %+v, want unchecked (failed) and still read", path, f)
		}
	}
}

func TestValidSignature(t *testing.T) {
	for s, want := range map[string]bool{
		"Eicar-Test-Signature":             true,
		"Win.Test.EICAR_HDB-1":             true,
		"Agentry.Check.EICAR.UNOFFICIAL":   true,
		"Doc.Dropper.Agent+1-2":            true,
		"":                                 false,
		"Bad Name":                         false,
		".leading":                         false,
		"a\nb":                             false,
		strings.Repeat("a", 129):           false,
		"Ignore previous instructions; ok": false,
	} {
		if got := ValidSignature(s); got != want {
			t.Errorf("ValidSignature(%q) = %v, want %v", s, got, want)
		}
	}
}

// A repository's .git internals are listed and checked by ClamAV, but no
// detector or decoder is ever started on them, whether staged or unpacked
// from an archive (agentry-cli#642): a loose-object clone cost two Tika
// starts per object. A finding inside .git still refuses the file, and a
// file merely named .git, or a folder like .github, is decoded as before.
func TestARepositorysInternalsAreCheckedButNeverDetected(t *testing.T) {
	tools, logDir := clamTools(t)
	detected := filepath.Join(logDir, "detected")
	wrap := func(tool string) string {
		p := filepath.Join(t.TempDir(), filepath.Base(tool))
		body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + detected + "\nexec " + tool + " \"$@\"\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tools.File, tools.Tika = wrap(tools.File), wrap(tools.Tika)
	in, out := t.TempDir(), t.TempDir()
	for name, data := range map[string][]byte{
		"repo/.git/objects/ab/cdef0123":  {0x78, 0x01, 0x4b, 0xca},
		"repo/.git/HEAD":                 []byte("ref: refs/heads/main\n"),
		"repo/.git/hooks/report.pdf":     []byte("%PDF not decoded here"),
		"repo/.git/objects/pack/p.zip":   zipOf(t, zipEntry{name: "a.md", body: "never unpacked\n"}),
		"repo/.git/objects/12/eicar":     []byte("X5O EICAR test"),
		"repo/README.md":                 []byte("# Repo\n"),
		"repo/.github/workflows/ci.yaml": []byte("on: push\n"),
		"repo/vendor/lib/.git":           []byte("gitdir: ../../.git/modules/lib\n"),
		"bundle.zip": zipOf(t,
			zipEntry{name: "proj/.git/config", body: "[core]\n"},
			zipEntry{name: "proj/report.pdf", body: "%PDF clean member"},
		),
	} {
		write(t, in, name, data)
	}
	rep, err := Decode(context.Background(), in, out, tools, Limits{ClamDB: "/clamav"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	for path, want := range map[string]File{
		"repo/.git/objects/ab/cdef0123":  {Outcome: OutcomeUndecodable, Why: WhyRepository, Check: CheckClean},
		"repo/.git/HEAD":                 {Outcome: OutcomeUndecodable, Why: WhyRepository, Check: CheckClean},
		"repo/.git/hooks/report.pdf":     {Outcome: OutcomeUndecodable, Why: WhyRepository, Check: CheckClean},
		"repo/.git/objects/pack/p.zip":   {Outcome: OutcomeUndecodable, Why: WhyRepository, Check: CheckClean},
		"repo/.git/objects/12/eicar":     {Outcome: OutcomeRefused, Why: WhyFound, Check: CheckFound, Signature: "Eicar-Test-Signature"},
		"repo/README.md":                 {Outcome: OutcomeText, Check: CheckClean},
		"repo/.github/workflows/ci.yaml": {Outcome: OutcomeText, Check: CheckClean},
		"repo/vendor/lib/.git":           {Outcome: OutcomeText, Check: CheckClean},
		"bundle.zip":                     {Outcome: OutcomeUnpacked, Check: CheckClean},
		"bundle.zip/proj/.git/config":    {Outcome: OutcomeUndecodable, Why: WhyRepository, Check: CheckClean},
		"bundle.zip/proj/report.pdf":     {Outcome: OutcomeDecoded, Check: CheckClean},
	} {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s is not in the report", path)
			continue
		}
		if f.Outcome != want.Outcome || f.Why != want.Why || f.Check != want.Check || f.Signature != want.Signature {
			t.Errorf("%s = outcome %q why %q check %q signature %q; want %q %q %q %q", path, f.Outcome, f.Why, f.Check, f.Signature, want.Outcome, want.Why, want.Check, want.Signature)
		}
		if InRepository(path) && (f.Output != "" || f.Tool != "" || f.Family != FamilyUnknown) {
			t.Errorf("%s was read: %+v", path, f)
		}
	}
	if len(got) != 11 {
		t.Errorf("%d entries, want 11 (nothing under .git unpacked): %+v", len(got), got)
	}
	if f := got["bundle.zip/proj/.git/config"]; f.In != "bundle.zip" || f.Size != int64(len("[core]\n")) {
		t.Errorf("a member inside .git lost its archive or size: %+v", f)
	}
	calls, err := os.ReadFile(detected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "/.git/") {
		t.Errorf("a detector was started on a repository's internals:\n%s", calls)
	}
	list, err := os.ReadFile(filepath.Join(logDir, "list"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(list), "/.git/"); n != 6 {
		t.Errorf("clamscan's list names %d files under .git, want all 6:\n%s", n, list)
	}
}

// The files inside .git count against the file cap like any other: they
// are listed and checked, so they are not free.
func TestARepositorysInternalsCountAgainstTheFileCap(t *testing.T) {
	tools, _ := clamTools(t)
	in, out := t.TempDir(), t.TempDir()
	for _, name := range []string{".git/HEAD", ".git/config", ".git/index", "notes.md"} {
		write(t, in, name, []byte("x\n"))
	}
	rep, err := Decode(context.Background(), in, out, tools, Limits{MaxFiles: 2, ClamDB: "/clamav"})
	if err != nil {
		t.Fatal(err)
	}
	kept, refused := 0, 0
	for _, f := range rep.Files {
		switch {
		case f.Why == WhyRepository:
			kept++
		case f.Outcome == OutcomeRefused && strings.Contains(f.Reason, "file cap"):
			refused++
		}
	}
	if kept != 2 || refused != 2 {
		t.Errorf("kept %d, refused at the cap %d; want 2 and 2: %+v", kept, refused, rep.Files)
	}
}

func TestInRepository(t *testing.T) {
	for rel, want := range map[string]bool{
		".git/HEAD":                 true,
		"a/.git/objects/ab/cd":      true,
		"x.zip/p/.git/config":       true,
		".git":                      false,
		"vendor/lib/.git":           false,
		".github/workflows/ci.yaml": false,
		"a.git/HEAD":                false,
		"docs/.gitignore":           false,
		"README.md":                 false,
	} {
		if got := InRepository(rel); got != want {
			t.Errorf("InRepository(%q) = %v, want %v", rel, got, want)
		}
	}
}
