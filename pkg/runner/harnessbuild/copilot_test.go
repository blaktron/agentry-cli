package harnessbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releases is a fake GitHub release of Copilot CLI 9.9.9: the two musl
// downloads and the checksums file that names them, with what it was
// asked for.
type releases struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
	files map[string][]byte
}

func newReleases(t *testing.T) *releases {
	t.Helper()
	r := &releases{files: map[string][]byte{
		"/v9.9.9/copilot-linuxmusl-x64.tar.gz":   []byte("x64 download"),
		"/v9.9.9/copilot-linuxmusl-arm64.tar.gz": []byte("arm64 download"),
	}}
	r.files["/v9.9.9/SHA256SUMS.txt"] = []byte(
		sha256Of([]byte("glibc download")) + "  copilot-linux-x64.tar.gz\n" +
			sha256Of(r.files["/v9.9.9/copilot-linuxmusl-arm64.tar.gz"]) + "  copilot-linuxmusl-arm64.tar.gz\n" +
			sha256Of(r.files["/v9.9.9/copilot-linuxmusl-x64.tar.gz"]) + "  copilot-linuxmusl-x64.tar.gz\n")
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.asked = append(r.asked, req.URL.Path)
		b, ok := r.files[req.URL.Path]
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

// release swaps the build's release table for release 9.9.9, pinned to
// the checksums file served now, and returns the image to build.
func (r *releases) release(t *testing.T) policy.HarnessImage {
	t.Helper()
	rel := copilotRelease{version: "9.9.9", sums: "sha256:" + sha256Of(r.files["/v9.9.9/SHA256SUMS.txt"])}
	saved := copilotReleases
	copilotReleases = []copilotRelease{rel}
	t.Cleanup(func() { copilotReleases = saved })
	return policy.HarnessImage{Ref: "agentry-copilot:9.9.9", Artifact: rel.sums}
}

// The image is built from the daemon's platform's musl download, checked
// against the checksum the release's checksums file names, which is itself
// held to the pin; the image is labelled with both, the binary is unpacked
// into the image at build, and the context is removed after.
func TestTheCopilotImageIsBuiltFromTheCheckedDownloadForTheDaemonsPlatform(t *testing.T) {
	for arch, asset := range map[string]string{"amd64": "copilot-linuxmusl-x64.tar.gz", "arm64": "copilot-linuxmusl-arm64.tar.gz"} {
		rs := newReleases(t)
		h := rs.release(t)
		d := &dockerx.Fake{Arch: arch}
		dir := t.TempDir()
		var log strings.Builder
		b := Builder{Docker: d, Releases: rs.URL, Dir: dir, Log: &log}
		if err := b.Ensure(context.Background(), CopilotImageAdapter, h); err != nil {
			t.Fatal(err)
		}
		if got := builds(d); len(got) != 1 || !strings.HasPrefix(got[0], "Build:agentry-copilot:9.9.9<-") {
			t.Fatalf("%s: builds %v", arch, got)
		}
		args := d.BuildArgs[h.Ref]
		if args["BASE"] != CopilotBase || args["ARTIFACT"] != h.Artifact || args["DOWNLOAD"] != "sha256:"+sha256Of(rs.files["/v9.9.9/"+asset]) {
			t.Fatalf("%s: build args %v", arch, args)
		}
		if !slices.Equal(rs.asked, []string{"/v9.9.9/SHA256SUMS.txt", "/v9.9.9/" + asset}) {
			t.Fatalf("%s: asked GitHub for %v", arch, rs.asked)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: the build context was left behind: %v", arch, left)
		}
		if !strings.Contains(log.String(), "GitHub signs neither") {
			t.Fatalf("%s: log: %q", arch, log.String())
		}
	}
	if !strings.HasPrefix(CopilotBase, "ghcr.io/blaktron/agentry-alpine:") || !policy.PinnedByDigest(CopilotBase) {
		t.Fatalf("the base is not our Alpine image pinned by digest: %s", CopilotBase)
	}
	for _, want := range []string{"COPILOT_SEA_EXTRACT_ONLY=1 COPILOT_PKG_CACHE_HOME=" + CopilotPkgCache, "-perm -4000 -o -perm -2000",
		"ENV COPILOT_PKG_CACHE_HOME=" + CopilotPkgCache, "USER copilot", "adduser -D -u 10001"} {
		if !strings.Contains(copilotDockerfile, want) {
			t.Fatalf("the Dockerfile lost %q", want)
		}
	}
}

// A checksums file other than the pinned one, or a download other than the
// one it names, is refused, and nothing reaches the daemon.
func TestACopilotDownloadWhoseChecksumDiffersIsRefusedAndNothingIsBuilt(t *testing.T) {
	for _, path := range []string{"/v9.9.9/SHA256SUMS.txt", "/v9.9.9/copilot-linuxmusl-x64.tar.gz"} {
		rs := newReleases(t)
		h := rs.release(t)
		// GitHub now serves other bytes than the release pins.
		rs.files[path] = append(slices.Clone(rs.files[path]), '\n')
		d := &dockerx.Fake{}
		dir := t.TempDir()
		err := Builder{Docker: d, Releases: rs.URL, Dir: dir}.Ensure(context.Background(), CopilotImageAdapter, h)
		var ce *ChecksumError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), "nothing was built") {
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

// An image of the tag that carries the artifact is used as it is; one that
// carries another, or none, is built again.
func TestACopilotImageIsBuiltAgainUnlessItCarriesItsArtifact(t *testing.T) {
	for label, rebuild := range map[string]bool{"": true, "sha256:other": true, "match": false} {
		rs := newReleases(t)
		h := rs.release(t)
		if label == "match" {
			label = h.Artifact
		}
		d := &dockerx.Fake{ExistingImages: map[string]bool{h.Ref: true}, Labels: map[string]map[string]string{h.Ref: {LabelArtifact: label}}}
		if err := (Builder{Docker: d, Releases: rs.URL, Dir: t.TempDir()}).Ensure(context.Background(), CopilotImageAdapter, h); err != nil {
			t.Fatal(err)
		}
		if got := len(builds(d)) == 1; got != rebuild {
			t.Errorf("label %q: rebuilt %v", label, got)
		}
		if !rebuild && len(rs.asked) != 0 {
			t.Errorf("label %q: fetched %v for an image already here", label, rs.asked)
		}
	}
}

// What this build cannot make true is refused before anything is fetched
// (a release it does not know, a tag naming another version, a platform
// GitHub has no musl download for), and a checksums file naming no
// download for the platform refuses the build.
func TestWhatTheCopilotBuildCannotPinIsRefused(t *testing.T) {
	rs := newReleases(t)
	h := rs.release(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		h    policy.HarnessImage
		arch string
		want string
	}{
		"unknown release": {policy.HarnessImage{Ref: h.Ref, Artifact: "sha256:" + sha256Of([]byte("x"))}, "", "knows no Copilot CLI release"},
		"other version":   {policy.HarnessImage{Ref: "agentry-copilot:9.9.8", Artifact: h.Artifact}, "", "its tag must be 9.9.9"},
		"platform":        {h, "s390x", "linux/s390x"},
	} {
		d := &dockerx.Fake{Arch: c.arch}
		err := Builder{Docker: d, Releases: rs.URL, Dir: t.TempDir()}.Ensure(ctx, CopilotImageAdapter, c.h)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(builds(d)) != 0 {
			t.Errorf("%s: built", name)
		}
	}
	if len(rs.asked) != 0 {
		t.Fatalf("fetched %v for a refused build", rs.asked)
	}

	rs2 := newReleases(t)
	rs2.files["/v9.9.9/SHA256SUMS.txt"] = []byte(sha256Of([]byte("glibc")) + "  copilot-linux-x64.tar.gz\n")
	h2 := rs2.release(t)
	d := &dockerx.Fake{}
	if err := (Builder{Docker: d, Releases: rs2.URL, Dir: t.TempDir()}).Ensure(ctx, CopilotImageAdapter, h2); err == nil || !strings.Contains(err.Error(), "names no copilot-linuxmusl-x64.tar.gz") {
		t.Fatalf("a checksums file without the platform's download: %v", err)
	}
	if len(builds(d)) != 0 || len(rs2.asked) != 1 {
		t.Fatalf("built %v, asked %v", builds(d), rs2.asked)
	}
}

// The release this build carries is the policy's default pin, and the
// receipt's note says what the pin does not prove.
func TestTheDefaultCopilotPinIsAReleaseThisBuildCarries(t *testing.T) {
	h, ok := policy.Default().Images.HarnessImageFor(CopilotImageAdapter)
	if !ok {
		t.Fatal("no default copilot-image")
	}
	if _, err := copilotReleaseFor(h); err != nil {
		t.Fatal(err)
	}
	if !Builds(CopilotImageAdapter) || !strings.Contains(PinNote(CopilotImageAdapter), "pinned by checksum, not signed") || PinNote(ClaudeImageAdapter) != "" {
		t.Fatal("the Copilot image's pin note")
	}
}
