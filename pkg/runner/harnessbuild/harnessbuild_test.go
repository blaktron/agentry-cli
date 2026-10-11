package harnessbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// tgz is an npm tarball holding files under package/.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func integrity(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

// registry is a fake npm registry: the wrapper of release 9.9.9 and its two
// musl native packages, with what it was asked for.
type registry struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
	pkgs  map[string][]byte
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	r := &registry{pkgs: map[string][]byte{
		"/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz": tgz(t, map[string]string{
			"package.json": `{"name":"@anthropic-ai/claude-code","version":"9.9.9","optionalDependencies":{"@anthropic-ai/claude-code-linux-x64-musl":"9.9.9","@anthropic-ai/claude-code-linux-arm64-musl":"9.9.9"}}`,
			"install.cjs":  "// links the native binary",
		}),
		"/@anthropic-ai/claude-code-linux-x64-musl/-/claude-code-linux-x64-musl-9.9.9.tgz":     tgz(t, map[string]string{"claude": "x64 binary"}),
		"/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-9.9.9.tgz": tgz(t, map[string]string{"claude": "arm64 binary"}),
	}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.asked = append(r.asked, req.URL.Path)
		b, ok := r.pkgs[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.Close)
	return r
}

// release swaps the build's release table for one release of the fake
// registry, pinned to what it serves now, and returns the image to build.
func (r *registry) release(t *testing.T) policy.HarnessImage {
	t.Helper()
	rel := claudeRelease{
		version: "9.9.9",
		wrapper: integrity(r.pkgs["/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz"]),
		native: map[string]string{
			"amd64": integrity(r.pkgs["/@anthropic-ai/claude-code-linux-x64-musl/-/claude-code-linux-x64-musl-9.9.9.tgz"]),
			"arm64": integrity(r.pkgs["/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-9.9.9.tgz"]),
		},
	}
	saved := claudeReleases
	claudeReleases = []claudeRelease{rel}
	t.Cleanup(func() { claudeReleases = saved })
	return policy.HarnessImage{Ref: "agentry-claude-code:9.9.9", Artifact: rel.wrapper}
}

func builds(d *dockerx.Fake) []string {
	var out []string
	for _, op := range d.Ops() {
		if strings.HasPrefix(op, "Build:") {
			out = append(out, op)
		}
	}
	return out
}

// The image is built from the wrapper and the daemon's own platform's
// native package, each checked against its pinned integrity, and labelled
// with the artifact; the context is removed after.
func TestTheImageIsBuiltFromItsCheckedPackagesForTheDaemonsPlatform(t *testing.T) {
	reg := newRegistry(t)
	h := reg.release(t)
	d := &dockerx.Fake{Arch: "arm64"}
	dir := t.TempDir()
	var log strings.Builder
	b := Builder{Docker: d, Registry: reg.URL, Dir: dir, Log: &log}
	if err := b.Ensure(context.Background(), ClaudeImageAdapter, h); err != nil {
		t.Fatal(err)
	}
	if got := builds(d); len(got) != 1 || !strings.HasPrefix(got[0], "Build:agentry-claude-code:9.9.9<-") {
		t.Fatalf("builds %v", got)
	}
	args := d.BuildArgs[h.Ref]
	if args["BASE"] != ClaudeBase || args["NATIVE"] != "claude-code-linux-arm64-musl" || args["ARTIFACT"] != h.Artifact ||
		args["NATIVE_INTEGRITY"] != claudeReleases[0].native["arm64"] || args["RECIPE"] != ClaudeRecipe {
		t.Fatalf("build args %v", args)
	}
	if !slices.Equal(reg.asked, []string{"/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz", "/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-9.9.9.tgz"}) {
		t.Fatalf("asked the registry for %v", reg.asked)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("the build context was left behind: %v", left)
	}
	if !strings.Contains(log.String(), "each checked against its npm integrity") {
		t.Fatalf("log: %q", log.String())
	}
	if !strings.HasPrefix(ClaudeBase, "ghcr.io/blaktron/agentry-node-alpine:") || !policy.PinnedByDigest(ClaudeBase) {
		t.Fatalf("the base is not our Node image pinned by digest: %s", ClaudeBase)
	}
}

// A package whose bytes are not the ones pinned is refused, and nothing
// reaches the daemon: neither a build nor a file of it is left.
func TestAPackageWhoseIntegrityDiffersIsRefusedAndNothingIsBuilt(t *testing.T) {
	for _, path := range []string{
		"/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz",
		"/@anthropic-ai/claude-code-linux-x64-musl/-/claude-code-linux-x64-musl-9.9.9.tgz",
	} {
		reg := newRegistry(t)
		h := reg.release(t)
		// The registry now serves other bytes than the release pins.
		reg.pkgs[path] = append(slices.Clone(reg.pkgs[path]), 0)
		d := &dockerx.Fake{}
		dir := t.TempDir()
		err := Builder{Docker: d, Registry: reg.URL, Dir: dir}.Ensure(context.Background(), ClaudeImageAdapter, h)
		var ie *IntegrityError
		if !errors.As(err, &ie) || !strings.Contains(err.Error(), "nothing was built") {
			t.Fatalf("%s: %v", path, err)
		}
		if got := builds(d); len(got) != 0 {
			t.Fatalf("%s: built anyway: %v", path, got)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: left %v", path, left)
		}
	}
}

