package build

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
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

// The signatures a build is given: a stand-in bundle, never the registry.
func init() {
	// No lookup leaves the machine, whatever the shell holds.
	hybridClient = func(*policy.Policy) (*hybrid.Client, error) { return &hybrid.Client{}, nil }
	ensureSignatures = func(_ context.Context, home, _ string, _ io.Writer) clamdb.DB {
		return clamdb.DB{Dir: filepath.Join(home, "clamav", "db", "84cbc016d58b4b70"), Ref: policy.DefaultClamAVSignatures, Digest: "sha256:84cbc016d58b4b70b7337b78cfd01929d6ad2646ac5d773be66efab8b3e2d10c", Daily: clamdb.Version{Number: 28143}}
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

// playArchive is the decode container unpacking a zip, as a decode asked to
// keep members does (keepMembers): the archive unpacked, each .pdf member
// decoded and each .md member read as text, both with their own bytes kept
// under originals/ when asked, anything else undecodable. Every entry is checked clean.
func playArchive(t *testing.T, src, rel, out string, keepMembers bool) []runnerdecode.File {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	whole, _ := os.ReadFile(src) //nolint:gosec // the test's own staged file
	files := []runnerdecode.File{{Path: rel, Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked,
		Check: runnerdecode.CheckClean, MIME: "application/zip", SHA256: sha256Hex(whole)}}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		path := rel + "/" + zf.Name
		f := runnerdecode.File{Path: path, In: rel, Size: int64(len(body)), Check: runnerdecode.CheckClean, SHA256: sha256Hex(body), MIME: "text/plain"}
		keep := func() {
			if !keepMembers {
				return
			}
			f.Original = runnerdecode.OriginalsDir + "/" + path
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Original)), string(body))
		}
		switch filepath.Ext(zf.Name) {
		case ".pdf":
			f.Family, f.Tool, f.Outcome, f.Output, f.MIME = "pdf", "pdftotext", runnerdecode.OutcomeDecoded, path+".txt", "application/pdf"
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Output)), "text of "+path+"\n")
			f.Bytes = int64(len("text of " + path + "\n"))
			keep()
		case ".md":
			f.Family, f.Outcome, f.Output = "text", runnerdecode.OutcomeText, path+".txt"
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Output)), string(body))
			f.Bytes = int64(len(body))
			keep()
		default:
			f.Family, f.Outcome, f.Reason, f.MIME = "unknown", runnerdecode.OutcomeUndecodable, "not text", "application/octet-stream"
		}
		files = append(files, f)
	}
	return files
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// printScan is the scan record the decode container prints before it
// decodes, of the report playDecode wrote.
func printScan(r dockerx.RunSpec) string {
	var rep runnerdecode.Report
	b, _ := os.ReadFile(filepath.Join(mountOf(r, "/out"), runnerdecode.ReportFile))
	_ = json.Unmarshal(b, &rep)
	return string(runnerdecode.RecordOf(rep.Files, mountOf(r, "/clamav") != ""))
}

