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
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// TestDecodeArchivesForReal runs the real decode image over archives (plan
// file-handling.md §6, D5; agentry-cli#562): a zip, a 7z, a tar.xz, a rar
// and a lone .gz of documents, an archive nested in a zip, and the hostile
// corpus: a zip slip, link members, nesting past three, a bomb past the
// 512 MiB budget, encrypted zips and rars. Each is read or refused exactly
// as the plan says, and nothing lands outside the decode's output. The
// 2,000-member cap is the unit tests' (runnerimage/decode): a real decode
// of 2,001 members spends most of an hour starting Tika. It needs a
// docker daemon, so it runs only when AGENTRY_DECODE_E2E=1.
func TestDecodeArchivesForReal(t *testing.T) {
	if os.Getenv("AGENTRY_DECODE_E2E") != "1" {
		t.Skip("set AGENTRY_DECODE_E2E=1 to build the decode image and decode the archives with the real decoders")
	}
	ctx := context.Background()
	d := dockerx.New()
	pol := policy.Default()
	home := t.TempDir()
	tag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(ctx, d, pol, tag, filepath.Join(home, "image"), os.Stderr, false); err != nil {
		t.Fatal(err)
	}
	img, err := Ensure(ctx, d, tag, pol, filepath.Join(home, "image"), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// The real signatures too, so clamscan reads every hostile archive and
	// member, the bomb's included, beside the decoders (agentry-cli#563).
	img.Signatures = clamdb.Ensure(ctx, home, pol.Images.ClamAVSignaturesRef(), os.Stderr)
	if img.Signatures.Dir == "" {
		t.Fatalf("no signatures: %+v", img.Signatures)
	}
	corpus := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "knowledge", "creator", "testdata", "corpus", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	in, out := t.TempDir(), t.TempDir()
	if err := os.CopyFS(in, os.DirFS("testdata/m4")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE-libarchive", "NOTICE.md"} {
		if err := os.Remove(filepath.Join(in, name)); err != nil {
			t.Fatal(err)
		}
	}
	put := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(in, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inner := gzipped(t, tarred(t, map[string][]byte{"memo.rtf": corpus("memo.rtf")}))
	put("docs.zip", zipped(t, zipMember{"policy.docx", corpus("policy.docx"), 0}, zipMember{"notes/runbook.md", corpus("runbook.md"), 0}, zipMember{"inner.tar.gz", inner, 0}))
	put("report.pdf.gz", gzipped(t, corpus("incident.pdf")))
	put("slip.zip", zipped(t, zipMember{"ok.md", []byte("fine\n"), 0}, zipMember{"../../evil.md", []byte("escaped\n"), 0}))
	put("links.zip", zipped(t, zipMember{"passwd", []byte("/etc/passwd"), fs.ModeSymlink | 0o777}, zipMember{"ok.md", []byte("fine\n"), 0}))
	l4 := zipped(t, zipMember{"deep.md", []byte("too deep\n"), 0})
	l3 := zipped(t, zipMember{"l4.zip", l4, 0})
	l2 := zipped(t, zipMember{"l3.zip", l3, 0})
	put("deep.zip", zipped(t, zipMember{"l2.zip", l2, 0}))
	put("bomb.zip", zipped(t, zipMember{"first.md", []byte("before the bomb\n"), 0}, zipMember{"zeros.bin", make([]byte, 600<<20), 0}, zipMember{"last.md", []byte("after the bomb\n"), 0}))

	start := time.Now()
	res, err := Run(ctx, d, img, in, out, pol.Docker.Limits.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decoded in %s", time.Since(start).Round(time.Second))
	type want struct {
		outcome runnerdecode.Outcome
		tool    string
		why     runnerdecode.Why
		phrase  string
	}
	unpacked := func(tool string) want { return want{outcome: runnerdecode.OutcomeUnpacked, tool: tool} }
	text := want{outcome: runnerdecode.OutcomeText}
	wants := map[string]want{
		"docs.zip":                       unpacked("agentry-runner"),
		"docs.zip/policy.docx":           {runnerdecode.OutcomeDecoded, "pandoc", "", "45-minute break"},
		"docs.zip/notes/runbook.md":      text,
		"docs.zip/inner.tar.gz":          unpacked("bsdcat"),
		"docs.zip/inner.tar.gz/memo.rtf": {runnerdecode.OutcomeDecoded, "pandoc", "", "Antwerp is ANR"},
		"docs.7z":                        unpacked("bsdtar"),
		"docs.7z/policy.docx":            {runnerdecode.OutcomeDecoded, "pandoc", "", "45-minute break"},
		"docs.7z/incident.pdf":           {runnerdecode.OutcomeDecoded, "pdftotext", "", "fuel filter was clogged"},
		"docs.tar.xz":                    unpacked("bsdcat"),
		"docs.tar.xz/slides/deck.pptx":   {runnerdecode.OutcomeDecoded, "agentry-runner", "", "Cold-chain incidents: 3"},
		"report.pdf.gz":                  unpacked("bsdcat"),
		"report.pdf.gz/report.pdf":       {runnerdecode.OutcomeDecoded, "pdftotext", "", "fuel filter was clogged"},
		"links.rar":                      unpacked("bsdtar"),
		"links.rar/test.txt":             text,
		"links.rar/testdir/test.txt":     text,
		"links.rar/testlink":             {runnerdecode.OutcomeRefused, "", runnerdecode.WhyLink, ""},
		"encrypted5.rar":                 {runnerdecode.OutcomeUndecodable, "bsdtar", runnerdecode.WhyEncrypted, ""},
		"locked.zip":                     unpacked("agentry-runner"),
		"locked.zip/secret.md":           {runnerdecode.OutcomeUndecodable, "", runnerdecode.WhyEncrypted, ""},
		"locked.zip/open.md":             text,
		"sealed.zip":                     unpacked("agentry-runner"),
		"sealed.zip/secret.md":           {runnerdecode.OutcomeUndecodable, "", runnerdecode.WhyEncrypted, ""},
		"slip.zip":                       {runnerdecode.OutcomeRefused, "agentry-runner", runnerdecode.WhyUnsafePath, ""},
		"slip.7z":                        {runnerdecode.OutcomeRefused, "bsdtar", runnerdecode.WhyUnsafePath, ""},
		"links.zip":                      unpacked("agentry-runner"),
		"links.zip/passwd":               {runnerdecode.OutcomeRefused, "", runnerdecode.WhyLink, ""},
		"links.zip/ok.md":                text,
		"deep.zip":                       unpacked("agentry-runner"),
		"deep.zip/l2.zip":                unpacked("agentry-runner"),
		"deep.zip/l2.zip/l3.zip":         unpacked("agentry-runner"),
		"deep.zip/l2.zip/l3.zip/l4.zip":  {runnerdecode.OutcomeUndecodable, "agentry-runner", runnerdecode.WhyTooDeep, ""},
		"bomb.zip":                       {runnerdecode.OutcomeUnpacked, "agentry-runner", runnerdecode.WhyTooLarge, ""},
		"bomb.zip/first.md":              text,
	}
	got := map[string]runnerdecode.File{}
	for _, f := range res.Report.Files {
		got[f.Path] = f
	}
	// libarchive cannot always tell an encrypted rar from a damaged one;
	// either reason is the honest answer, and nothing in it is read.
	if f := got["encrypted.rar"]; f.Outcome != runnerdecode.OutcomeUndecodable || (f.Why != runnerdecode.WhyEncrypted && f.Why != runnerdecode.WhyUnreadable) {
		t.Errorf("encrypted.rar: %+v", f)
	}
	delete(got, "encrypted.rar")
	for path, w := range wants {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s: not in the report", path)
			continue
		}
		delete(got, path)
		if f.Outcome != w.outcome || f.Tool != w.tool || f.Why != w.why {
			t.Errorf("%s: outcome %s tool %q why %q (%s), want %s %q %q", path, f.Outcome, f.Tool, f.Why, f.Reason, w.outcome, w.tool, w.why)
			continue
		}
		if w.phrase == "" {
			continue
		}
		dst := filepath.Join(t.TempDir(), "text")
		if err := res.CopyText(f.Path, dst); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if b, _ := os.ReadFile(dst); !strings.Contains(string(b), w.phrase) {
			t.Errorf("%s: the text does not hold %q:\n%.400s", f.Path, w.phrase, b)
		}
	}
	for path, f := range got {
		t.Errorf("unexpected %s: %+v", path, f)
	}
	// Nothing here is malware, and clamscan answered for every file it was
	// given: clean, or past its limits (the bomb's leftovers).
	for _, f := range res.Report.Files {
		switch {
		case f.Check == runnerdecode.CheckFound:
			t.Errorf("%s: ClamAV found %q", f.Path, f.Signature)
		case f.Check == runnerdecode.CheckUnchecked && f.Unchecked != runnerdecode.UncheckedTooLarge:
			t.Errorf("%s: unchecked (%s)", f.Path, f.Unchecked)
		}
	}
	_ = filepath.WalkDir(filepath.Dir(out), func(p string, e fs.DirEntry, err error) error {
		if err == nil && (strings.Contains(e.Name(), "evil") || strings.Contains(e.Name(), "zeros")) {
			t.Errorf("written outside what the decode reads: %s", p)
		}
		return nil
	})
	t.Logf("receipt line: %s", res.Line())
}

type zipMember struct {
	name string
	data []byte
	mode fs.FileMode
}

func zipped(t *testing.T, members ...zipMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		if m.mode != 0 {
			h.SetMode(m.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarred(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
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