// An image of the tag that carries the artifact and this build's recipe is
// used as it is; one that carries another artifact, none, or an older
// recipe (an image built before the stand-in browser, agentry-cli#848) is
// built again.
func TestAnImageIsBuiltAgainUnlessItCarriesItsArtifact(t *testing.T) {
	for _, c := range []struct {
		artifact, recipe string
		rebuild          bool
	}{
		{"", ClaudeRecipe, true}, {"sha512-other", ClaudeRecipe, true},
		{"match", "", true}, {"match", "1", true}, {"match", ClaudeRecipe, false},
	} {
		reg := newRegistry(t)
		h := reg.release(t)
		label := c.artifact
		if label == "match" {
			label = h.Artifact
		}
		d := &dockerx.Fake{ExistingImages: map[string]bool{h.Ref: true}, Labels: map[string]map[string]string{h.Ref: {LabelArtifact: label, LabelRecipe: c.recipe}}}
		if err := (Builder{Docker: d, Registry: reg.URL, Dir: t.TempDir()}).Ensure(context.Background(), ClaudeImageAdapter, h); err != nil {
			t.Fatal(err)
		}
		if got := len(builds(d)) == 1; got != c.rebuild {
			t.Errorf("%+v: rebuilt %v", c, got)
		}
		if !c.rebuild && len(reg.asked) != 0 {
			t.Errorf("%+v: fetched %v for an image already here", c, reg.asked)
		}
	}
}

// What this build cannot make true is refused before anything is fetched:
// a release it does not know, a tag naming another version, a platform it
// has no native package for, an adapter it does not build, and a wrapper
// that installs another native package than the one pinned.
func TestWhatTheBuildCannotPinIsRefused(t *testing.T) {
	reg := newRegistry(t)
	h := reg.release(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		h       policy.HarnessImage
		arch    string
		adapter string
		want    string
	}{
		"unknown release": {policy.HarnessImage{Ref: h.Ref, Artifact: integrity([]byte("x"))}, "", ClaudeImageAdapter, "knows no Claude Code release"},
		"other version":   {policy.HarnessImage{Ref: "agentry-claude-code:9.9.8", Artifact: h.Artifact}, "", ClaudeImageAdapter, "its tag must be 9.9.9"},
		"platform":        {h, "s390x", ClaudeImageAdapter, "linux/s390x"},
		"adapter":         {h, "", "gemini-image", "cannot build"},
	} {
		d := &dockerx.Fake{Arch: c.arch}
		err := Builder{Docker: d, Registry: reg.URL, Dir: t.TempDir()}.Ensure(ctx, c.adapter, c.h)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(builds(d)) != 0 {
			t.Errorf("%s: built", name)
		}
	}
	if len(reg.asked) != 0 {
		t.Fatalf("fetched %v for a refused build", reg.asked)
	}

	// The wrapper's own package.json names the native package it installs;
	// a pinned wrapper naming another version is refused before the native
	// package is fetched.
	reg2 := newRegistry(t)
	reg2.pkgs["/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz"] = tgz(t, map[string]string{
		"package.json": `{"name":"@anthropic-ai/claude-code","version":"9.9.9","optionalDependencies":{"@anthropic-ai/claude-code-linux-x64-musl":"9.9.7"}}`,
	})
	h2 := reg2.release(t)
	d := &dockerx.Fake{}
	if err := (Builder{Docker: d, Registry: reg2.URL, Dir: t.TempDir()}).Ensure(ctx, ClaudeImageAdapter, h2); err == nil || !strings.Contains(err.Error(), "not this release's") {
		t.Fatalf("a wrapper installing another native package: %v", err)
	}
	if len(builds(d)) != 0 || len(reg2.asked) != 1 {
		t.Fatalf("built %v, asked %v", builds(d), reg2.asked)
	}
}

// The release this build carries is the policy's default pin, and names a
// native package for both platforms the CLI ships for.
func TestTheDefaultPinIsAReleaseThisBuildCarries(t *testing.T) {
	h, ok := policy.Default().Images.HarnessImageFor(ClaudeImageAdapter)
	if !ok {
		t.Fatal("no default claude-image")
	}
	rel, err := claudeReleaseFor(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if !strings.HasPrefix(rel.native[arch], "sha512-") || len(rel.native[arch]) != 95 {
			t.Errorf("%s: %q", arch, rel.native[arch])
		}
	}
	if !strings.Contains(claudeDockerfile, "mkdir -m 0700 "+ClaudeConfigDir+" && chown node:node "+ClaudeConfigDir) ||
		!strings.Contains(claudeDockerfile, "node install.cjs") || !strings.Contains(claudeDockerfile, "USER node") {
		t.Fatal("the Dockerfile lost its install, its config directory or its user")
	}
}

// The Claude Code image's browser is the stand-in opener (agentry-cli#848):
// the Dockerfile puts it where BROWSER names it and labels the recipe, and
// the script only writes the address to PID 1's stderr, opening nothing.
func TestTheClaudeImageCarriesItsStandInBrowser(t *testing.T) {
	for _, want := range []string{"COPY agentry-open " + ClaudeOpener, "chmod 0755 " + ClaudeOpener, `org.agentry.harness.recipe="${RECIPE}"`} {
		if !strings.Contains(claudeDockerfile, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
	if !strings.HasPrefix(claudeOpenerScript, "#!/bin/sh\n") || !strings.Contains(claudeOpenerScript, `printf 'agentry-open %s\n' "$1" > /proc/1/fd/2`) {
		t.Fatalf("opener %q", claudeOpenerScript)
	}
	for _, never := range []string{"xdg-open", "curl", "wget", "http"} {
		if strings.Contains(strings.SplitN(claudeOpenerScript, "\nprintf", 2)[1], never) {
			t.Errorf("the opener does more than hand the address on: %q", never)
		}
	}
}
