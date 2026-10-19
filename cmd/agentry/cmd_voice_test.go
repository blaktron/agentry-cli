package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/pkg/voice/build"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

const (
	firstVoice  = "# Your voice\n\nWrite in short sentences.\n"
	secondVoice = "# Your voice\n\nWrite in short sentences. Never a semicolon.\n"
)

// fakeVoiceBuild stands in for the Voice Writer: it records the options it
// was given and answers text.
func fakeVoiceBuild(t *testing.T, text string) *build.Options {
	t.Helper()
	got := &build.Options{}
	prev := voiceBuild
	t.Cleanup(func() { voiceBuild = prev })
	voiceBuild = func(_ context.Context, _ dockerx.Client, o build.Options) (*build.Result, error) {
		*got = o
		return &build.Result{Text: []byte(text), Samples: []string{"post.md"}}, nil
	}
	return got
}

func TestVoiceCommandsKeepOneVoiceOnThisComputer(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	signOut(t)
	vc := func(args ...string) (string, string, error) {
		return runCLI(t, strings.NewReader(""), append([]string{"voice"}, args...)...)
	}
	sample := filepath.Join(t.TempDir(), "post.md")
	if err := os.WriteFile(sample, []byte("Short. Direct.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := vc("show"); !errors.Is(err, voice.ErrNoVoice) {
		t.Fatalf("show with no voice: %v", err)
	}
	got := fakeVoiceBuild(t, firstVoice)
	out, _, err := vc("create", "--file", sample, "--brief", "warmer")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Job != build.Create || got.Brief != "warmer" || got.Current != nil || len(got.Samples) != 1 {
		t.Fatalf("create ran the Voice Writer with %+v", got)
	}
	if !strings.Contains(out, "Your voice is on for every run.") {
		t.Fatalf("the first voice did not say it is on:\n%s", out)
	}
	if text, _, err := vc("show"); err != nil || text != firstVoice {
		t.Fatalf("show = %q, %v", text, err)
	}

	got = fakeVoiceBuild(t, secondVoice)
	out, _, err = vc("edit", "--note", "no semicolons")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got.Job != build.Edit || string(got.Current) != firstVoice || got.Brief != "no semicolons" {
		t.Fatalf("edit ran the Voice Writer with %+v", got)
	}
	if strings.Contains(out, "is on for every run") {
		t.Fatalf("a second version said again that the voice is on:\n%s", out)
	}
	meta, _, err := vc("show", "--json")
	if err != nil || !strings.Contains(meta, `"source": "ai-edit"`) || !strings.Contains(meta, `"alwaysAttach": true`) {
		t.Fatalf("show --json = %s, %v", meta, err)
	}

	if _, _, err := vc("attach", "off"); err != nil {
		t.Fatal(err)
	}
	hand := filepath.Join(t.TempDir(), "VOICE.md")
	if err := os.WriteFile(hand, []byte(firstVoice), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vc("set", hand); err != nil {
		t.Fatal(err)
	}
	meta, _, _ = vc("show", "--json")
	if !strings.Contains(meta, `"source": "edit"`) || !strings.Contains(meta, `"alwaysAttach": false`) {
		t.Fatalf("after set, show --json = %s", meta)
	}
	if _, _, err := vc("delete"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vc("show"); !errors.Is(err, voice.ErrNoVoice) {
		t.Fatalf("show after delete: %v", err)
	}
}

func TestVoiceWriteLeavesTheStoreAlone(t *testing.T) {
	isolateEnv(t)
	home, _ := newInstall(t)
	signOut(t)
	sample := filepath.Join(t.TempDir(), "post.md")
	if err := os.WriteFile(sample, []byte("Short.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(t.TempDir(), "current.md")
	if err := os.WriteFile(current, []byte(firstVoice), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fakeVoiceBuild(t, secondVoice)
	dst := filepath.Join(t.TempDir(), "VOICE.md")
	if _, _, err := runCLI(t, strings.NewReader(""), "voice", "improve", "--file", sample, "--current", current, "--write", dst, "--max-text-bytes", "1000000"); err != nil {
		t.Fatalf("improve --write: %v", err)
	}
	if string(got.Current) != firstVoice || got.MaxTextBytes != 1000000 {
		t.Fatalf("improve ran with %+v", got)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != secondVoice {
		t.Fatalf("--write wrote %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, "voice")); !os.IsNotExist(err) {
		t.Fatalf("--write kept a voice on this computer: %v", err)
	}
}

// An AI Edit takes samples beside its note, as a sandbox host drives it.
func TestAnEditTakesSamplesBesideItsNote(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	signOut(t)
	sample := filepath.Join(t.TempDir(), "post.md")
	if err := os.WriteFile(sample, []byte("Short.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(t.TempDir(), "current.md")
	if err := os.WriteFile(current, []byte(firstVoice), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fakeVoiceBuild(t, secondVoice)
	dst := filepath.Join(t.TempDir(), "VOICE.md")
	if _, _, err := runCLI(t, strings.NewReader(""), "voice", "edit", "--note", "more like this", "--file", sample, "--current", current, "--write", dst, "--max-text-bytes", "1000000"); err != nil {
		t.Fatalf("edit --file: %v", err)
	}
	if got.Job != build.Edit || got.Brief != "more like this" || len(got.Samples) != 1 || string(got.Current) != firstVoice || got.MaxTextBytes != 1000000 {
		t.Fatalf("edit ran with %+v", got)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "voice", "edit", "--note", "x", "--brief", "y", "--current", current, "--write", dst); err == nil {
		t.Fatal("edit took a --brief beside its note")
	}
}

func TestARunAttachesTheVoiceByItsFlag(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	signOut(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetErr(&strings.Builder{})

	if v, err := attachVoice(cmd, runOpts{}); err != nil || v != nil {
		t.Fatalf("no voice kept: %v %v", v, err)
	}
	var ce *engine.ClassifiedError
	if _, err := attachVoice(cmd, runOpts{voice: true}); !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
		t.Fatalf("--voice with none kept: %v", err)
	}
	vc := func(args ...string) {
		t.Helper()
		if _, _, err := runCLI(t, strings.NewReader(firstVoice), append([]string{"voice"}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	vc("set", "-")
	v, err := attachVoice(cmd, runOpts{})
	if err != nil || v == nil || string(v.Text) != firstVoice || v.Digest != voice.Digest([]byte(firstVoice)) {
		t.Fatalf("always-attach on: %+v %v", v, err)
	}
	if !strings.Contains(v.Origin, "kept on this computer") || !strings.Contains(v.Origin, "not reviewed") {
		t.Fatalf("origin %q", v.Origin)
	}
	if v, err := attachVoice(cmd, runOpts{noVoice: true}); err != nil || v != nil {
		t.Fatalf("--no-voice: %v %v", v, err)
	}
	if _, err := attachVoice(cmd, runOpts{voice: true, noVoice: true}); err == nil {
		t.Fatal("--voice and --no-voice together were accepted")
	}
	vc("attach", "off")
	if v, err := attachVoice(cmd, runOpts{}); err != nil || v != nil {
		t.Fatalf("always-attach off: %v %v", v, err)
	}
	if v, err := attachVoice(cmd, runOpts{voice: true}); err != nil || v == nil {
		t.Fatalf("--voice with always-attach off: %v %v", v, err)
	}

	t.Run("--voice-file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "VOICE.md")
		if err := os.WriteFile(f, []byte(secondVoice), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := attachVoice(cmd, runOpts{voiceFile: f})
		if err != nil || string(v.Text) != secondVoice {
			t.Fatalf("--voice-file: %+v %v", v, err)
		}
		if err := os.WriteFile(f, []byte(" \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := attachVoice(cmd, runOpts{voiceFile: f}); err == nil {
			t.Fatal("an empty --voice-file was attached")
		}
		link := filepath.Join(t.TempDir(), "linked.md")
		if err := os.Symlink(f, link); err == nil {
			if _, err := attachVoice(cmd, runOpts{voiceFile: link}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("a linked --voice-file: %v", err)
			}
		}
	})
}

// TestVoiceFromWebAddresses: --url reaches the build as url sources with AI
// Review and a web configuration on the person's own TypeSafe key and no
// search; without the key the addresses are refused, saying why, while
// files alone need none; and an address that is not https is refused.
func TestVoiceFromWebAddresses(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	signOut(t)
	vc := func(args ...string) (string, string, error) {
		return runCLI(t, strings.NewReader(""), append([]string{"voice"}, args...)...)
	}
	sample := filepath.Join(t.TempDir(), "post.md")
	if err := os.WriteFile(sample, []byte("Short. Direct.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("without the key", func(t *testing.T) {
		t.Setenv(policy.DefaultJudgeKeyEnv, "")
		fakeVoiceBuild(t, firstVoice)
		_, _, err := vc("create", "--url", "https://blog.example.com/post", "--write", filepath.Join(t.TempDir(), "VOICE.md"))
		wantClassified(t, err, engine.ExitPolicy)
		if !errors.Is(err, build.ErrNoReviewKey) || !strings.Contains(err.Error(), policy.DefaultJudgeKeyEnv) {
			t.Fatalf("err = %v", err)
		}
		got := fakeVoiceBuild(t, firstVoice)
		if _, _, err := vc("create", "--file", sample, "--write", filepath.Join(t.TempDir(), "VOICE.md")); err != nil || got.Review != nil || got.Web != nil || len(got.URLs) != 0 {
			t.Fatalf("files alone: %v %+v", err, got)
		}
	})
	t.Run("with the key", func(t *testing.T) {
		t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
		got := fakeVoiceBuild(t, firstVoice)
		_, stderr, err := vc("create", "--file", sample, "--url", "https://blog.example.com/post", "--url", "https://blog.example.com/other", "--write", filepath.Join(t.TempDir(), "VOICE.md"))
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr)
		}
		if len(got.URLs) != 2 || got.URLs[0].ID != "url-1" || got.URLs[1].URL != "https://blog.example.com/other" || got.Review == nil {
			t.Fatalf("options %+v", got)
		}
		if w := got.Web; w == nil || w.Scan != toolsrv.WebScanJev || w.JevKey != testJev || w.Search != "" || w.SearchKey != "" {
			t.Fatalf("web %+v", got.Web)
		}
		if !strings.Contains(stderr, "read by AI Review with your Jev key") || strings.Contains(stderr, testJev) {
			t.Fatalf("stderr:\n%s", stderr)
		}
	})
	t.Run("on a sandbox host, through the registry with the job's credential", func(t *testing.T) {
		t.Setenv(policy.DefaultJudgeKeyEnv, "")
		t.Setenv(envJobToken, "agyj_the-voice-job")
		got := fakeVoiceBuild(t, firstVoice)
		_, stderr, err := vc("create", "--url", "https://blog.example.com/post", "--render", "--write", filepath.Join(t.TempDir(), "VOICE.md"))
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr)
		}
		if _, ok := got.Review.(*webscan.Registry); !ok || !got.Render {
			t.Fatalf("review %T render %v", got.Review, got.Render)
		}
		if w := got.Web; w == nil || w.Scan != toolsrv.WebScanRegistry || w.JobToken != "agyj_the-voice-job" || w.JevKey != "" || w.Search != "" || w.Registry == "" {
			t.Fatalf("web %+v", got.Web)
		}
		if strings.Contains(stderr, "agyj_the-voice-job") {
			t.Fatal("the credential on stderr")
		}
		t.Setenv(envJobToken, "")
		t.Setenv(envHosted, "1")
		_, _, err = vc("create", "--url", "https://blog.example.com/post", "--write", filepath.Join(t.TempDir(), "VOICE.md"))
		wantClassified(t, err, engine.ExitPolicy)
		if !strings.Contains(err.Error(), envJobToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an address that is not https", func(t *testing.T) {
		t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
		fakeVoiceBuild(t, firstVoice)
		_, _, err := vc("create", "--url", "http://blog.example.com/post", "--write", filepath.Join(t.TempDir(), "VOICE.md"))
		wantClassified(t, err, engine.ExitUsage)
	})
}
