package decode

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveTools are fakeTools that also name the archive types and stand in
// for bsdcat (gzip only) and bsdtar (whose "7z" here is a tar it passes on
// unchanged, and whose "encrypted" 7z fails as libarchive does), each
// checking its fixed argv.
func archiveTools(t *testing.T) Tools {
	t.Helper()
	tools := fakeTools(t)
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	detector := `case "$*" in
 *.zip) echo application/zip ;;
 *.tar) echo application/x-tar ;;
 *.gz) echo application/gzip ;;
 *.7z) echo application/x-7z-compressed ;;
 *.rar) echo "application/x-rar-compressed; version=5" ;;
 *report.pdf*) echo application/pdf ;;
 *memo.docx*) echo application/vnd.openxmlformats-officedocument.wordprocessingml.document ;;
 *) echo text/plain ;;
 esac`
	tools.File = script("file", `[ "$1 $2 $3" = "--brief --mime-type --" ] || exit 9; `+detector)
	tools.Tika = script("java", `[ "$1 $2 $3 $4 $5" = "-Xmx512m -Djava.io.tmpdir=/tmp -jar /opt/tika/tika-app.jar --detect" ] || exit 9; `+detector)
	tools.Bsdcat = script("bsdcat", `[ "$#" = 1 ] || exit 9; gzip -dc "$1"`)
	tools.Bsdtar = script("bsdtar", `[ "$1 $2 $3 $4 $5" = "-c -f - --format pax" ] && [ "$#" = 6 ] || exit 9
case "$6" in
 *enc*) echo "bsdtar: Error reading archive: Encryption is not supported" >&2; exit 1 ;;
 @*) cat "${6#@}" ;;
 *) exit 9 ;;
esac`)
	return tools
}

