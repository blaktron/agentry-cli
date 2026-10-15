// Package harnessbuild makes the harness images that are built on this machine
// from a vendor artifact and never published. Claude Code's licence grants no
// redistribution, so each person's machine builds its own from Anthropic's npm
// packages, as `npm install -g` would lay them out, on agentry-dockerimages'
// Node base. Copilot CLI's allows redistribution only inside an application, so
// it is built the same way, from GitHub's release download, on
// agentry-dockerimages' Alpine base (copilot.go).
//
// Every download is held to the hash this build carries before anything
// reaches the daemon: one whose bytes do not hash to it is
// refused and nothing is built. The image is labelled with the artifact it
// was built from, and an image of the tag without that label, or with
// another, is built again, so the receipt's pin is true of the image that
// ran.
package harnessbuild

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The labels a built image carries: the artifact its pin names, and the
// integrity of the native package it installed for the daemon's platform.
const (
	LabelArtifact = "org.agentry.harness.artifact"
	LabelNative   = "org.agentry.harness.native"
	// LabelRecipe is the version of the Claude Code image's own recipe
	// (what this package adds around the vendor's packages): an image
	// built from the pinned artifact by an older recipe is built again.
	LabelRecipe = "org.agentry.harness.recipe"
)

// ClaudeRecipe is the Claude Code image's recipe version: 2 adds the
// stand-in browser opener, ClaudeOpener.
const ClaudeRecipe = "2"

// ClaudeOpener is the Claude Code image's browser: the program its BROWSER
// names for a sign-in. Claude Code (2.1.296) hands its opener the sign-in
// address whose redirect is its own callback on the container's loopback;
// the opener writes it to the container's stderr as one line,
// "agentry-open <address>", for the agentry CLI, which opens it in the
// person's browser and relays the callback in. It opens nothing itself.
const ClaudeOpener = "/usr/local/bin/agentry-open"

// claudeOpenerScript is ClaudeOpener. PID 1 is Claude Code (the image's
// entrypoint for a sign-in), so its stderr is the container's.
const claudeOpenerScript = `#!/bin/sh
# Claude Code's browser in Agentry's Claude Code image:
# hands the sign-in address to the agentry CLI, which opens it in the
# person's own browser. It opens nothing itself.
printf 'agentry-open %s\n' "$1" > /proc/1/fd/2
`

// ClaudeImageAdapter is the adapter whose image this package builds (the
// adapter package, which imports this one, names it too; a test there
// holds the two to one name).
const ClaudeImageAdapter = "claude-image"

// ClaudeBase is the image Claude Code is installed on: agentry-dockerimages'
// mirror of node:24-alpine, pinned by digest. Alpine is musl, so the musl
// native packages are the ones installed; the base carries the libstdc++
// and libgcc they link against, and the Node the wrapper's own install
// script runs on.
const ClaudeBase = "ghcr.io/blaktron/agentry-node-alpine:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1"

// ClaudeConfigDir is where the image keeps Claude Code's config directory:
// made by the build and owned by the image's user, so a docker volume
// first mounted there is that user's too.
const ClaudeConfigDir = "/claude"

// claudeWrapper is the npm package a Claude Code release is: a wrapper
// whose install script links the native binary of the platform's own
// package (an optional dependency at the same version) into its bin.
const claudeWrapper = "@anthropic-ai/claude-code"

// claudeRelease is a Claude Code release this build can make an image of:
// the npm integrity of the wrapper, which is the policy's artifact, and of
// the musl native package for each platform a daemon may report.
type claudeRelease struct {
	version string
	wrapper string
	native  map[string]string
}

