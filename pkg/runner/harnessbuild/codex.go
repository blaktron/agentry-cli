package harnessbuild

// The Codex image built on this machine: OpenAI's Codex CLI release binary,
// unmodified, from the newest release of openai/codex, on agentry-dockerimages'
// Alpine base, laid out as agentry-dockerimages' published agentry-codex
// image is (the binary, the release record and its signature bundle, the
// licence files, uid 10001, CODEX_HOME the user's home). Nothing reaches the
// daemon until the binary verifies against OpenAI's keyless signature from
// the release workflow at that tag and commit. The CLI's tested Codex is the
// published image, pinned by digest; this build is only ever of a newer
// release, and the tested image is the fallback.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// CodexImageAdapter is the adapter whose newest image codex.go builds (the
// adapter package names it too).
const CodexImageAdapter = "codex-image"

// CodexBase is the image the Codex binary is put on: the same Alpine mirror
// as the published agentry-codex image and the Copilot CLI image, pinned by
// digest. The binary is static (musl).
const CodexBase = CopilotBase

// Where Codex's releases are: GitHub's API for the newest release and the
// commit its tag names, the release downloads, and the files at a commit.
const (
	CodexAPI      = "https://api.github.com/repos/openai/codex"
	CodexReleases = "https://github.com/openai/codex/releases/download"
	CodexFiles    = "https://raw.githubusercontent.com/openai/codex"
)

// codexImageName is the local name a newest Codex image is tagged with,
// before its version.
const codexImageName = "agentry-codex"

// codexTriple is the release's name for a daemon's architecture.
var codexTriple = map[string]string{"amd64": "x86_64-unknown-linux-musl", "arm64": "aarch64-unknown-linux-musl"}

// codexTagRE is the tag of a Codex CLI release.
var codexTagRE = regexp.MustCompile(`^rust-v([0-9]+\.[0-9]+\.[0-9]+)$`)

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Bounds on what is read from GitHub: its API's answers and the release's
// signature bundle are small, a licence file a few hundred KB at most.
const (
	maxAPI     = 4 << 20
	maxBundle  = 1 << 20
	maxLicence = 1 << 20
)

// codexRelease is a Codex release this build made an image of, as the
// record of the newest images keeps it: enough to build it again, held to
// the same binary.
type codexRelease struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
	// Binary is the SHA-256 (hex) of the release binary for each
	// architecture it was built for; the image's artifact is the daemon's.
	Binary map[string]string `json:"binary"`
}

// codexRef is the local tag of the image of version.
func codexRef(version string) string { return codexImageName + ":" + version }

// newestCodex asks GitHub for openai/codex's newest release (a published
// release, never a draft or a prerelease) and the commit its tag names.
func (b Builder) newestCodex(ctx context.Context) (codexRelease, error) {
	api := strings.TrimRight(b.CodexAPI, "/")
	if api == "" {
		api = CodexAPI
	}
	var rel struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := b.getJSON(ctx, api+"/releases/latest", &rel, "Codex's newest release", "GitHub"); err != nil {
		return codexRelease{}, err
	}
	m := codexTagRE.FindStringSubmatch(rel.Tag)
	if m == nil || rel.Draft || rel.Prerelease {
		return codexRelease{}, fmt.Errorf("GitHub's newest Codex release %q is not a published rust-v<version> release", rel.Tag)
	}
	type object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var ref struct {
		Object object `json:"object"`
	}
	if err := b.getJSON(ctx, api+"/git/ref/tags/"+rel.Tag, &ref, "Codex's tag "+rel.Tag, "GitHub"); err != nil {
		return codexRelease{}, err
	}
	o := ref.Object
	// An annotated tag names a tag object, which names the commit.
	for range 2 {
		if o.Type != "tag" {
			break
		}
		var tag struct {
			Object object `json:"object"`
		}
		if err := b.getJSON(ctx, api+"/git/tags/"+o.SHA, &tag, "Codex's tag "+rel.Tag, "GitHub"); err != nil {
			return codexRelease{}, err
		}
		o = tag.Object
	}
	if o.Type != "commit" || !commitRE.MatchString(o.SHA) {
		return codexRelease{}, fmt.Errorf("the tag %s of Codex does not name a commit", rel.Tag)
	}
	return codexRelease{Version: m[1], Tag: rel.Tag, Commit: o.SHA}, nil
}