type zipEntry struct {
	name, body string
	mode       fs.FileMode
	flags      uint16
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		if e.flags != 0 {
			// An encrypted member: written raw, with the flag set.
			h.Flags, h.Method = e.flags, zip.Store
			h.CompressedSize64, h.UncompressedSize64 = uint64(len(e.body)), uint64(len(e.body))
			w, err := zw.CreateRaw(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarOf(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		body := h.Linkname
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
			h.Linkname = ""
		}
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// reg is a regular tar member whose body rides in Linkname for tarOf.
func reg(name, body string) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Linkname: body}
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeArchives(t *testing.T, lim Limits, files map[string][]byte) (Report, map[string]File, string) {
	t.Helper()
	in, out := t.TempDir(), t.TempDir()
	for name, data := range files {
		write(t, in, name, data)
	}
	rep, err := Decode(context.Background(), in, out, archiveTools(t), lim)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	return rep, got, out
}

func TestArchiveMembersAreDecodedLikeFiles(t *testing.T) {
	inner := tarOf(t, reg("memo.docx", "PK not really"), reg("notes/todo.md", "- ship M4\n"))
	_, got, out := decodeArchives(t, Limits{}, map[string][]byte{
		"bundle.zip": zipOf(t,
			zipEntry{name: "dir/"},
			zipEntry{name: "dir/readme.md", body: "# Read me\n"},
			zipEntry{name: "report.pdf", body: "%PDF not really"},
			zipEntry{name: "inner.tar", body: string(inner)},
		),
		"letter.md.gz":  gz(t, []byte("Dear reader,\n")),
		"backup.tgz.gz": gz(t, tarOf(t, reg("a.md", "alpha\n"))),
		"docs.7z":       tarOf(t, reg("b.md", "beta\n")),
		"docs.rar":      tarOf(t, reg("c.md", "gamma\n")),
	})
	want := map[string]struct {
		family  Family
		tool    string
		outcome Outcome
		in      string
		text    string
	}{
		"bundle.zip":                         {FamilyArchive, "agentry-runner", OutcomeUnpacked, "", ""},
		"bundle.zip/dir/readme.md":           {FamilyText, "", OutcomeText, "bundle.zip", "# Read me\n"},
		"bundle.zip/report.pdf":              {FamilyPDF, "pdftotext", OutcomeDecoded, "bundle.zip", "pdftotext read report.pdf\n"},
		"bundle.zip/inner.tar":               {FamilyArchive, "agentry-runner", OutcomeUnpacked, "bundle.zip", ""},
		"bundle.zip/inner.tar/memo.docx":     {FamilyDocument, "pandoc", OutcomeDecoded, "bundle.zip/inner.tar", "pandoc read memo.docx\n"},
		"bundle.zip/inner.tar/notes/todo.md": {FamilyText, "", OutcomeText, "bundle.zip/inner.tar", "- ship M4\n"},
		"letter.md.gz":                       {FamilyArchive, "bsdcat", OutcomeUnpacked, "", ""},
		"letter.md.gz/letter.md":             {FamilyText, "", OutcomeText, "letter.md.gz", "Dear reader,\n"},
		"backup.tgz.gz":                      {FamilyArchive, "bsdcat", OutcomeUnpacked, "", ""},
		"backup.tgz.gz/a.md":                 {FamilyText, "", OutcomeText, "backup.tgz.gz", "alpha\n"},
		"docs.7z":                            {FamilyArchive, "bsdtar", OutcomeUnpacked, "", ""},
		"docs.7z/b.md":                       {FamilyText, "", OutcomeText, "docs.7z", "beta\n"},
		"docs.rar":                           {FamilyArchive, "bsdtar", OutcomeUnpacked, "", ""},
		"docs.rar/c.md":                      {FamilyText, "", OutcomeText, "docs.rar", "gamma\n"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d: %+v", len(got), len(want), got)
	}
	for path, w := range want {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s: not in the report", path)
			continue
		}
		if f.Family != w.family || f.Tool != w.tool || f.Outcome != w.outcome || f.In != w.in || f.Why != "" {
			t.Errorf("%s: %+v, want family %q tool %q outcome %q in %q", path, f, w.family, w.tool, w.outcome, w.in)
			continue
		}
		if w.text == "" {
			if f.Output != "" {
				t.Errorf("%s: an archive names an output %q", path, f.Output)
			}
			continue
		}
		if f.Output != path+".txt" {
			t.Errorf("%s: output %q", path, f.Output)
			continue
		}
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(f.Output)))
		if err != nil || string(b) != w.text || f.Bytes != int64(len(w.text)) {
			t.Errorf("%s: text %q (%d recorded), %v; want %q", path, b, f.Bytes, err, w.text)
		}
	}
	if f := got["bundle.zip/dir/readme.md"]; f.Size != int64(len("# Read me\n")) {
		t.Errorf("a member's size: %+v", f)
	}
}