// claudeReleases are read from the npm registry's packuments (dist.integrity)
// and checked against the downloaded tarballs. A bump adds a release here,
// moves policy.DefaultHarnessImages["claude-image"], and re-records the
// adapter's conformance.
var claudeReleases = []claudeRelease{{
	version: "2.1.296",
	wrapper: "sha512-OX/k/rpcthMqnFKOWcpNcbiLsMKtAjLOpQNJHlYiDCFUznWCIwdRTyl/p9SdHVrsrzZuPMCNqZjnpakIni6upw==",
	native: map[string]string{
		"amd64": "sha512-mbFZ0QENsyWzmDo2eWc8MxGCAWFXc2w1v2rLavu0JYd2pY7+0S8pJSB6RV9mzJ10FVXT09bXWt0sHM6fgeAz5w==",
		"arm64": "sha512-c6lZRVkvuHIHgo4SzF2yQp+RWwPDLLLjXuM6SNtJkR8EGcOR5FhLK7cmsrjDcC/t5ACP4N4pCRfRDzjC31cYQA==",
	},
}}

// npmCPU is npm's name for a daemon's architecture.
var npmCPU = map[string]string{"amd64": "x64", "arm64": "arm64"}

// maxPackage bounds one downloaded package (2.1.296's native tarballs are
// about 110 MB).
const maxPackage = 1 << 30

// Builds reports whether this build can make adapterName's image.
func Builds(adapterName string) bool {
	return adapterName == ClaudeImageAdapter || adapterName == CopilotImageAdapter
}

// PinNote is what an image this build makes for adapterName is pinned by
// and does not prove, for the receipt (spec.ImagePin.Note): "" when the
// pin says it all.
func PinNote(adapterName string) string {
	if adapterName == CopilotImageAdapter {
		return CopilotPinNote
	}
	return ""
}

// Builder makes the images this package builds.
type Builder struct {
	Docker dockerx.Client
	// HTTP fetches the packages; nil is a plain client, bounded by ctx.
	HTTP *http.Client
	// Registry is the npm registry; "" is policy.DefaultNPMRegistry.
	Registry string
	// Releases is where GitHub's release downloads are; ""
	// is CopilotReleases.
	Releases string
	// Dir is where the build context is made and removed; "" is the
	// system's temporary directory.
	Dir string
	// Log is told what is built from what.
	Log io.Writer
}

// IntegrityError is a package whose bytes are not the ones this build
// pins: the build is refused and nothing reaches the daemon.
type IntegrityError struct {
	Package, Want, Got string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("refused: the registry's %s is not the package this build of agentry pins (npm integrity %s, and it hashes to %s); nothing was built", e.Package, e.Want, e.Got)
}

// Ensure makes adapterName's image h present: nothing when an image of
// h.Ref carries h's artifact, else the build.
func (b Builder) Ensure(ctx context.Context, adapterName string, h policy.HarnessImage) error {
	if !Builds(adapterName) {
		return fmt.Errorf("this build of agentry cannot build the %s harness image", adapterName)
	}
	var build func() error
	switch adapterName {
	case CopilotImageAdapter:
		rel, err := copilotReleaseFor(h)
		if err != nil {
			return err
		}
		build = func() error { return b.buildCopilot(ctx, h, rel) }
	default:
		rel, err := claudeReleaseFor(h)
		if err != nil {
			return err
		}
		build = func() error { return b.buildClaude(ctx, h, rel) }
	}
	ok, err := b.Docker.ImageExists(ctx, h.Ref)
	if err != nil {
		return err
	}
	if ok {
		got, err := b.Docker.ImageLabel(ctx, h.Ref, LabelArtifact)
		if err != nil {
			return err
		}
		switch {
		case got != h.Artifact:
			b.logf("%s is here but was not built from %s; building it again\n", h.Ref, h.Artifact)
		case adapterName != ClaudeImageAdapter:
			return nil
		default:
			recipe, err := b.Docker.ImageLabel(ctx, h.Ref, LabelRecipe)
			if err != nil {
				return err
			}
			if recipe == ClaudeRecipe {
				return nil
			}
			b.logf("%s was built by an older recipe of this image; building it again\n", h.Ref)
		}
	}
	return build()
}