// buildCodex fetches rel's binary for the daemon's platform, verifies it
// against OpenAI's signature at rel's tag and commit (and, when rel
// already names the binary's SHA-256 for this platform, holds it to that),
// and builds the image from it. It returns the image and rel with the
// binary's SHA-256 recorded.
func (b Builder) buildCodex(ctx context.Context, rel codexRelease) (policy.HarnessImage, codexRelease, error) {
	arch, err := b.Docker.ServerArch(ctx)
	if err != nil {
		return policy.HarnessImage{}, rel, err
	}
	triple := codexTriple[arch]
	if triple == "" {
		return policy.HarnessImage{}, rel, fmt.Errorf("no Codex %s image for this docker daemon's platform (linux/%s); it builds for linux/amd64 and linux/arm64", rel.Version, arch)
	}
	if !codexTagRE.MatchString(rel.Tag) || !commitRE.MatchString(rel.Commit) {
		return policy.HarnessImage{}, rel, fmt.Errorf("the Codex release %q at %q is not one this build can verify", rel.Tag, rel.Commit)
	}
	b.building(rel.Version)
	dir, err := os.MkdirTemp(b.Dir, "agentry-codex-")
	if err != nil {
		return policy.HarnessImage{}, rel, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	releases := strings.TrimRight(b.CodexReleases, "/")
	if releases == "" {
		releases = CodexReleases
	}
	base := releases + "/" + rel.Tag + "/codex-" + triple
	tgz := filepath.Join(dir, "codex.tar.gz")
	if _, err := b.get(ctx, base+".tar.gz", tgz, maxDownload, sha256.New(), "Codex "+rel.Version, "GitHub"); err != nil {
		return policy.HarnessImage{}, rel, err
	}
	bin := filepath.Join(dir, "codex")
	sum, err := extractOne(tgz, "codex-"+triple, bin, maxDownload)
	if err != nil {
		return policy.HarnessImage{}, rel, fmt.Errorf("the download of Codex %s: %w", rel.Version, err)
	}
	_ = os.Remove(tgz)
	got := hex.EncodeToString(sum)
	if want := rel.Binary[arch]; want != "" && want != got {
		return policy.HarnessImage{}, rel, &ChecksumError{What: "Codex " + rel.Version + " binary", Want: want, Got: got}
	}
	bundlePath := filepath.Join(dir, "codex.sigstore")
	if _, err := b.get(ctx, base+".sigstore", bundlePath, maxBundle, sha256.New(), "Codex "+rel.Version+"'s signature", "GitHub"); err != nil {
		return policy.HarnessImage{}, rel, err
	}
	raw, err := os.ReadFile(bundlePath) //nolint:gosec // a file in the build's own folder
	if err != nil {
		return policy.HarnessImage{}, rel, err
	}
	tm, err := trustedRoot(ctx, b.sigstoreCache())
	if err != nil {
		return policy.HarnessImage{}, rel, fmt.Errorf("the trust root of Sigstore: %w", err)
	}
	if err := verifyCodex(tm, raw, sum, rel.Tag, rel.Commit); err != nil {
		return policy.HarnessImage{}, rel, err
	}
	files := strings.TrimRight(b.CodexFiles, "/")
	if files == "" {
		files = CodexFiles
	}
	for _, name := range []string{"LICENSE", "NOTICE"} {
		if _, err := b.get(ctx, files+"/"+rel.Commit+"/"+name, filepath.Join(dir, name), maxLicence, sha256.New(), "Codex's "+name, "GitHub"); err != nil {
			return policy.HarnessImage{}, rel, err
		}
	}
	record := fmt.Sprintf("version=%s\ntag=%s\ncommit=%s\nbinary=codex-%s\nsha256=%s\nverified=sigstore %s@refs/tags/%s\n",
		rel.Version, rel.Tag, rel.Commit, triple, got, codexWorkflow, rel.Tag)
	if err := os.WriteFile(filepath.Join(dir, "release"), []byte(record), 0o600); err != nil {
		return policy.HarnessImage{}, rel, err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(codexDockerfile), 0o600); err != nil {
		return policy.HarnessImage{}, rel, err
	}
	h := policy.HarnessImage{Ref: codexRef(rel.Version), Artifact: "sha256:" + got}
	b.logf("building %s from Codex %s (codex-%s, verified against OpenAI's signature from its release workflow at %s), on %s\n",
		h.Ref, rel.Version, triple, rel.Tag, CodexBase)
	if err := b.Docker.Build(ctx, dir, h.Ref, map[string]string{"BASE": CodexBase, "ARTIFACT": h.Artifact, "VERSION": rel.Version}); err != nil {
		return policy.HarnessImage{}, rel, fmt.Errorf("build %s: %w", h.Ref, err)
	}
	if rel.Binary == nil {
		rel.Binary = map[string]string{}
	}
	rel.Binary[arch] = got
	return h, rel, nil
}

// codexDockerfile lays the image out as the published agentry-codex is,
// and runs the binary once as the smoke check: it must start on the base
// and report the release's version, or the build fails.
var codexDockerfile = `ARG BASE
FROM ${BASE}
ARG ARTIFACT
ARG VERSION
LABEL org.agentry.harness.artifact="${ARTIFACT}"
` + imagelabel.Line(imagelabel.Codex) + `RUN addgroup -g 10001 codex \
    && adduser -D -u 10001 -G codex -h /home/codex -s /sbin/nologin codex \
    && mkdir -p /etc/agentry/codex /usr/share/licenses/codex
COPY codex /usr/local/bin/codex
COPY release codex.sigstore /etc/agentry/codex/
COPY LICENSE NOTICE /usr/share/licenses/codex/
RUN set -e; chmod 0755 /usr/local/bin/codex; chmod 0644 /etc/agentry/codex/* /usr/share/licenses/codex/*; \
    mkdir /tmp/codex-check; v="$(CODEX_HOME=/tmp/codex-check /usr/local/bin/codex --version)"; rm -rf /tmp/codex-check; \
    test "$v" = "codex-cli ${VERSION}" || { echo "codex --version printed '$v', not 'codex-cli ${VERSION}'" >&2; exit 1; }
ENV CODEX_HOME=/home/codex
USER 10001:10001
WORKDIR /home/codex
ENTRYPOINT ["/usr/local/bin/codex"]
`

// extractOne extracts the one member name of the gzipped tar tgz into
// dest, at most limit bytes, and returns its SHA-256: a tarball holding
// anything else is refused.
func extractOne(tgz, name, dest string, limit int64) ([]byte, error) {
	f, err := os.Open(tgz) //nolint:gosec // a file in the build's own folder
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	var sum []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != name || hdr.Typeflag != tar.TypeReg || sum != nil {
			return nil, fmt.Errorf("it holds %q, not the one binary %s", hdr.Name, name)
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a file in the build's own folder
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, limit+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		if n > limit {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
		}
		sum = h.Sum(nil)
	}
	if sum == nil {
		return nil, fmt.Errorf("it does not hold %s", name)
	}
	return sum, nil
}

// getJSON decodes the JSON answer at url into v, at most maxAPI bytes.
func (b Builder) getJSON(ctx context.Context, url string, v any, what, who string) error {
	f, err := os.CreateTemp(b.Dir, "agentry-answer-")
	if err != nil {
		return err
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()
	if _, err := b.get(ctx, url, path, maxAPI, sha256.New(), what, who); err != nil {
		return err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a file this function made
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %s's answer: %w", what, who, err)
	}
	return nil
}

// sigstoreCache is where Sigstore's trust root is cached.
func (b Builder) sigstoreCache() string {
	if b.CacheDir != "" {
		return filepath.Join(b.CacheDir, "sigstore")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentry-sigstore")
	}
	return filepath.Join(dir, "agentry", "sigstore")
}
