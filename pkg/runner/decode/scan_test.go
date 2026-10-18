package decode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// signedImage is sampleImage with signatures mounted.
func signedImage() Image {
	img := sampleImage
	img.Signatures = clamdb.DB{Dir: "/stand-in/clamav", Ref: policy.DefaultClamAVSignatures, Digest: "sha256:84cbc016d58b"}
	return img
}

// A report that calls a file clean, decoded and readable, as a decoder a
// file exploited could have rewritten it, is held to the scan record the
// runner stated before any decoder ran: the file is found, refused and
// has no text, and a member the report left out is found too. A line
// written after the record changes nothing.
func TestAReportsCleanIsRefusedWhenTheScanFoundIt(t *testing.T) {
	in := t.TempDir()
	for _, n := range []string{"eicar.com", "report.pdf", "bundle.zip"} {
		if err := os.WriteFile(filepath.Join(in, n), []byte("document "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	forged := []runnerdecode.File{
		{Path: "bundle.zip", Family: "archive", Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked, Check: runnerdecode.CheckClean},
		{Path: "eicar.com", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "eicar.com.txt", Bytes: 20, Check: runnerdecode.CheckClean},
		{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "report.pdf.txt", Bytes: 20, Check: runnerdecode.CheckClean},
	}
	record := `{"schema":"agentry.decode.scan/v2","checked":true,"files":[{"path":"bundle.zip","check":"found","signature":"Eicar-Test-Signature"},` +
		`{"path":"bundle.zip/docs/bad.txt","check":"found","signature":"Eicar-Test-Signature"},{"path":"eicar.com","check":"found","signature":"Eicar-Test-Signature"},` +
		`{"path":"report.pdf","check":"unchecked","unchecked":"too-large"}]}` + "\n" +
		`{"schema":"agentry.decode.scan/v2","checked":true,"files":[{"path":"bundle.zip","check":"clean"},{"path":"eicar.com","check":"clean"},{"path":"report.pdf","check":"clean"}]}` + "\n"
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, forged), Stdout: record}}}
	res, err := Run(context.Background(), f, signedImage(), in, t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused() {
		t.Fatal("a run whose scan found a file is not refused")
	}
	found := map[string]runnerdecode.File{}
	for _, f := range res.Found() {
		found[f.Path] = f
	}
	for _, p := range []string{"eicar.com", "bundle.zip/docs/bad.txt"} {
		f, ok := found[p]
		if !ok || f.Signature != "Eicar-Test-Signature" || f.Outcome != runnerdecode.OutcomeRefused || f.Why != runnerdecode.WhyFound || f.HasText() {
			t.Fatalf("%s: %+v (found %v)", p, f, ok)
		}
	}
	if _, ok := found["report.pdf"]; ok {
		t.Fatal("a file the scan did not find was taken as found")
	}
	// The archive is found and still unpacked: its members answer for
	// themselves. The file the scan could not check is not clean.
	byPath := map[string]runnerdecode.File{}
	for _, f := range res.Report.Files {
		byPath[f.Path] = f
	}
	if z := byPath["bundle.zip"]; z.Check != runnerdecode.CheckFound || z.Outcome != runnerdecode.OutcomeUnpacked || z.Why != "" {
		t.Fatalf("the archive: %+v", z)
	}
	if p := byPath["report.pdf"]; p.Check != runnerdecode.CheckUnchecked || p.Unchecked != runnerdecode.UncheckedTooLarge {
		t.Fatalf("a file the scan could not check reads %+v", p)
	}
	if err := res.CopyText("eicar.com", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("the text of a file the scan found was copied out")
	}
	if !strings.Contains(res.Refusal(), "eicar.com (Eicar-Test-Signature)") || !strings.Contains(res.Refusal(), "bundle.zip/docs/bad.txt (Eicar-Test-Signature)") {
		t.Fatalf("refusal %q", res.Refusal())
	}
}

// A decode given signatures that stated no scan record before it decoded,
// or one that is not a check with signatures, is refused; one given none
// needs no record.
func TestADecodeThatStatesNoScanIsRefused(t *testing.T) {
	v2 := func(files string) string {
		return `{"schema":"agentry.decode.scan/v2","checked":true,"files":[` + files + `]}` + "\n"
	}
	all := `{"path":"blob.bin","check":"clean"},{"path":"deck.pptx","check":"clean"},{"path":"notes.md","check":"clean"},{"path":"report.pdf","check":"clean"}`
	for name, stdout := range map[string]string{
		"nothing":                 "",
		"no line end":             strings.TrimSuffix(v2(all), "\n"),
		"junk first":              "decoded 3 files\n" + v2(all),
		"not checked":             `{"schema":"agentry.decode.scan/v2","checked":false,"files":[]}` + "\n",
		"another schema":          `{"schema":"agentry.decode.scan/v1","checked":true,"files":[]}` + "\n",
		"an unknown field":        `{"schema":"agentry.decode.scan/v2","checked":true,"files":[],"clean":["eicar.com"]}` + "\n",
		"a path that climbs":      v2(all + `,{"path":"../etc/passwd","check":"found"}`),
		"a path twice":            v2(all + `,{"path":"notes.md","check":"found"}`),
		"an unknown check":        v2(`{"path":"blob.bin","check":"safe"}`),
		"an unknown reason":       v2(`{"path":"blob.bin","check":"unchecked","unchecked":"trusted"}`),
		"a check the scan missed": v2(`{"path":"blob.bin","check":"clean"}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, sampleFiles), Stdout: stdout}}}
			if _, err := Run(context.Background(), f, signedImage(), staged(t), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr); err == nil {
				t.Fatal("the decode was accepted")
			}
		})
	}
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, sampleFiles)}}}
	if _, err := Run(context.Background(), f, sampleImage, staged(t), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr); err != nil {
		t.Fatalf("a decode with no signatures needed a record: %v", err)
	}
}

// A signature outside the grammar is named as no signature, never shown.
func TestAScanRecordsSignatureIsHeldToTheGrammar(t *testing.T) {
	stdout := `{"schema":"agentry.decode.scan/v2","checked":true,"files":[{"path":"blob.bin","check":"found","signature":"Bad Name; rm -rf /"},` +
		`{"path":"deck.pptx","check":"clean"},{"path":"notes.md","check":"clean"},{"path":"report.pdf","check":"clean"}]}` + "\n"
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, sampleFiles), Stdout: stdout}}}
	res, err := Run(context.Background(), f, signedImage(), staged(t), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if fs := res.Found(); len(fs) != 1 || fs[0].Path != "blob.bin" || fs[0].Signature != "" {
		t.Fatalf("found %+v", fs)
	}
}