// claudeReleaseFor is the release h's artifact names, whose version h's
// tag must be, so the image's name never says another version than it
// holds.
func claudeReleaseFor(h policy.HarnessImage) (claudeRelease, error) {
	known := make([]string, 0, len(claudeReleases))
	for _, r := range claudeReleases {
		known = append(known, r.version)
		if r.wrapper != h.Artifact {
			continue
		}
		if !strings.HasSuffix(h.Ref, ":"+r.version) {
			return claudeRelease{}, fmt.Errorf("the claude-image harness image %s is pinned to Claude Code %s's npm integrity, so its tag must be %s", h.Ref, r.version, r.version)
		}
		return r, nil
	}
	return claudeRelease{}, fmt.Errorf("this build of agentry knows no Claude Code release whose npm integrity is %s (it can build %s): remove images.harnessImages.claude-image from the policy for the default",
		h.Artifact, strings.Join(known, ", "))
}

// buildClaude fetches and checks the wrapper and the daemon's native
// package, then builds the image from them alone: the build reaches no
// registry but the base's.
func (b Builder) buildClaude(ctx context.Context, h policy.HarnessImage, rel claudeRelease) error {
	arch, err := b.Docker.ServerArch(ctx)
	if err != nil {
		return err
	}
	cpu, native := npmCPU[arch], rel.native[arch]
	if cpu == "" || native == "" {
		return fmt.Errorf("no Claude Code %s image for this docker daemon's platform (linux/%s); it builds for linux/amd64 and linux/arm64", rel.version, arch)
	}
	nativePkg := claudeWrapper + "-linux-" + cpu + "-musl"
	dir, err := os.MkdirTemp(b.Dir, "agentry-claude-code-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	wrapper := filepath.Join(dir, "claude-code.tgz")
	if err := b.fetch(ctx, claudeWrapper, rel.version, rel.wrapper, wrapper); err != nil {
		return err
	}
	if err := checkWrapper(wrapper, rel.version, nativePkg); err != nil {
		return err
	}
	if err := b.fetch(ctx, nativePkg, rel.version, native, filepath.Join(dir, "claude-code-native.tgz")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(claudeDockerfile), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "agentry-open"), []byte(claudeOpenerScript), 0o600); err != nil {
		return err
	}
	b.logf("building %s from %s@%s and %s@%s, each checked against its npm integrity, on %s (first use)\n",
		h.Ref, claudeWrapper, rel.version, nativePkg, rel.version, ClaudeBase)
	err = b.Docker.Build(ctx, dir, h.Ref, map[string]string{
		"BASE":             ClaudeBase,
		"NATIVE":           strings.TrimPrefix(nativePkg, "@anthropic-ai/"),
		"ARTIFACT":         h.Artifact,
		"NATIVE_INTEGRITY": native,
		"RECIPE":           ClaudeRecipe,
	})
	if err != nil {
		return fmt.Errorf("build %s: %w", h.Ref, err)
	}
	return nil
}

