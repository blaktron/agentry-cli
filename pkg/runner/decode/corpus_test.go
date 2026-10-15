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
	if err := os.CopyFS(in, os.DirFS("testdata/m3")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "NOTICE.md"} {
		if err := os.Remove(filepath.Join(in, name)); err != nil {
			t.Fatal(err)
		}
	}
	renamed, err := os.ReadFile(filepath.Join(in, "policy.docx"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "renamed.bin"), renamed, 0o600); err != nil {
		t.Fatal(err)
	}
	tiff, err := os.ReadFile(filepath.Join(in, "scan.tif"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "scan.tiff"), tiff, 0o600); err != nil {
		t.Fatal(err)
	}
	// Code, markup, configuration and data text, as a hosted run gives
	// them: each is read as uploaded, whatever libmagic
	// calls it.
	code := map[string]string{
		"index.html": "<!doctype html>\n<form action=\"/q\"><input name=\"q\"></form>\n<div id=\"out\"></div>\n<script>document.getElementById('out').innerHTML = location.hash.slice(1);</script>\n",
		"pom.xml":    "<?xml version=\"1.0\"?>\n<project><dependencies><dependency><groupId>org.apache.logging.log4j</groupId><artifactId>log4j-core</artifactId><version>2.14.1</version></dependency></dependencies></project>\n",
		"deploy.sh":  "#!/bin/sh\nset -eu\nscp build.tgz \"$HOST\":/srv/\n",
		"server.ts":  "import { exec } from 'child_process';\nexport const run = (dir: string) => exec(`ls ${dir}`);\n",
		"logo.svg":   "<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script><circle r=\"4\"/></svg>\n",
		"login.php":  "<?php $r = mysqli_query($db, \"SELECT * FROM users WHERE id = \" . $_GET['id']); ?>\n",
		"Dockerfile": "FROM alpine:3.20\nENV API_KEY=sk_live_not_a_real_key\nRUN apk add curl\n",
		"Makefile":   "build:\n\tgo build ./...\n",
		"k8s.yaml":   "apiVersion: v1\nkind: Pod\nspec:\n  containers:\n  - name: app\n    securityContext: {privileged: true}\n",
	}
	isCode := map[string]bool{}
	for name, body := range code {
		isCode[name] = true // code is emptied as each file is checked
		if err := os.WriteFile(filepath.Join(in, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Text that is not UTF-8 is read by Tika, which guesses its charset.
	if err := os.WriteFile(filepath.Join(in, "latin1.csv"), []byte("depot,city\nANR,Li\xe8ge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(ctx, d, img, in, out, pol.Docker.Limits.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Report.Files {
		body, ok := code[f.Path]
		if !ok {
			continue
		}
		// HTML also gets pandoc's Markdown as its text view.
		wantBy := ""
		if f.Path == "index.html" {
			wantBy = "pandoc"
			md := filepath.Join(t.TempDir(), "view.md")
			if err := res.CopyMarkdown(f.Path, md); err != nil {
				t.Fatalf("%s: %v", f.Path, err)
			}
			if b, _ := os.ReadFile(md); !strings.Contains(string(b), `<div id="out">`) || strings.Contains(string(b), "<script>") {
				t.Errorf("%s: the Markdown view %q", f.Path, b)
			}
		}
		if f.Family != runnerdecode.FamilyText || f.Outcome != runnerdecode.OutcomeText || f.Tool != "" || f.MarkdownBy != wantBy {
			t.Errorf("%s (%s): %+v, want text read as uploaded", f.Path, f.MIME, f)
		}
		dst := filepath.Join(t.TempDir(), "original")
		if err := res.CopyOriginal(f.Path, dst); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if b, _ := os.ReadFile(dst); string(b) != body {
			t.Errorf("%s: served %q, want %q", f.Path, b, body)
		}
		t.Logf("%s: libmagic %s, read as uploaded", f.Path, f.MIME)
		delete(code, f.Path)
	}
	if len(code) != 0 {
		t.Errorf("not in the report: %v", code)
	}
	// Each family's decoder, and a phrase only that document holds.
	want := map[string]struct {
		outcome runnerdecode.Outcome
		tool    string
		phrase  string
	}{
		"testWORD.doc":                   {runnerdecode.OutcomeDecoded, "tika", "Sample"},
		"testEXCEL.xls":                  {runnerdecode.OutcomeDecoded, "tika", "Sample"},
		"testPPT.ppt":                    {runnerdecode.OutcomeDecoded, "tika", "Sample"},
		"test-outlook.msg":               {runnerdecode.OutcomeDecoded, "tika", "Microsoft Outlook Express 6"},
		"testOpenOffice2.odt":            {runnerdecode.OutcomeDecoded, "pandoc", ""},
		"LibreOfficeCalc_ods_1.3.ods":    {runnerdecode.OutcomeDecoded, "agentry-runner", "|"},
		"LibreOfficeImpress_odp_1.3.odp": {runnerdecode.OutcomeDecoded, "agentry-runner", "## Slide"},
		"mail.eml":                       {runnerdecode.OutcomeDecoded, "tika", "M3 mail content is readable."},
		"table.docx":                     {runnerdecode.OutcomeDecoded, "pandoc", "Antwerp"},
		"data.json":                      {runnerdecode.OutcomeText, "", ""},
		"unread.bin":                     {runnerdecode.OutcomeUndecodable, "", ""},
		"renamed.bin":                    {runnerdecode.OutcomeDecoded, "pandoc", "45-minute break"},
		"contact.html":                   {runnerdecode.OutcomeText, "", ""},
		"depots.csv":                     {runnerdecode.OutcomeText, "", ""},
		"runbook.md":                     {runnerdecode.OutcomeText, "", ""},
		"latin1.csv":                     {runnerdecode.OutcomeDecoded, "tika", "Liège"},
		"incident.pdf":                   {runnerdecode.OutcomeDecoded, "pdftotext", "fuel filter was clogged"},
		"policy.docx":                    {runnerdecode.OutcomeDecoded, "pandoc", "45-minute break"},
		"handbook.epub":                  {runnerdecode.OutcomeDecoded, "pandoc", "doors 7 to 12 for inbound"},
		"memo.rtf":                       {runnerdecode.OutcomeDecoded, "pandoc", "Antwerp is ANR"},
		"deck.pptx":                      {runnerdecode.OutcomeDecoded, "agentry-runner", "Cold-chain incidents: 3"},
		"prices.xlsx":                    {runnerdecode.OutcomeDecoded, "agentry-runner", "Cold storage per week | 11.25"},
		"whiteboard.png":                 {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"photo.jpg":                      {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"scan.tiff":                      {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"scan.tif":                       {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
		"sign.webp":                      {runnerdecode.OutcomeDecoded, "tesseract", "FOR ALARMED REEFERS"},
	}
	if len(res.Report.Files) != len(want)+len(isCode) {
		t.Fatalf("%d files decoded, want %d: %+v", len(res.Report.Files), len(want)+len(isCode), res.Report.Files)
	}
	for _, f := range res.Report.Files {
		w, ok := want[f.Path]
		if !ok {
			if !isCode[f.Path] {
				t.Errorf("unexpected %s", f.Path)
			}
			continue
		}
		if f.Outcome != w.outcome || f.Tool != w.tool {
			t.Errorf("%s: outcome %s tool %q (%s), want %s %q", f.Path, f.Outcome, f.Tool, f.Reason, w.outcome, w.tool)
			continue
		}
		if f.Outcome != runnerdecode.OutcomeDecoded {
			continue
		}
		dst := filepath.Join(t.TempDir(), "text")
		if err := res.CopyText(f.Path, dst); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		b, _ := os.ReadFile(dst)
		if len(b) == 0 || !strings.Contains(string(b), w.phrase) {
			t.Errorf("%s: the text does not hold %q:\n%s", f.Path, w.phrase, b)
		}
		if f.Path == "table.docx" && (!strings.Contains(string(b), "| Depot") || !strings.Contains(string(b), "---")) {
			t.Errorf("DOCX table is not Markdown: %s", b)
		}
		t.Logf("%s: %s %s: %.160s", f.Path, f.Family, f.Tool, b)
	}
	t.Logf("receipt line: %s", res.Line())
}