// playDecode is the decode container: every .pdf becomes its text, every
// .md passes as text, a .com is malware ClamAV finds, anything else is
// undecodable. Each file is clean when the signatures are mounted, and
// unchecked for want of them otherwise.
func playDecode(t *testing.T) func(dockerx.RunSpec) {
	return func(r dockerx.RunSpec) {
		in, out := mountOf(r, "/in"), mountOf(r, "/out")
		check := func(f runnerdecode.File) runnerdecode.File {
			switch {
			case filepath.Ext(f.Path) == ".com":
				f.Family, f.Tool, f.Outcome, f.Output, f.Bytes, f.Reason = "unknown", "", runnerdecode.OutcomeRefused, "", 0, ""
				f.Why, f.Check, f.Signature = runnerdecode.WhyFound, runnerdecode.CheckFound, "Eicar-Test-Signature"
			case mountOf(r, "/clamav") != "":
				f.Check = runnerdecode.CheckClean
			default:
				f.Check, f.Unchecked = runnerdecode.CheckUnchecked, runnerdecode.UncheckedNoSignatures
			}
			return f
		}
		rep := runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{}}
		_ = filepath.WalkDir(in, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(in, p)
			rel = filepath.ToSlash(rel)
			switch filepath.Ext(rel) {
			case ".zip":
				rep.Files = append(rep.Files, playArchive(t, p, rel, out, slices.Contains(r.Argv, "--keep-members"))...)
				return nil
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
		for i := range rep.Files {
			if rep.Files[i].Check == "" {
				rep.Files[i] = check(rep.Files[i])
			}
		}
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
			"agentry-decode-": {Before: playDecode(t), Print: printScan},
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

// TestACreateReadsTheSamplesInAnArchive: given an archive of samples, the
// Voice Writer reads each sample it holds by its own name, and the archive
// is not a sample of its own.
func TestACreateReadsTheSamplesInAnArchive(t *testing.T) {
	fx := newFixture(t, Create, written)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"essays/one.pdf": "%PDF one", "posts/two.md": "# Two\n\nShort.\n"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	writeFile(t, filepath.Join(fx.samples, "samples.zip"), buf.String())
	fx.opts.Samples = []string{filepath.Join(fx.samples, "samples.zip")}
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Join(res.Samples, ",") != "samples.zip/essays/one.pdf,samples.zip/posts/two.md" || len(res.Skipped) != 0 {
		t.Fatalf("samples %v skipped %v", res.Samples, res.Skipped)
	}
	// It keeps no sample, so the decode keeps no member's own bytes.
	if strings.Contains(strings.Join(mustRun(t, fx.fake).Argv, " "), "--keep-members") {
		t.Fatal("the decode was asked to keep members")
	}
}

func mustRun(t *testing.T, f *dockerx.Fake) dockerx.RunSpec {
	t.Helper()
	r, ok := f.FindRun("agentry-decode-")
	if !ok {
		t.Fatal("no decode container ran")
	}
	return r
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
	if env.Attestation.State != "builtin" || env.Ref != "agentry/voice-writer@1.2.0" {
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
		// The signatures are the runner home's own bundle, read-only.
		if m.Target == "/clamav" && m.ReadOnly && strings.HasPrefix(m.Source, filepath.Join(fx.opts.Home, "clamav")+string(filepath.Separator)) {
			continue
		}
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

// An AI Edit takes samples beside its note: they are decoded as Create's are,
// and the task keeps the note as the change to make, not as a brief.
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
// hosted Edit's 1 MB cuts them in order, and the task says which.
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

// A sample ClamAV finds refuses the build before the Voice Writer starts,
// with the policy class, naming it.
func TestASampleClamAVFindsRefusesTheBuild(t *testing.T) {
	fx := newFixture(t, Create, written)
	writeFile(t, filepath.Join(fx.samples, "eicar.com"), "X5O EICAR")
	fx.opts.Samples = []string{filepath.Join(fx.samples, "post.md"), filepath.Join(fx.samples, "eicar.com")}
	_, err := Build(context.Background(), fx.fake, fx.opts)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "ClamAV found malware in eicar.com (Eicar-Test-Signature)") {
		t.Fatalf("err %v", err)
	}
	if _, ok := fx.fake.FindRun("-agent"); ok {
		t.Fatal("the Voice Writer ran on a sample ClamAV found")
	}
}

// maliciousHA stands for Hybrid Analysis with every hash malicious, for
// this test alone.
func maliciousHA(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"verdict":"malicious","threat_score":100}`)
	}))
	t.Cleanup(srv.Close)
	saved := hybridClient
	t.Cleanup(func() { hybridClient = saved })
	hybridClient = func(*policy.Policy) (*hybrid.Client, error) {
		return &hybrid.Client{Key: "ha-test-key", Base: srv.URL, HTTP: srv.Client()}, nil
	}
}

// A risky sample Hybrid Analysis calls malicious refuses the build with
// the policy class, before the Voice Writer starts.
func TestASampleHybridAnalysisCallsMaliciousRefusesTheBuild(t *testing.T) {
	maliciousHA(t)
	fx := newFixture(t, Create, written)
	writeFile(t, filepath.Join(fx.samples, "letter.pdf"), "%PDF not really")
	fx.opts.Samples = []string{filepath.Join(fx.samples, "post.md"), filepath.Join(fx.samples, "letter.pdf")}
	_, err := Build(context.Background(), fx.fake, fx.opts)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "Hybrid Analysis's verdict on letter.pdf is malicious (threat score 100)") {
		t.Fatalf("err %v", err)
	}
	if _, ok := fx.fake.FindRun("-agent"); ok {
		t.Fatal("the Voice Writer ran on a sample Hybrid Analysis called malicious")
	}
}
