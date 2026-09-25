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

// playDecode is the container: it writes the report a real decode would.
func playDecode(t *testing.T, files []runnerdecode.File) func(dockerx.RunSpec) {
	t.Helper()
	return func(r dockerx.RunSpec) {
		var out string
		for _, m := range r.Mounts {
			if m.Target == "/out" {
				out = m.Source
			}
		}
		b, _ := json.Marshal(runnerdecode.Report{Schema: runnerdecode.Schema, Files: files})
		if err := os.WriteFile(filepath.Join(out, runnerdecode.ReportFile), b, 0o644); err != nil {
			t.Error(err)
		}
	}
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
	in, out := t.TempDir(), t.TempDir()
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
	if r.Network != "none" || !r.ReadOnly || !slices.Equal(r.Tmpfs, []string{"/tmp"}) || !slices.Equal(r.CapDrop, []string{"ALL"}) || !r.NoNewPrivileges || !r.AutoRemove {
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
		" by pdftotext, pandoc and tesseract (pandoc-cli-3.6.4-r0, poppler-utils-25.04.0-r0, tesseract-ocr-5.5.0-r2, tesseract-ocr-data-eng-5.5.0-r2)" +
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
			return &dockerx.FakeForeground{Before: playDecode(t, []runnerdecode.File{{Path: "a.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "../a.txt"}})}
		}, wantErr: "outside"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-decode-": tc.fg(t)}}
			_, err := Run(context.Background(), f, sampleImage, t.TempDir(), t.TempDir(), policy.Limits{}.WithDefaults(), os.Stderr)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