func TestHostileArchives(t *testing.T) {
	t.Run("a member that climbs out refuses the whole archive, and nothing is written", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"slip.zip":  zipOf(t, zipEntry{name: "ok.md", body: "fine\n"}, zipEntry{name: "../../evil.md", body: "x"}),
			"inner.zip": zipOf(t, zipEntry{name: "a/../b.md", body: "x"}),
			"abs.tar":   tarOf(t, reg("ok.md", "fine\n"), reg("/etc/evil.md", "x")),
			"slash.zip": zipOf(t, zipEntry{name: "dir\\evil.md", body: "x"}),
			"ctrl.tar":  tarOf(t, reg("bad\nname.md", "x")),
			"slip.7z":   tarOf(t, reg("ok.md", "fine\n"), reg("../evil.md", "x")),
		} {
			t.Run(name, func(t *testing.T) {
				scratch := t.TempDir()
				_, got, out := decodeArchives(t, Limits{ScratchDir: scratch}, map[string][]byte{name: data})
				f := got[name]
				if len(got) != 1 || f.Outcome != OutcomeRefused || f.Why != WhyUnsafePath {
					t.Fatalf("%+v", got)
				}
				_ = filepath.WalkDir(filepath.Dir(out), func(p string, d fs.DirEntry, err error) error {
					if err == nil && strings.Contains(d.Name(), "evil") {
						t.Errorf("written: %s", p)
					}
					return nil
				})
				if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
					t.Errorf("unpack directory left behind: %v", entries)
				}
			})
		}
	})
	t.Run("links, devices and FIFOs are refused, the rest is read", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{
			"links.zip": zipOf(t, zipEntry{name: "passwd", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777}, zipEntry{name: "ok.md", body: "fine\n"}),
			"special.tar": tarOf(t,
				tar.Header{Name: "fifo", Typeflag: tar.TypeFifo},
				tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
				tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "/etc/shadow"},
				tar.Header{Name: "soft", Typeflag: tar.TypeSymlink, Linkname: "../../.."},
				reg("ok.md", "fine\n"),
			),
		})
		for path, why := range map[string]Why{
			"links.zip/passwd": WhyLink, "special.tar/fifo": WhySpecial, "special.tar/dev": WhySpecial,
			"special.tar/hard": WhyLink, "special.tar/soft": WhyLink,
		} {
			if f := got[path]; f.Outcome != OutcomeRefused || f.Why != why || f.In == "" || f.Output != "" {
				t.Errorf("%s: %+v, want refused %s", path, f, why)
			}
		}
		for _, path := range []string{"links.zip/ok.md", "special.tar/ok.md"} {
			if got[path].Outcome != OutcomeText {
				t.Errorf("%s: %+v", path, got[path])
			}
		}
	})
	t.Run("an archive that unpacks past the budget stops there, as it streams", func(t *testing.T) {
		zeros := strings.Repeat("\x00", 2<<20)
		_, got, _ := decodeArchives(t, Limits{MaxUnpackedBytes: 1 << 20}, map[string][]byte{
			"bomb.zip": zipOf(t, zipEntry{name: "a.md", body: "first\n"}, zipEntry{name: "zeros.bin", body: zeros}, zipEntry{name: "z.md", body: "never\n"}),
		})
		if f := got["bomb.zip"]; f.Outcome != OutcomeUnpacked || f.Why != WhyTooLarge {
			t.Fatalf("%+v", f)
		}
		if _, ok := got["bomb.zip/zeros.bin"]; ok {
			t.Errorf("the member past the budget is listed: %+v", got)
		}
		if _, ok := got["bomb.zip/z.md"]; ok {
			t.Errorf("unpacking went on past the budget: %+v", got)
		}
		if got["bomb.zip/a.md"].Outcome != OutcomeText {
			t.Errorf("the member before the budget: %+v", got["bomb.zip/a.md"])
		}
	})
	t.Run("a bomb stops its own archive, not the archives after it", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{MaxUnpackedBytes: 1 << 20}, map[string][]byte{
			"a-bomb.zip": zipOf(t, zipEntry{name: "zeros.bin", body: strings.Repeat("\x00", 2<<20)}),
			"b-docs.zip": zipOf(t, zipEntry{name: "b.md", body: "after the bomb\n"}),
		})
		if got["a-bomb.zip"].Why != WhyTooLarge || got["b-docs.zip"].Why != "" || got["b-docs.zip/b.md"].Outcome != OutcomeText {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("the budget is shared by every archive", func(t *testing.T) {
		half := strings.Repeat("a", 600<<10)
		_, got, _ := decodeArchives(t, Limits{MaxUnpackedBytes: 1 << 20}, map[string][]byte{
			"one.zip": zipOf(t, zipEntry{name: "a.md", body: half}),
			"two.zip": zipOf(t, zipEntry{name: "b.md", body: half}),
		})
		if got["one.zip"].Why != "" || got["two.zip"].Why != WhyTooLarge {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("nesting past three levels is not unpacked", func(t *testing.T) {
		level4 := zipOf(t, zipEntry{name: "deep.md", body: "deep\n"})
		level3 := zipOf(t, zipEntry{name: "l4.zip", body: string(level4)})
		level2 := zipOf(t, zipEntry{name: "l3.zip", body: string(level3)})
		level1 := zipOf(t, zipEntry{name: "l2.zip", body: string(level2)})
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{"l1.zip": level1})
		for _, p := range []string{"l1.zip", "l1.zip/l2.zip", "l1.zip/l2.zip/l3.zip"} {
			if got[p].Outcome != OutcomeUnpacked {
				t.Errorf("%s: %+v", p, got[p])
			}
		}
		if f := got["l1.zip/l2.zip/l3.zip/l4.zip"]; f.Outcome != OutcomeUndecodable || f.Why != WhyTooDeep || f.In != "l1.zip/l2.zip/l3.zip" {
			t.Errorf("level 4: %+v", f)
		}
		if _, ok := got["l1.zip/l2.zip/l3.zip/l4.zip/deep.md"]; ok {
			t.Errorf("level 4 was unpacked")
		}
	})
	t.Run("members count against the file cap with the files as staged", func(t *testing.T) {
		var entries []zipEntry
		for _, n := range []string{"a", "b", "c", "d", "e"} {
			entries = append(entries, zipEntry{name: n + ".md", body: n})
		}
		rep, got, _ := decodeArchives(t, Limits{MaxFiles: 3}, map[string][]byte{"many.zip": zipOf(t, entries...), "z.md": []byte("z\n")})
		if f := got["many.zip"]; f.Outcome != OutcomeUnpacked || f.Why != WhyTooMany {
			t.Fatalf("%+v", rep.Files)
		}
		if _, ok := got["many.zip/b.md"]; !ok || len(got) != 4 {
			t.Fatalf("%+v", rep.Files)
		}
		if f := got["z.md"]; f.Outcome != OutcomeRefused {
			t.Errorf("a file staged after the cap: %+v", f)
		}
	})
	t.Run("an encrypted member is unread, the others are read", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{
			"locked.zip": zipOf(t, zipEntry{name: "secret.md", body: "ciphertext", flags: 0x1}, zipEntry{name: "open.md", body: "open\n"}),
		})
		if f := got["locked.zip/secret.md"]; f.Outcome != OutcomeUndecodable || f.Why != WhyEncrypted || f.Output != "" {
			t.Errorf("%+v", f)
		}
		if got["locked.zip/open.md"].Outcome != OutcomeText || got["locked.zip"].Outcome != OutcomeUnpacked {
			t.Errorf("%+v", got)
		}
	})
	t.Run("an archive libarchive calls encrypted is unread", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{"enc.7z": []byte("7z not really")})
		if f := got["enc.7z"]; len(got) != 1 || f.Outcome != OutcomeUndecodable || f.Why != WhyEncrypted {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a damaged archive is unread", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{"broken.zip": []byte("PK\x03\x04 nothing more"), "broken.gz": []byte("\x1f\x8b not gzip")})
		for _, p := range []string{"broken.zip", "broken.gz"} {
			if f := got[p]; f.Outcome != OutcomeUndecodable || f.Why != WhyDamaged {
				t.Errorf("%s: %+v", p, f)
			}
		}
	})
	t.Run("a path named twice conflicts", func(t *testing.T) {
		_, got, _ := decodeArchives(t, Limits{}, map[string][]byte{
			"dup.tar":   tarOf(t, reg("a.md", "first\n"), reg("a.md", "second\n")),
			"under.tar": tarOf(t, reg("a.md", "a file\n"), reg("a.md/b.md", "under a file\n")),
		})
		if f := got["dup.tar/a.md"]; f.Why != WhyConflict || f.Outcome != OutcomeRefused || f.Output != "" {
			t.Errorf("%+v", got)
		}
		if f := got["under.tar/a.md/b.md"]; f.Why != WhyConflict || f.Outcome != OutcomeRefused || got["under.tar/a.md"].Outcome != OutcomeText {
			t.Errorf("%+v", got)
		}
	})
}

// endless is a member that never ends: only a budget checked as it
// streams stops it.
type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }

func TestTheBudgetIsCheckedAsTheMemberStreams(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	d := &decoder{lim: Limits{MaxFiles: 10}, budget: 1 << 20}
	u := &unpacker{d: d, root: root, named: map[string]int{}}
	if u.file("endless.bin", endless{}) || u.stop != WhyTooLarge || len(u.members) != 0 {
		t.Fatalf("stop %q members %+v", u.stop, u.members)
	}
	if _, err := root.Stat("endless.bin"); !os.IsNotExist(err) {
		t.Fatalf("the member past the budget was left: %v", err)
	}
	if d.budget != 1<<20 {
		t.Fatalf("the removed member's bytes were counted: %d left", d.budget)
	}
}
