package gateway

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The exec image is built once from a generated Dockerfile: the runner
// image, the operator's catalogued packages installed and their resolved
// versions recorded into the image, git's safe.directory when git is
// among them, then the runner's minimize role in exec form (no shell is
// needed after busybox is gone). It is reused when present — the recorded
// versions read back either way — and skipped when nothing is declared
// (agentry-cli#9, #100).
func TestEnsureExecImage(t *testing.T) {
	img := spec.ExecImageSpec{
		Tag:      spec.ExecImageTag("agentry-runner:v1", []string{"jq", "git"}, []string{"git", "jq"}, ""),
		Commands: []string{"git", "jq"},
		Packages: []string{"git", "jq"},
	}
	fake := &dockerx.Fake{ExistingImages: map[string]bool{}, ImageFiles: map[string]string{
		img.Tag + ":" + ExecPackagesFile: "git-2.49.1-r0\njq-1.8.2-r0\n",
	}}
	ctxDir := t.TempDir()
	resolved, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", img, ctxDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(resolved, ",") != "git-2.49.1-r0,jq-1.8.2-r0" {
		t.Fatalf("resolved = %v", resolved)
	}
	built := false
	for _, op := range fake.Ops() {
		if strings.HasPrefix(op, "Build:"+img.Tag+"<-") {
			built = true
			dir := strings.TrimPrefix(op, "Build:"+img.Tag+"<-")
			df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"FROM agentry-runner:v1\n",
				"apk add --no-cache git jq",
				"apk list --installed git jq",
				// Paths inside the image, spelled for its Linux shell on
				// any host (agentry-cli#420).
				" && mkdir -p /etc/agentry && ",
				ExecPackagesFile,
				"git config --system --add safe.directory '/workspace'",
				"git config --system --add safe.directory '/workspace/*'",
				"git config --system --add safe.directory '/inputs/*'",
				`RUN ["/usr/local/bin/agentry-runner", "minimize", "git", "jq"]`,
			} {
				if !strings.Contains(string(df), want) {
					t.Fatalf("dockerfile lacks %q:\n%s", want, df)
				}
			}
			if strings.Contains(string(df), `\`) {
				t.Fatalf("a Windows path separator in the image's Dockerfile:\n%s", df)
			}
			if strings.Contains(string(df), "/etc/apk/repositories") {
				t.Fatalf("no mirror was configured, yet the dockerfile rewrites the repositories:\n%s", df)
			}
		}
	}
	if !built {
		t.Fatalf("no build recorded: %v", fake.Ops())
	}

	fake.ExistingImages[img.Tag] = true
	before := len(fake.Ops())
	again, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", img, ctxDir, io.Discard)
	if err != nil || strings.Join(again, ",") != "git-2.49.1-r0,jq-1.8.2-r0" {
		t.Fatalf("reuse: %v %v", again, err)
	}
	for _, op := range fake.Ops()[before:] {
		if strings.HasPrefix(op, "Build:") {
			t.Fatalf("reuse must not build: %v", fake.Ops()[before:])
		}
	}
	if r, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", spec.ExecImageSpec{}, ctxDir, io.Discard); err != nil || r != nil {
		t.Fatalf("no exec: %v %v", r, err)
	}
}

// TestEnsureExecImageMirrorAndPins: a pinned package reaches apk as
// written, a mirror rewrites the image's repositories for its own Alpine
// release before the install, and no git means no gitconfig line.
func TestEnsureExecImageMirrorAndPins(t *testing.T) {
	img := spec.ExecImageSpec{
		Tag:       spec.ExecImageTag("agentry-runner:v1", []string{"openssl"}, []string{"openssl=3.5.1-r0"}, "https://mirror.example/alpine"),
		Commands:  []string{"openssl"},
		Packages:  []string{"openssl=3.5.1-r0"},
		APKMirror: "https://mirror.example/alpine",
	}
	fake := &dockerx.Fake{ExistingImages: map[string]bool{}, ImageFiles: map[string]string{img.Tag + ":" + ExecPackagesFile: "openssl-3.5.1-r0\n"}}
	if _, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", img, t.TempDir(), io.Discard); err != nil {
		t.Fatal(err)
	}
	var df string
	for _, op := range fake.Ops() {
		if strings.HasPrefix(op, "Build:") {
			b, _ := os.ReadFile(filepath.Join(strings.SplitN(op, "<-", 2)[1], "Dockerfile"))
			df = string(b)
		}
	}
	for _, want := range []string{
		"apk add --no-cache openssl=3.5.1-r0",
		"apk list --installed openssl |",
		"'https://mirror.example/alpine' \"$v\"",
		"> /etc/apk/repositories",
	} {
		if !strings.Contains(df, want) {
			t.Fatalf("dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(df, "safe.directory") {
		t.Fatalf("no git declared, yet gitconfig is set:\n%s", df)
	}
}
