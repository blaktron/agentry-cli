package harnessbuild

// The Copilot CLI image (plan model-sign-ins.md §4.5, D27;
// agentry-cli#741): GitHub's Copilot CLI, unmodified, from the release's
// musl download for the daemon's platform, on agentry-dockerimages'
// Alpine base. GitHub signs neither the download nor its checksums file,
// so the pin is the checksums file's own SHA-256: the build fetches
// SHA256SUMS.txt, holds it to the artifact, and holds the download to the
// checksum it names, before the daemon is touched. The receipt says the
// image is pinned by checksum, not signed.
//
// The download is a Node single-executable application that unpacks its
// package (about 200 MB, with the native runtime it loads) into a cache on
// first start. A run's home is a tmpfs, which docker mounts noexec, so the
// build unpacks it once, with the binary's own extract-only switch, into
// the image, where a run reads it read-only (COPILOT_PKG_CACHE_HOME).

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// CopilotImageAdapter is the adapter whose image copilot.go builds (the
// adapter package names it too).
const CopilotImageAdapter = "copilot-image"

// CopilotBase is the image Copilot CLI is installed on: agentry-dockerimages'
// mirror of alpine:3.22, the runner image's own runtime base, pinned by
// digest. The musl download needs nothing but musl's libc.
const CopilotBase = "ghcr.io/blaktron/agentry-alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

// CopilotReleases is where GitHub publishes Copilot CLI's release
// downloads, each under v<version>/.
const CopilotReleases = "https://github.com/github/copilot-cli/releases/download"

// CopilotPinNote is what the receipt says of the pin (plan D27).
const CopilotPinNote = "pinned by checksum, not signed: GitHub publishes the download's SHA-256 and no signature"

// The image's layout: the binary, the unpacked package it runs from, and
// the user it runs as.
const (
	CopilotBinary   = "/usr/local/bin/copilot"
	CopilotPkgCache = "/opt/copilot/cache"
	CopilotHome     = "/home/copilot"
	// LabelDownload is the checksum of the download the image holds.
	LabelDownload = "org.agentry.harness.download"
)

// copilotRelease is a Copilot CLI release this build can make an image of:
// the SHA-256 of its SHA256SUMS.txt, which is the policy's artifact.
type copilotRelease struct {
	version string
	sums    string
}

// copilotReleases were read from GitHub's release on 2026-10-09: 1.0.95
// was the latest stable release that day, its checksums file fetched and
// hashed, and its linuxmusl-x64 download checked against it. A bump adds a
// release here, moves policy.DefaultHarnessImages["copilot-image"], and
// re-records the adapter's conformance.
var copilotReleases = []copilotRelease{{
	version: "1.0.95",
	sums:    "sha256:fd36c0e57ae6285ccd9cff306fbcdf114343c44f968ff2364fa7fd1a4dc4b7bf",
}}

// copilotArch is the release's name for a daemon's architecture.
var copilotArch = map[string]string{"amd64": "x64", "arm64": "arm64"}

// Bounds on what is downloaded: the checksums file is a few lines, the
// musl download about 120 MB.
const (
	maxSums     = 64 << 10
	maxDownload = 1 << 30
)

// copilotReleaseFor is the release h's artifact names, whose version h's
// tag must be.
func copilotReleaseFor(h policy.HarnessImage) (copilotRelease, error) {
	known := make([]string, 0, len(copilotReleases))
	for _, r := range copilotReleases {
		known = append(known, r.version)
		if r.sums != h.Artifact {
			continue
		}
		if !strings.HasSuffix(h.Ref, ":"+r.version) {
			return copilotRelease{}, fmt.Errorf("the %s harness image %s is pinned to Copilot CLI %s's checksums, so its tag must be %s", CopilotImageAdapter, h.Ref, r.version, r.version)
		}
		return r, nil
	}
	return copilotRelease{}, fmt.Errorf("this build of agentry knows no Copilot CLI release whose SHA256SUMS.txt hashes to %s (it can build %s): remove images.harnessImages.%s from the policy for the default",
		h.Artifact, strings.Join(known, ", "), CopilotImageAdapter)
}

// ChecksumError is a download whose bytes are not the ones this build
// pins: the build is refused and nothing reaches the daemon.
type ChecksumError struct {
	What, Want, Got string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("refused: GitHub's %s is not the one this build of agentry pins (sha256 %s, and it hashes to %s); nothing was built", e.What, e.Want, e.Got)
}

