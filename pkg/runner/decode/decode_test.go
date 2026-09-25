package decode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

func TestImageSpecNamesTheCatalogueOnce(t *testing.T) {
	img := ImageSpec("agentry-runner:abc", "")
	if strings.Join(img.Commands, ",") != "pandoc,pdftotext,tesseract" {
		t.Fatalf("commands %v", img.Commands)
	}
	if strings.Join(img.Packages, ",") != "pandoc-cli,poppler-utils,tesseract-ocr,tesseract-ocr-data-eng" {
		t.Fatalf("packages %v", img.Packages)
	}
	if img.Tag == "" || img.Tag == ImageSpec("agentry-runner:def", "").Tag || img.Tag == ImageSpec("agentry-runner:abc", "https://mirror.example").Tag {
		t.Fatalf("the tag %q does not move with the runner image or the mirror", img.Tag)
	}
}

// playDecode is the container: it writes the report a real decode would,
// and a text for each file it says it decoded.
func playDecode(t *testing.T, files []runnerdecode.File) func(dockerx.RunSpec) {
	t.Helper()
	return func(r dockerx.RunSpec) {
		out := mountSource(r, "/out")
		for _, f := range files {
			if f.Outcome == runnerdecode.OutcomeDecoded && !strings.Contains(f.Output, "..") {
				if err := os.WriteFile(filepath.Join(out, filepath.FromSlash(f.Output)), []byte("text of "+f.Path+"\n"), 0o644); err != nil {
					t.Error(err)
				}
			}
		}
		b, _ := json.Marshal(runnerdecode.Report{Schema: runnerdecode.Schema, Files: files})
		if err := os.WriteFile(filepath.Join(out, runnerdecode.ReportFile), b, 0o644); err != nil {
			t.Error(err)
		}
	}
}

