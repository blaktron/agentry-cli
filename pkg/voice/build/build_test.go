package build

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

const written = "# Your voice\n\nWrite in short sentences. Put the verb early.\n"

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mountOf(r dockerx.RunSpec, target string) string {
	for _, m := range r.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

// playDecode is the decode container: every .pdf becomes its text, every
// .md passes as text, anything else is undecodable.
func playDecode(t *testing.T) func(dockerx.RunSpec) {
	return func(r dockerx.RunSpec) {
		in, out := mountOf(r, "/in"), mountOf(r, "/out")
		rep := runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{}}
		_ = filepath.WalkDir(in, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(in, p)
			rel = filepath.ToSlash(rel)
			switch filepath.Ext(rel) {
			case ".pdf":
				writeFile(t, filepath.Join(out, rel+".txt"), "text of "+rel+"\n")
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: rel + ".txt", Bytes: 20})
			case ".md":
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "text", Outcome: runnerdecode.OutcomeText})
			default:
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "unknown", Outcome: runnerdecode.OutcomeUndecodable, Reason: "not text"})
			}
			return nil
		})
		b, _ := json.Marshal(rep)
		writeFile(t, filepath.Join(out, runnerdecode.ReportFile), string(b))
	}
}

type fixture struct {
	opts    Options
	fake    *dockerx.Fake
	samples string
	// current is the current voice the Voice Writer found among its
	// inputs.
	current string
}

const finalMessage = `{"type":"agent_choice","message":{"content":"wrote VOICE.md"}}` + "\n" + `{"type":"stream_stopped","usage":{"input_tokens":900,"output_tokens":300}}` + "\n"

func newFixture(t *testing.T, job Job, voiceMD string) *fixture {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Model.Name = "test-model"
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	fx := &fixture{samples: t.TempDir()}
	agent := &dockerx.FakeForeground{Stdout: finalMessage}
	fx.fake = &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-decode-": {Before: playDecode(t)},
			"-probe":          {ExitCode: 0},
			"-agent":          agent,
		},
		ImageFiles: map[string]string{
			decode.ImageSpec(runnerTag, policy.DefaultFilesImage).Tag + ":" + decode.FilesPackagesFile: "pandoc-cli-3.6.4-r0\npoppler-utils-25.04.0-r0\n",
		},
	}
	writeFile(t, filepath.Join(fx.samples, "my essay.pdf"), "%PDF bytes")
	writeFile(t, filepath.Join(fx.samples, "post.md"), "# A post\n\nShort. Direct.\n")
	writeFile(t, filepath.Join(fx.samples, "blob.bin"), "\x00\x01")
	fx.opts = Options{
		Job:  job,
		Home: t.TempDir(), Policy: pol, PolicySHA: "sha", Build: "agentry 9.9.9 (test)",
		OutDir: t.TempDir(), Stderr: os.Stderr, RunID: "cafe0003",
	}
	if job != Edit {
		fx.opts.Samples = []string{filepath.Join(fx.samples, "my essay.pdf"), filepath.Join(fx.samples, "post.md"), filepath.Join(fx.samples, "blob.bin")}
	}
	agent.Before = func(dockerx.RunSpec) {
		// The run's task and inputs, read where the engine staged them.
		_ = filepath.WalkDir(fx.opts.Home, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil //nolint:nilerr // a walk over a live run directory
			}
			switch {
			case strings.HasSuffix(p, filepath.Join("inputs", "current", "VOICE.md")):
				b, _ := os.ReadFile(p)
				fx.current = string(b)
			}
			return nil
		})
		if voiceMD != "" {
			writeFile(t, filepath.Join(fx.opts.OutDir, "reports", "VOICE.md"), voiceMD)
		}
	}
	return fx
}