// buildCopilot fetches the checksums file and the daemon's platform's musl
// download, holds each to its checksum, and builds the image from the
// download alone: the build reaches no registry but the base's.
func (b Builder) buildCopilot(ctx context.Context, h policy.HarnessImage, rel copilotRelease) error {
	arch, err := b.Docker.ServerArch(ctx)
	if err != nil {
		return err
	}
	cpu := copilotArch[arch]
	if cpu == "" {
		return fmt.Errorf("no Copilot CLI %s image for this docker daemon's platform (linux/%s); it builds for linux/amd64 and linux/arm64", rel.version, arch)
	}
	asset := "copilot-linuxmusl-" + cpu + ".tar.gz"
	dir, err := os.MkdirTemp(b.Dir, "agentry-copilot-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	sumsPath := filepath.Join(dir, "SHA256SUMS.txt")
	if err := b.download(ctx, rel.version, "SHA256SUMS.txt", sumsPath, maxSums, rel.sums); err != nil {
		return err
	}
	want, err := sumFor(sumsPath, asset)
	if err != nil {
		return fmt.Errorf("the checksums of Copilot CLI %s: %w", rel.version, err)
	}
	if err := b.download(ctx, rel.version, asset, filepath.Join(dir, "copilot.tar.gz"), maxDownload, want); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(copilotDockerfile), 0o600); err != nil {
		return err
	}
	b.logf("building %s from Copilot CLI %s's %s, checked against the release's SHA256SUMS.txt (%s; GitHub signs neither), on %s (first use)\n",
		h.Ref, rel.version, asset, rel.sums, CopilotBase)
	err = b.Docker.Build(ctx, dir, h.Ref, map[string]string{
		"BASE":     CopilotBase,
		"ARTIFACT": h.Artifact,
		"DOWNLOAD": want,
	})
	if err != nil {
		return fmt.Errorf("build %s: %w", h.Ref, err)
	}
	return nil
}

// copilotDockerfile unpacks the download in a stage of its own, then runs
// the binary's own extraction into the cache a run reads, so the image
// holds the binary and its unpacked package, not the tarball. Copilot CLI
// is run once as a check that it starts on the base; a setuid or setgid
// file in what it unpacked refuses the build.
const copilotDockerfile = `ARG BASE
FROM ${BASE} AS unpack
COPY copilot.tar.gz /tmp/
RUN set -e; mkdir -p /opt/copilot/bin ` + CopilotPkgCache + `; \
    tar -xzf /tmp/copilot.tar.gz -C /opt/copilot/bin copilot; \
    COPILOT_SEA_EXTRACT_ONLY=1 COPILOT_PKG_CACHE_HOME=` + CopilotPkgCache + ` HOME=/tmp /opt/copilot/bin/copilot --binary-version; \
    test -n "$(ls ` + CopilotPkgCache + `/pkg)"; \
    if [ -n "$(find /opt/copilot -perm -4000 -o -perm -2000)" ]; then echo "a setuid or setgid file in the download" >&2; exit 1; fi

FROM ${BASE}
ARG ARTIFACT
ARG DOWNLOAD
LABEL org.agentry.harness.artifact="${ARTIFACT}" org.agentry.harness.download="${DOWNLOAD}"
COPY --from=unpack /opt/copilot/ /opt/copilot/
RUN set -e; adduser -D -u 10001 -h ` + CopilotHome + ` copilot; ln -s /opt/copilot/bin/copilot ` + CopilotBinary + `; \
    COPILOT_PKG_CACHE_HOME=` + CopilotPkgCache + ` COPILOT_HOME=/tmp/copilot-check HOME=/tmp/copilot-check copilot --version; rm -rf /tmp/copilot-check
ENV COPILOT_PKG_CACHE_HOME=` + CopilotPkgCache + `
USER copilot
WORKDIR ` + CopilotHome + `
`

// download fetches the release's asset into dest and holds it to want
// (sha256:<hex>), removing it when it does not match.
func (b Builder) download(ctx context.Context, version, asset, dest string, limit int64, want string) error {
	base := strings.TrimRight(b.Releases, "/")
	if base == "" {
		base = CopilotReleases
	}
	what := "Copilot CLI " + version + " " + asset
	sum, err := b.get(ctx, base+"/v"+version+"/"+asset, dest, limit, sha256.New(), what, "GitHub")
	if err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(sum); got != want {
		_ = os.Remove(dest)
		return &ChecksumError{What: what, Want: want, Got: got}
	}
	return nil
}

// sumFor is the checksum the checksums file names for asset, as
// sha256:<hex>.
func sumFor(path, asset string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a file in the build's own folder
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if len(fields[0]) != 64 || strings.Trim(fields[0], "0123456789abcdef") != "" {
			return "", fmt.Errorf("SHA256SUMS.txt names %s with %q, not a SHA-256", asset, fields[0])
		}
		return "sha256:" + fields[0], nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SHA256SUMS.txt names no %s", asset)
}
