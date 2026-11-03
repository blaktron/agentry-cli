package localrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// TestAFolderClamAVFoundMalwareInIsRefused: the decode step runs over a copy of
// a folder, and a folder holding a file ClamAV found a signature in is refused,
// naming the file and the signature. With the malware check on, the folder is
// reported checked only when it holds files and every one was read and found
// clean; with it off no signatures are fetched and nothing is reported checked.
// A step that fails is the check's failure, and the check leaves nothing
// beside the folder.
func TestAFolderClamAVFoundMalwareInIsRefused(t *testing.T) {
	sigs := 0
	was := checkSignatures
	checkSignatures = func(_ context.Context, _, ref string, _ io.Writer) clamdb.DB {
		sigs++
		return clamdb.DB{Dir: t.TempDir(), Ref: ref, Digest: "sha256:84cbc016d58b"}
	}
	t.Cleanup(func() { checkSignatures = was })

	// check is a folder of two files, checked under pol as the decode's scan
	// record says, on a daemon that has every image already but the decode
	// image's, which a case may leave out.
	check := func(t *testing.T, pol *policy.Policy, scanned []runnerdecode.File, files map[string]string, edit func(f *dockerx.Fake, decodeImage string)) (bool, error) {
		t.Helper()
		pol.Images.Runner = "agentry-runner:test"
		runner := pol.RunnerImageTag(runnerimage.ContentVersion())
		f := &dockerx.Fake{ExistingImages: map[string]bool{pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true, runner: true}}
		decode.ScriptFake(f, runner, pol.Images.FilesImage())
		scripted := f.Foreground["agentry-decode-"]
		writeReport := scripted.Before
		scripted.Before = func(r dockerx.RunSpec) {
			writeReport(r)
			held := slices.ContainsFunc(r.Mounts, func(m dockerx.Mount) bool { return m.Target == "/clamav" })
			scripted.Stdout = string(runnerdecode.RecordOf(scanned, held)) + "\n"
		}
		if edit != nil {
			edit(f, decode.ImageSpec(runner, pol.Images.FilesImage()).Tag)
		}
		parent := t.TempDir()
		dir := filepath.Join(parent, "skill")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for rel, body := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		clamav, err := CheckFolder(context.Background(), f, pol, t.TempDir(), dir, nil)
		if left, _ := filepath.Glob(filepath.Join(parent, ".check-*")); len(left) != 0 {
			t.Errorf("the check left %v beside the folder", left)
		}
		return clamav, err
	}
	two := map[string]string{"SKILL.md": "---\nname: skill\ndescription: Use it.\n---\n", "notes/a.md": "notes\n"}

	for _, tc := range []struct {
		name    string
		on      bool
		scanned []runnerdecode.File
		files   map[string]string
		want    bool
		refusal string
	}{
		{"check off", false, nil, two, false, ""},
		{"check on, every file clean", true, []runnerdecode.File{
			{Path: "SKILL.md", Check: runnerdecode.CheckClean}, {Path: "notes/a.md", Check: runnerdecode.CheckClean}}, two, true, ""},
		{"check on, one file found", true, []runnerdecode.File{
			{Path: "SKILL.md", Check: runnerdecode.CheckClean}, {Path: "notes/a.md", Check: runnerdecode.CheckFound, Signature: "Eicar-Test-Signature"}}, two, false,
			"ClamAV found malware in notes/a.md (Eicar-Test-Signature)"},
		{"check on, one file not read", true, []runnerdecode.File{
			{Path: "SKILL.md", Check: runnerdecode.CheckClean}, {Path: "notes/a.md", Check: runnerdecode.CheckUnchecked, Unchecked: runnerdecode.UncheckedTooLarge}}, two, false, ""},
		{"check on, no file", true, nil, nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sigs = 0
			pol := policy.Default()
			pol.MalwareCheck = &tc.on
			clamav, err := check(t, pol, tc.scanned, tc.files, nil)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v, want %q", err, tc.refusal)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if clamav != tc.want {
				t.Errorf("checked by ClamAV = %v, want %v", clamav, tc.want)
			}
			if wantSigs := map[bool]int{true: 1, false: 0}[tc.on]; sigs != wantSigs {
				t.Errorf("signatures fetched %d times, want %d", sigs, wantSigs)
			}
		})
	}

	t.Run("a step that fails is the check's failure", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			edit func(f *dockerx.Fake, decodeImage string)
			want string
		}{
			{"no docker", func(f *dockerx.Fake, _ string) { f.AvailableErr = errors.New("no docker daemon") }, "no docker daemon"},
			{"a decode image that cannot be built", func(f *dockerx.Fake, decodeImage string) {
				f.ExistingImages[decodeImage] = false
				f.FailBuild = map[string]error{decodeImage: errors.New("build refused")}
			}, "build the decode image"},
			{"a decode that fails", func(f *dockerx.Fake, _ string) {
				f.Foreground["agentry-decode-"] = &dockerx.FakeForeground{ExitCode: 3}
			}, "the decode container failed (exit 3)"},
		} {
			_, err := check(t, policy.Default(), nil, two, tc.edit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
			}
		}
		// A decode image that is not built yet is built, and said so, with no log
		// writer given.
		if _, err := check(t, policy.Default(), nil, two, func(f *dockerx.Fake, decodeImage string) { f.ExistingImages[decodeImage] = false }); err != nil {
			t.Errorf("a decode image that is built on the way: %v", err)
		}
	})
}