func TestCreateWritesAVoiceFromSamples(t *testing.T) {
	fx := newFixture(t, Create, written)
	fx.opts.Brief = "Warmer than my emails."
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(res.Text) != written {
		t.Fatalf("text %q", res.Text)
	}
	if strings.Join(res.Samples, ",") != "my-essay.pdf,post.md" || strings.Join(res.Skipped, ",") != "blob.bin" {
		t.Fatalf("samples %v skipped %v", res.Samples, res.Skipped)
	}
	env := res.Envelope
	if env.Attestation.State != "builtin" || env.Ref != "agentry/voice-writer@1.1.0" {
		t.Fatalf("attestation %+v ref %s", env.Attestation, env.Ref)
	}
	for _, line := range []string{"voice", "knowledge"} {
		if _, ok := env.CapabilitiesEnforced[line]; ok {
			t.Fatalf("the Voice Writer's own run has a %s line", line)
		}
	}
	if !strings.Contains(env.CapabilitiesEnforced["decode"], "1 decoded, 1 read as text, 1 not decoded (blob.bin)") {
		t.Fatalf("decode line %q", env.CapabilitiesEnforced["decode"])
	}
	dec, ok := fx.fake.FindRun("agentry-decode-")
	if !ok {
		t.Fatal("no decode container ran")
	}
	for _, m := range dec.Mounts {
		if !strings.HasPrefix(m.Source, filepath.Join(fx.opts.Home, "work")+string(filepath.Separator)) {
			t.Errorf("the decode container binds %s, outside the runner home's work directory", m.Source)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(fx.opts.Home, "work")); len(entries) != 0 {
		t.Errorf("the build left its work, the samples in the clear, behind: %v", entries)
	}
	if fx.current != "" {
		t.Errorf("a Create was given a current voice: %q", fx.current)
	}
	task, err := os.ReadFile(filepath.Join(fx.opts.OutDir, "receipts", "task.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Job: Create", "- my-essay.pdf", "- post.md", "The person's brief", "Warmer than my emails."} {
		if !strings.Contains(string(task), want) {
			t.Errorf("the task does not say %q:\n%s", want, task)
		}
	}
}

func TestImproveAndEditReadTheCurrentVoice(t *testing.T) {
	const current = "# Your voice\n\nWrite plainly.\n"
	t.Run("improve", func(t *testing.T) {
		fx := newFixture(t, Improve, written)
		fx.opts.Current = []byte(current)
		if _, err := Build(context.Background(), fx.fake, fx.opts); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if fx.current != current {
			t.Fatalf("the run's current/VOICE.md = %q", fx.current)
		}
	})
	t.Run("edit", func(t *testing.T) {
		fx := newFixture(t, Edit, written)
		fx.opts.Current, fx.opts.Brief = []byte(current), "Fewer exclamation marks."
		res, err := Build(context.Background(), fx.fake, fx.opts)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if fx.current != current || string(res.Text) != written {
			t.Fatalf("current %q text %q", fx.current, res.Text)
		}
		if _, ok := fx.fake.FindRun("agentry-decode-"); ok {
			t.Fatal("an edit, which has no samples, ran the decode container")
		}
		if res.Decoded != nil {
			t.Fatal("an edit has a decode result")
		}
	})
}

// An AI Edit takes samples beside its note (plan voice-page-redesign.md
// D3, agentry-cli#605): they are decoded as Create's are, and the task
// keeps the note as the change to make, not as a brief.
func TestAnEditWithSamplesDecodesThemAndKeepsTheNoteAsTheChange(t *testing.T) {
	const current = "# Your voice\n\nWrite plainly.\n"
	fx := newFixture(t, Edit, written)
	fx.opts.Current, fx.opts.Brief = []byte(current), "Sound more like these newer posts."
	fx.opts.Samples = []string{filepath.Join(fx.samples, "post.md"), filepath.Join(fx.samples, "blob.bin")}
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if fx.current != current || string(res.Text) != written {
		t.Fatalf("current %q text %q", fx.current, res.Text)
	}
	if _, ok := fx.fake.FindRun("agentry-decode-"); !ok {
		t.Fatal("an edit with samples ran no decode container")
	}
	if res.Decoded == nil || strings.Join(res.Samples, ",") != "post.md" || strings.Join(res.Skipped, ",") != "blob.bin" {
		t.Fatalf("decoded %v samples %v skipped %v", res.Decoded != nil, res.Samples, res.Skipped)
	}
	if entries, _ := os.ReadDir(filepath.Join(fx.opts.Home, "work")); len(entries) != 0 {
		t.Errorf("the build left its work, the samples in the clear, behind: %v", entries)
	}
	task, err := os.ReadFile(filepath.Join(fx.opts.OutDir, "receipts", "task.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Job: Edit", "- post.md", "The person's note (the change to make", "Sound more like these newer posts."} {
		if !strings.Contains(string(task), want) {
			t.Errorf("the task does not say %q:\n%s", want, task)
		}
	}
	for _, never := range []string{"There are no samples", "The person's brief"} {
		if strings.Contains(string(task), never) {
			t.Errorf("an edit with samples says %q:\n%s", never, task)
		}
	}
}

// An Edit's samples are held to the decoded-text cap as Create's are: a
// hosted Edit's 1 MB (D9) cuts them in order, and the task says which.
func TestAnEditsSamplesAreCutAtTheTextCap(t *testing.T) {
	fx := newFixture(t, Edit, written)
	fx.opts.Current, fx.opts.Brief = []byte("# Your voice\n\nWrite plainly.\n"), "Shorter."
	fx.opts.Samples = []string{filepath.Join(fx.samples, "post.md")}
	fx.opts.MaxTextBytes = 10
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Join(res.Cut, ",") != "post.md" {
		t.Fatalf("cut %v", res.Cut)
	}
	task, err := os.ReadFile(filepath.Join(fx.opts.OutDir, "receipts", "task.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(task), "capped at 10 bytes") {
		t.Fatalf("the task does not say the samples were capped:\n%s", task)
	}
}

func TestAVoiceThatBreaksTheFormatIsNotReturned(t *testing.T) {
	for name, md := range map[string]string{
		"none written": "",
		"past the cap": strings.Repeat("a", voice.MaxBytes+1),
		"only space":   "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, Create, md)
			res, err := Build(context.Background(), fx.fake, fx.opts)
			var ce *engine.ClassifiedError
			if !errors.As(err, &ce) || ce.Code != engine.ExitHarness {
				t.Fatalf("err = %v, want the harness class", err)
			}
			if res != nil && res.Text != nil {
				t.Fatalf("returned %d bytes", len(res.Text))
			}
		})
	}
	t.Run("a link is never followed", func(t *testing.T) {
		fx := newFixture(t, Create, "")
		secret := filepath.Join(t.TempDir(), "secret")
		writeFile(t, secret, "# not the voice\n")
		before := fx.fake.Foreground["-agent"].Before
		fx.fake.Foreground["-agent"].Before = func(r dockerx.RunSpec) {
			before(r)
			_ = os.MkdirAll(filepath.Join(fx.opts.OutDir, "reports"), 0o755)
			_ = os.Symlink(secret, filepath.Join(fx.opts.OutDir, "reports", "VOICE.md"))
		}
		if _, err := Build(context.Background(), fx.fake, fx.opts); err == nil {
			t.Fatal("a linked VOICE.md was read")
		}
	})
}

func TestTheJobsTakeWhatTheyNeed(t *testing.T) {
	cur := []byte("# v\n\nPlain.\n")
	for _, tc := range []struct {
		name string
		o    Options
		want string
	}{
		{"create without samples", Options{Job: Create}, "samples"},
		{"create over a voice", Options{Job: Create, Samples: []string{"a"}, Current: cur}, "no current voice"},
		{"improve without a voice", Options{Job: Improve, Samples: []string{"a"}}, "no voice to improve"},
		{"improve without samples", Options{Job: Improve, Current: cur}, "more samples"},
		{"edit without a note", Options{Job: Edit, Current: cur}, "--note"},
		{"edit without a voice", Options{Job: Edit, Brief: "x"}, "no voice to edit"},
		{"a brief past its cap", Options{Job: Edit, Current: cur, Brief: strings.Repeat("x", MaxBriefBytes+1)}, "at most"},
		{"an unknown job", Options{Job: "rewrite"}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(context.Background(), &dockerx.Fake{}, tc.o)
			var ce *engine.ClassifiedError
			if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a usage error saying %q", err, tc.want)
			}
		})
	}
}

func TestCapTextCutsInOrderAtARuneBoundary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "12345")
	writeFile(t, filepath.Join(dir, "b.md"), "héllo")
	writeFile(t, filepath.Join(dir, "c.md"), "later")
	docs := []decode.Document{{Name: "a.md", Read: "a.md"}, {Name: "b.md", Read: "b.md"}, {Name: "c.md", Read: "c.md"}}
	cut, err := capText(docs, dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cut, ",") != "b.md,c.md" {
		t.Fatalf("cut %v", cut)
	}
	for name, want := range map[string]string{"a.md": "12345", "b.md": "h", "c.md": ""} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
