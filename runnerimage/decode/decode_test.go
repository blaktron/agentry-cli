package decode

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTools writes shell stand-ins for the three decoders. Each writes a
// fixed line naming itself and its input's base name where the real tool
// writes its text, so a test sees which tool ran on which file and with
// which output path — the argv is the thing under test.
func fakeTools(t *testing.T) Tools {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the decoder runs in the Linux runner image")
	}
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return Tools{
		// pdftotext -layout -enc UTF-8 -q IN OUT
		PDFToText: script("pdftotext", `[ "$1 $2 $3 $4" = "-layout -enc UTF-8 -q" ] || exit 9; echo "pdftotext read $(basename "$5")" > "$6"`),
		// pandoc --sandbox --to=plain --wrap=none --output=OUT IN
		Pandoc: script("pandoc", `[ "$1 $2 $3" = "--sandbox --to=plain --wrap=none" ] || exit 9; echo "pandoc read $(basename "$5")" > "${4#--output=}"`),
		// tesseract IN OUTBASE -l eng  (writes OUTBASE.txt)
		Tesseract: script("tesseract", `[ "$3 $4" = "-l eng" ] || exit 9; echo "tesseract read $(basename "$1")" > "$2.txt"`),
	}
}

// ooxml builds a zip of the given entries.
func ooxml(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const aNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`

func pptx(t *testing.T) []byte {
	return ooxml(t, map[string]string{
		"ppt/slides/slide2.xml":  `<p:sld ` + aNS + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Second slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide1.xml":  `<p:sld ` + aNS + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Quarterly </a:t></a:r><a:r><a:t>review</a:t></a:r></a:p><a:p><a:r><a:t>Revenue up 4%</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide10.xml": `<p:sld ` + aNS + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Tenth</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"[Content_Types].xml":    `<Types/>`,
	})
}

func xlsx(t *testing.T) []byte {
	const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	return ooxml(t, map[string]string{
		"xl/sharedStrings.xml":     `<sst ` + ns + `><si><t>Region</t></si><si><t>Total</t></si><si><r><t>No</t></r><r><t>rth</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet ` + ns + `><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>1250.5</v></c></row><row r="3"><c r="A3" t="inlineStr"><is><t>South</t></is></c><c r="B3" t="b"><v>1</v></c></row></sheetData></worksheet>`,
	})
}

func TestDecodeFamilies(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	write(t, in, "notes.md", []byte("# Notes\n"))
	write(t, in, "data.csv", []byte("a,b\n1,2\n"))
	write(t, in, "config.unknownext", []byte("plain text with an unusual name\n"))
	write(t, in, "report.pdf", []byte("%PDF-1.7 not really"))
	write(t, in, "memo.docx", []byte("PK not really"))
	write(t, in, "book.EPUB", []byte("PK not really"))
	write(t, in, "scan.png", []byte("\x89PNG not really"))
	write(t, in, "deck.pptx", pptx(t))
	write(t, in, "sheet.xlsx", xlsx(t))
	write(t, in, "sub/photo.jpeg", []byte("\xff\xd8 not really"))
	write(t, in, "blob.bin", []byte{0x00, 0x01, 0xfe, 0xff})

	rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range rep.Files {
		got[f.Path] = f
	}
	want := map[string]struct {
		family       Family
		tool         string
		outcome      Outcome
		output, text string
	}{
		"notes.md":          {FamilyText, "", OutcomeText, "", ""},
		"data.csv":          {FamilyText, "", OutcomeText, "", ""},
		"config.unknownext": {FamilyText, "", OutcomeText, "", ""},
		"report.pdf":        {FamilyPDF, "pdftotext", OutcomeDecoded, "report.pdf.txt", "pdftotext read report.pdf\n"},
		"memo.docx":         {FamilyDocument, "pandoc", OutcomeDecoded, "memo.docx.txt", "pandoc read memo.docx\n"},
		"book.EPUB":         {FamilyDocument, "pandoc", OutcomeDecoded, "book.EPUB.txt", "pandoc read book.EPUB\n"},
		"scan.png":          {FamilyImage, "tesseract", OutcomeDecoded, "scan.png.txt", "tesseract read scan.png\n"},
		"sub/photo.jpeg":    {FamilyImage, "tesseract", OutcomeDecoded, "sub/photo.jpeg.txt", "tesseract read photo.jpeg\n"},
		"deck.pptx": {FamilyPresentation, "agentry-runner", OutcomeDecoded, "deck.pptx.txt",
			"Slide 1\nQuarterly review\nRevenue up 4%\n\nSlide 2\nSecond slide\n\nSlide 10\nTenth\n"},
		"sheet.xlsx": {FamilySpreadsheet, "agentry-runner", OutcomeDecoded, "sheet.xlsx.txt",
			"Sheet 1\nRegion\tTotal\nNorth\t1250.5\nSouth\tTRUE\n"},
		"blob.bin": {FamilyUnknown, "", OutcomeUndecodable, "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("report has %d files, want %d: %+v", len(got), len(want), rep.Files)
	}
	for path, w := range want {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s: not in the report", path)
			continue
		}
		if f.Family != w.family || f.Tool != w.tool || f.Outcome != w.outcome || f.Output != w.output {
			t.Errorf("%s: family %q tool %q outcome %q output %q, want %q %q %q %q (%s)", path, f.Family, f.Tool, f.Outcome, f.Output, w.family, w.tool, w.outcome, w.output, f.Reason)
		}
		if w.outcome != OutcomeDecoded {
			continue
		}
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(f.Output)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(b) != w.text || f.Bytes != int64(len(w.text)) {
			t.Errorf("%s: text %q (%d bytes recorded), want %q", path, b, f.Bytes, w.text)
		}
	}
	// The report is written beside the texts, for the caller outside the
	// container to read.
	raw, err := os.ReadFile(filepath.Join(out, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Report
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.Schema != Schema || len(onDisk.Files) != len(want) {
		t.Fatalf("report on disk: %v %+v", err, onDisk)
	}
}

func TestDecodeRefusals(t *testing.T) {
	t.Run("a link is refused, never followed", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		write(t, in, "ok.txt", []byte("fine\n"))
		if err := os.Symlink("/etc/hostname", filepath.Join(in, "host.pdf")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range rep.Files {
			if f.Path == "host.pdf" && (f.Outcome != OutcomeRefused || !strings.Contains(f.Reason, "not a regular file")) {
				t.Fatalf("link: %+v", f)
			}
		}
	})
	t.Run("a tool that fails is the file's outcome, not the run's", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		write(t, in, "bad.pdf", []byte("x"))
		tools := fakeTools(t)
		tools.PDFToText = filepath.Join(t.TempDir(), "missing")
		rep, err := Decode(context.Background(), in, out, tools, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if f := rep.Files[0]; f.Outcome != OutcomeUndecodable || !strings.Contains(f.Reason, "pdftotext") {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("text past the cap is cut and says so", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		write(t, in, "deck.pptx", pptx(t))
		rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{MaxOutputBytes: 12})
		if err != nil {
			t.Fatal(err)
		}
		f := rep.Files[0]
		b, _ := os.ReadFile(filepath.Join(out, "deck.pptx.txt"))
		if string(b) != "Slide 1\nQuar" || f.Bytes != 12 || !strings.Contains(f.Reason, "cut at 12 bytes") {
			t.Fatalf("%+v %q", f, b)
		}
	})
	t.Run("an office file that expands past its cap is refused", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		big := strings.Repeat("<a:t>aaaaaaaaaa</a:t>", 2000)
		write(t, in, "bomb.pptx", ooxml(t, map[string]string{"ppt/slides/slide1.xml": `<p:sld ` + aNS + `>` + big + `</p:sld>`}))
		rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{MaxExpandedBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		if f := rep.Files[0]; f.Outcome != OutcomeUndecodable || !strings.Contains(f.Reason, "expands past") {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("files past the cap are refused, not read", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		for _, n := range []string{"a.md", "b.md", "c.md"} {
			write(t, in, n, []byte("x\n"))
		}
		rep, err := Decode(context.Background(), in, out, fakeTools(t), Limits{MaxFiles: 2})
		if err != nil {
			t.Fatal(err)
		}
		if f := rep.Files[2]; f.Path != "c.md" || f.Outcome != OutcomeRefused || !strings.Contains(f.Reason, "2-file cap") {
			t.Fatalf("%+v", rep.Files)
		}
	})
	t.Run("a decoder's text is cut in scratch, and no more reaches the output", func(t *testing.T) {
		in, out := t.TempDir(), t.TempDir()
		write(t, in, "big.pdf", []byte("x"))
		tools := fakeTools(t)
		tools.PDFToText = filepath.Join(t.TempDir(), "pdftotext")
		if err := os.WriteFile(tools.PDFToText, []byte("#!/bin/sh\nhead -c 100000 /dev/zero | tr '\\0' 'a' > \"$6\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		scratch := t.TempDir()
		rep, err := Decode(context.Background(), in, out, tools, Limits{MaxOutputBytes: 1000, ScratchDir: scratch})
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(out, "big.pdf.txt"))
		if err != nil || fi.Size() != 1000 || !strings.Contains(rep.Files[0].Reason, "cut at 1000 bytes of 100000") {
			t.Fatalf("%v %+v %v", fi, rep.Files[0], err)
		}
		if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
			t.Fatalf("scratch left behind: %v", entries)
		}
	})
	t.Run("an input root that is missing is the run's error", func(t *testing.T) {
		if _, err := Decode(context.Background(), filepath.Join(t.TempDir(), "absent"), t.TempDir(), fakeTools(t), Limits{}); err == nil {
			t.Fatal("no error for a missing input root")
		}
	})
}
