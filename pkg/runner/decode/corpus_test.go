package decode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// TestDecodeTheCorpusForReal runs the real decode image — pdftotext,
// pandoc and tesseract as Alpine ships them, and the runner's own PPTX and
// XLSX reader — over the Creator's fixture corpus, one file per family.
// It needs a docker daemon and builds the runner and decode images, so it
// runs only when AGENTRY_DECODE_E2E=1; the unit tests fake the decoders.
func TestDecodeTheCorpusForReal(t *testing.T) {
	if os.Getenv("AGENTRY_DECODE_E2E") != "1" {
		t.Skip("set AGENTRY_DECODE_E2E=1 to build the decode image and decode the corpus with the real decoders")
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
	in, out := t.TempDir(), t.TempDir()
	if err := os.CopyFS(in, os.DirFS(filepath.Join("..", "..", "knowledge", "creator", "testdata", "corpus"))); err != nil {
		t.Fatal(err)
	}
	res, err := Run(ctx, d, img, in, out, pol.Docker.Limits.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Each family's decoder, and a phrase only that document holds.
	want := map[string]struct {
		outcome runnerdecode.Outcome
		tool    string
		phrase  string
	}{
		"contact.html":   {runnerdecode.OutcomeText, "", ""},
		"depots.csv":     {runnerdecode.OutcomeText, "", ""},
		"runbook.md":     {runnerdecode.OutcomeText, "", ""},
		"incident.pdf":   {runnerdecode.OutcomeDecoded, "pdftotext", "fuel filter was clogged"},
		"policy.docx":    {runnerdecode.OutcomeDecoded, "pandoc", "45-minute break"},
		"handbook.epub":  {runnerdecode.OutcomeDecoded, "pandoc", "doors 7 to 12 for inbound"},
		"memo.rtf":       {runnerdecode.OutcomeDecoded, "pandoc", "Antwerp is ANR"},
		"deck.pptx":      {runnerdecode.OutcomeDecoded, "agentry-runner", "Cold-chain incidents: 3"},
		"prices.xlsx":    {runnerdecode.OutcomeDecoded, "agentry-runner", "Cold storage per week\t11.25"},
		"whiteboard.png": {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"photo.jpg":      {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"scan.tif":       {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"sign.webp":      {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
	}
	if len(res.Report.Files) != len(want) {
		t.Fatalf("%d files decoded, want %d: %+v", len(res.Report.Files), len(want), res.Report.Files)
	}
	for _, f := range res.Report.Files {
		w, ok := want[f.Path]
		if !ok {
			t.Errorf("unexpected %s", f.Path)
			continue
		}
		if f.Outcome != w.outcome || f.Tool != w.tool {
			t.Errorf("%s: outcome %s tool %q (%s), want %s %q", f.Path, f.Outcome, f.Tool, f.Reason, w.outcome, w.tool)
			continue
		}
		if w.phrase == "" {
			continue
		}
		dst := filepath.Join(t.TempDir(), "text")
		if err := res.CopyText(f.Path, dst); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		b, _ := os.ReadFile(dst)
		if !strings.Contains(string(b), w.phrase) {
			t.Errorf("%s: the text does not hold %q:\n%s", f.Path, w.phrase, b)
		}
	}
	t.Logf("receipt line: %s", res.Line())
}