// claudeDockerfile installs the two packages where `npm install -g` puts
// them and runs the wrapper's own install script, which links the native
// binary over the wrapper's bin (unmodified: Claude Code "must be
// installed and run as published"). The unpacking is a stage of its own,
// so the image holds the packages and not their tarballs. Claude Code is
// run once as a check that it starts on the base; its config directory is
// made for the image's user.
var claudeDockerfile = `ARG BASE
FROM ${BASE} AS unpack
` + imagelabel.Line(imagelabel.ClaudeCode) + `ARG NATIVE
COPY claude-code.tgz claude-code-native.tgz /tmp/
RUN set -e; m=/usr/local/lib/node_modules/@anthropic-ai; \
    mkdir -p "$m/claude-code" "$m/$NATIVE"; \
    tar -xzf /tmp/claude-code.tgz -C "$m/claude-code" --strip-components=1; \
    tar -xzf /tmp/claude-code-native.tgz -C "$m/$NATIVE" --strip-components=1; \
    cd "$m/claude-code" && node install.cjs

FROM ${BASE}
ARG ARTIFACT
ARG NATIVE_INTEGRITY
ARG RECIPE
LABEL org.agentry.harness.artifact="${ARTIFACT}" org.agentry.harness.native="${NATIVE_INTEGRITY}" org.agentry.harness.recipe="${RECIPE}"
` + imagelabel.Line(imagelabel.ClaudeCode) + `COPY --from=unpack /usr/local/lib/node_modules/@anthropic-ai/ /usr/local/lib/node_modules/@anthropic-ai/
COPY agentry-open ` + ClaudeOpener + `
RUN set -e; ln -s ../lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe /usr/local/bin/claude; \
    chmod 0755 ` + ClaudeOpener + `; \
    DISABLE_AUTOUPDATER=1 CLAUDE_CONFIG_DIR=/tmp/claude-check claude --version; rm -rf /tmp/claude-check; \
    mkdir -m 0700 ` + ClaudeConfigDir + ` && chown node:node ` + ClaudeConfigDir + `
ENV DISABLE_AUTOUPDATER=1 CLAUDE_CONFIG_DIR=` + ClaudeConfigDir + `
USER node
WORKDIR /home/node
`

// fetch downloads name@version's tarball into dest and holds it to
// integrity, removing it when it does not match.
func (b Builder) fetch(ctx context.Context, name, version, integrity, dest string) error {
	reg := strings.TrimRight(b.Registry, "/")
	if reg == "" {
		reg = policy.DefaultNPMRegistry
	}
	url := reg + "/" + name + "/-/" + name[strings.LastIndex(name, "/")+1:] + "-" + version + ".tgz"
	sum, err := b.get(ctx, url, dest, maxPackage, sha512.New(), name+"@"+version, "the registry")
	if err != nil {
		return err
	}
	if got := "sha512-" + base64.StdEncoding.EncodeToString(sum); got != integrity {
		_ = os.Remove(dest)
		return &IntegrityError{Package: name + "@" + version, Want: integrity, Got: got}
	}
	return nil
}

// get downloads url into dest, at most limit bytes, and returns its hash
// by h; dest is removed when the download fails. what and who name the
// download and its server in errors.
func (b Builder) get(ctx context.Context, url, dest string, limit int64, h hash.Hash, what, who string) (sum []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	hc := b.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s answered %s", what, who, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a file in the build's own folder
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dest)
		}
	}()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", what, err)
	}
	if n > limit {
		return nil, fmt.Errorf("fetch %s: larger than %d bytes", what, limit)
	}
	return h.Sum(nil), nil
}

// checkWrapper holds the wrapper's package.json to the release: its name
// and version, and the native package it installs at that version, so the
// native package this build pins is the one this release installs.
func checkWrapper(tgz, version, nativePkg string) error {
	f, err := os.Open(tgz) //nolint:gosec // a file in the build's own folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s@%s: %w", claudeWrapper, version, err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s@%s has no package.json", claudeWrapper, version)
		}
		if err != nil {
			return fmt.Errorf("%s@%s: %w", claudeWrapper, version, err)
		}
		if hdr.Name != "package/package.json" {
			continue
		}
		var pj struct {
			Name     string            `json:"name"`
			Version  string            `json:"version"`
			Optional map[string]string `json:"optionalDependencies"`
		}
		if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&pj); err != nil {
			return fmt.Errorf("%s@%s package.json: %w", claudeWrapper, version, err)
		}
		if pj.Name != claudeWrapper || pj.Version != version || pj.Optional[nativePkg] != version {
			return fmt.Errorf("%s@%s's package.json is %s@%s and installs %s@%q, not this release's", claudeWrapper, version, pj.Name, pj.Version, nativePkg, pj.Optional[nativePkg])
		}
		return nil
	}
}

func (b Builder) logf(format string, args ...any) {
	if b.Log != nil {
		_, _ = fmt.Fprintf(b.Log, format, args...)
	}
}