func mountSource(r dockerx.RunSpec, target string) string {
	for _, m := range r.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

// staged writes the sample documents into in, as the caller stages them.
func staged(t *testing.T) string {
	t.Helper()
	in := t.TempDir()
	for _, n := range []string{"blob.bin", "deck.pptx", "notes.md", "report.pdf"} {
		if err := os.WriteFile(filepath.Join(in, n), []byte("document "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(in, "link.pdf")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	return in
}

var sampleFiles = []runnerdecode.File{
	{Path: "blob.bin", Family: "unknown", Outcome: runnerdecode.OutcomeUndecodable, Reason: "not text"},
	{Path: "deck.pptx", Family: "presentation", Tool: "agentry-runner", Outcome: runnerdecode.OutcomeDecoded, Output: "deck.pptx.txt", Bytes: 40},
	{Path: "link.pdf", Family: "unknown", Outcome: runnerdecode.OutcomeRefused, Reason: "not a regular file"},
	{Path: "notes.md", Family: "text", Outcome: runnerdecode.OutcomeText},
	{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "report.pdf.txt", Bytes: 900},
}

var sampleImage = Image{
	Tag:      "agentry-exec:0123456789ab",
	Packages: []string{"pandoc-cli-3.6.4-r0", "poppler-utils-25.04.0-r0", "tesseract-ocr-5.5.0-r2", "tesseract-ocr-data-eng-5.5.0-r2"},
}

func TestRunDecodesInAContainerWithNothing(t *testing.T) {
	in, out := staged(t), t.TempDir()
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, sampleFiles)}}}
	res, err := Run(context.Background(), f, sampleImage, in, out, policy.Limits{}.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := f.FindRun("agentry-decode-")
	if !ok {
		t.Fatal("no decode container ran")
	}
	if r.Image != sampleImage.Tag || strings.Join(r.Argv, " ") != "decode --in /in --out /out" {
		t.Fatalf("image %q argv %v", r.Image, r.Argv)
	}
	if r.Network != "none" || !r.ReadOnly || !slices.Equal(r.Tmpfs, []string{"/tmp:size=512m,mode=1777"}) || !slices.Equal(r.CapDrop, []string{"ALL"}) || !r.NoNewPrivileges || !r.AutoRemove {
		t.Fatalf("posture: %+v", r)
	}
	if want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); os.Getuid() >= 0 && r.User != want {
		t.Fatalf("user %q, want %q", r.User, want)
	}
	wantMounts := []dockerx.Mount{{Source: in, Target: "/in", ReadOnly: true, Relabel: true}, {Source: out, Target: "/out", Relabel: true}}
	if !slices.Equal(r.Mounts, wantMounts) {
		t.Fatalf("mounts %+v", r.Mounts)
	}
	if res.Decoded() != 2 || res.Text() != 1 || len(res.Undecodable()) != 2 {
		t.Fatalf("counts %d %d %v", res.Decoded(), res.Text(), res.Undecodable())
	}
	want := "the inputs were decoded before the run, in agentry-exec:0123456789ab with no network, every capability dropped and the inputs read-only," +
		" by pandoc, pdftotext and tesseract (pandoc-cli-3.6.4-r0, poppler-utils-25.04.0-r0, tesseract-ocr-5.5.0-r2, tesseract-ocr-data-eng-5.5.0-r2)" +
		" and the runner's own PPTX and XLSX reader: 2 decoded, 1 read as text, 2 not decoded (blob.bin, link.pdf)"
	if got := res.Line(); got != want {
		t.Fatalf("line =\n%s\nwant\n%s", got, want)
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		name    string
		fg      func(t *testing.T) *dockerx.FakeForeground
		wantErr string
	}{
		{name: "the container fails", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{ExitCode: 1, Stderr: "agentry-runner: decode: boom\n"}
		}, wantErr: "exit 1"},
		{name: "no report", fg: func(t *testing.T) *dockerx.FakeForeground { return &dockerx.FakeForeground{} }, wantErr: "decode.json"},
		{name: "another report schema", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: func(r dockerx.RunSpec) {
				_ = os.WriteFile(filepath.Join(r.Mounts[1].Source, runnerdecode.ReportFile), []byte(`{"schema":"decode/v9","files":[]}`), 0o644)
			}}
		}, wantErr: "decode/v9"},
		{name: "a report naming an output outside the output root", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "../a.txt"}})}
		}, wantErr: "outside"},
		{name: "a report naming an output that is not the document's", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "notes.md.txt"}})}
		}, wantErr: "outside"},
		{name: "a report naming a document never staged", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "home/u/.ssh/id_rsa", Family: "text", Outcome: runnerdecode.OutcomeText}})}
		}, wantErr: "not staged"},
		{name: "a report naming a path that climbs out", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "../../etc/passwd", Family: "text", Outcome: runnerdecode.OutcomeText}})}
		}, wantErr: "not a document once"},
		{name: "a report of an unknown outcome", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "notes.md", Family: "text", Outcome: "trusted"}})}
		}, wantErr: "unknown outcome"},
		{name: "a text that is a link out of the output", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: func(r dockerx.RunSpec) {
				out := mountSource(r, "/out")
				_ = os.Symlink("/etc/hostname", filepath.Join(out, "report.pdf.txt"))
				b, _ := json.Marshal(runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "report.pdf.txt"}}})
				_ = os.WriteFile(filepath.Join(out, runnerdecode.ReportFile), b, 0o644)
			}}
		}, wantErr: "not a regular file"},
		{name: "a text reached through a directory link inside the output", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: func(r dockerx.RunSpec) {
				out := mountSource(r, "/out")
				_ = os.Symlink("/", filepath.Join(out, "sub"))
				b, _ := json.Marshal(runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{{Path: "report.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "sub/etc/hostname"}}})
				_ = os.WriteFile(filepath.Join(out, runnerdecode.ReportFile), b, 0o644)
			}}
		}, wantErr: "outside"},
		{name: "a report that is a link", fg: func(t *testing.T) *dockerx.FakeForeground {
			return &dockerx.FakeForeground{Before: func(r dockerx.RunSpec) {
				elsewhere := filepath.Join(t.TempDir(), "decode.json")
				_ = os.WriteFile(elsewhere, []byte(`{"schema":"decode/v1","files":[]}`), 0o644)
				_ = os.Symlink(elsewhere, filepath.Join(mountSource(r, "/out"), runnerdecode.ReportFile))
			}}
		}, wantErr: "not a regular file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": tc.fg(t)}}
			_, err := Run(context.Background(), f, sampleImage, staged(t), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestStage(t *testing.T) {
	in, out, dst := staged(t), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "report.pdf.txt"), []byte("report text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := &Result{In: in, Out: out, Report: runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{
		{Path: "blob.bin", Family: runnerdecode.FamilyUnknown, Outcome: runnerdecode.OutcomeUndecodable, Reason: "not text, and no decoder reads this kind of file"},
		{Path: "notes.md", Family: runnerdecode.FamilyText, Outcome: runnerdecode.OutcomeText},
		{Path: "report.pdf", Family: runnerdecode.FamilyPDF, Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "report.pdf.txt"},
	}}}
	docs, inputs, err := res.Stage(map[string]string{"report.pdf": "/given/Report.pdf"}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("docs %+v", docs)
	}
	var names []string
	for _, p := range inputs {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, ",") != "documents.json,notes.md,report.pdf.txt" {
		t.Fatalf("inputs %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "report.pdf.txt")); string(b) != "report text\n" {
		t.Fatalf("staged text %q", b)
	}
	raw, _ := os.ReadFile(filepath.Join(dst, DocumentsFile))
	want := `{
  "documents": [
    {
      "name": "blob.bin",
      "family": "unknown",
      "unread": "not text, and no decoder reads this kind of file"
    },
    {
      "name": "notes.md",
      "read": "notes.md",
      "family": "text"
    },
    {
      "name": "report.pdf",
      "givenAs": "/given/Report.pdf",
      "read": "report.pdf.txt",
      "family": "pdf",
      "decodedBy": "pdftotext"
    }
  ]
}
`
	if string(raw) != want {
		t.Fatalf("documents.json =\n%s", raw)
	}
	t.Run("a text document named as another's decoded text is refused", func(t *testing.T) {
		clash := &Result{In: in, Out: out, Report: runnerdecode.Report{Files: []runnerdecode.File{
			{Path: "report.pdf", Outcome: runnerdecode.OutcomeDecoded, Output: "report.pdf.txt"},
			{Path: "report.pdf.txt", Outcome: runnerdecode.OutcomeText},
		}}}
		if _, _, err := clash.Stage(nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "would both be read as report.pdf.txt") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a staged document that became a link is not copied", func(t *testing.T) {
		if err := os.Remove(filepath.Join(in, "notes.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/hostname", filepath.Join(in, "notes.md")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := res.Stage(nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestAStagedDocumentTheReportLeavesOutIsNotDecoded: a decoder that drops a
// document from its report cannot make it vanish from what a run is told.
func TestAStagedDocumentTheReportLeavesOutIsNotDecoded(t *testing.T) {
	in := staged(t)
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": {Before: playDecode(t, []runnerdecode.File{
		{Path: "notes.md", Family: runnerdecode.FamilyText, Outcome: runnerdecode.OutcomeText},
	})}}}
	res, err := Run(context.Background(), f, sampleImage, in, t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range res.Report.Files {
		got[file.Path] = string(file.Outcome) + ": " + file.Reason
	}
	for _, p := range []string{"blob.bin", "deck.pptx", "link.pdf", "report.pdf"} {
		if got[p] != "undecodable: the decode's report does not name it" {
			t.Errorf("%s: %q", p, got[p])
		}
	}
	if got["notes.md"] != "text: " {
		t.Errorf("notes.md: %q", got["notes.md"])
	}
}
