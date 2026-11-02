package harnessbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A harness image whose vendor's newest release is newer than the one this
// build pins is built from that newest release first. The newest release is
// held to the vendor's own signature in place of a hash this build carries:
// Claude Code's two npm packages to the integrities the npm registry signs
// (npmsig.go), Codex's binary to OpenAI's keyless signature from its release
// workflow at the release's tag and commit (sigstore.go). Each is held to
// that before the daemon is touched, and the image runs the binary once
// before it is used. A newest release that fails any step is not used: the
// tested pin is built or pulled as it always was.

// Newest is a vendor release newer than the one this build pins, as its
// vendor answers for it, ready to be built.
type Newest struct {
	Adapter string
	Version string
	// Tested is the version this build pins and tested.
	Tested string
	claude *claudeRelease
	codex  *codexRelease
}

// claudeRecord is a Claude Code release as the record of the newest images
// keeps it: the integrities the registry signed.
type claudeRecord struct {
	Version string            `json:"version"`
	Wrapper string            `json:"wrapper"`
	Native  map[string]string `json:"native"`
}

// TestedVersion is the version of the release this build pins for
// adapterName and tested: "" for an adapter whose newest release is never
// tried.
func TestedVersion(adapterName string) string {
	if !policy.TriesNewest(adapterName) {
		return ""
	}
	h := policy.DefaultHarnessImages[adapterName]
	if adapterName == ClaudeImageAdapter {
		if r, err := claudeReleaseFor(h); err == nil {
			return r.version
		}
		return ""
	}
	// The published Codex image's tag is <version>-<build date>.<run>.
	ref := h.Ref
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	tag := ref[strings.LastIndex(ref, ":")+1:]
	v, _, _ := strings.Cut(tag, "-")
	return v
}

// AskNewest asks adapterName's vendor for its newest release, and returns
// it when it is newer than the tested one, ok false when it is not.
func (b Builder) AskNewest(ctx context.Context, adapterName string) (n Newest, ok bool, err error) {
	tested := TestedVersion(adapterName)
	if tested == "" {
		return Newest{}, false, fmt.Errorf("this build of agentry does not build the newest %s image", adapterName)
	}
	n = Newest{Adapter: adapterName, Tested: tested}
	switch adapterName {
	case ClaudeImageAdapter:
		rel, err := b.newestClaude(ctx)
		if err != nil {
			return Newest{}, false, err
		}
		n.Version, n.claude = rel.version, &rel
	default:
		rel, err := b.newestCodex(ctx)
		if err != nil {
			return Newest{}, false, err
		}
		n.Version, n.codex = rel.Version, &rel
	}
	return n, newerVersion(n.Version, tested), nil
}

// BuildNewest builds n's image, checking everything the release is held
// to, and returns the image and the record entry that builds it again.
func (b Builder) BuildNewest(ctx context.Context, n Newest) (policy.HarnessImage, policy.Newest, error) {
	switch {
	case n.claude != nil:
		h := policy.HarnessImage{Ref: claudeRef(n.claude.version), Artifact: n.claude.wrapper}
		if err := b.buildClaude(ctx, h, *n.claude); err != nil {
			return policy.HarnessImage{}, policy.Newest{}, err
		}
		raw, err := json.Marshal(claudeRecord{Version: n.claude.version, Wrapper: n.claude.wrapper, Native: n.claude.native})
		if err != nil {
			return policy.HarnessImage{}, policy.Newest{}, err
		}
		return h, policy.Newest{Ref: h.Ref, Artifact: h.Artifact, Version: n.Version, Release: raw}, nil
	case n.codex != nil:
		h, rel, err := b.buildCodex(ctx, *n.codex)
		if err != nil {
			return policy.HarnessImage{}, policy.Newest{}, err
		}
		raw, err := json.Marshal(rel)
		if err != nil {
			return policy.HarnessImage{}, policy.Newest{}, err
		}
		return h, policy.Newest{Ref: h.Ref, Artifact: h.Artifact, Version: n.Version, Release: raw}, nil
	}
	return policy.HarnessImage{}, policy.Newest{}, fmt.Errorf("no release to build for %s", n.Adapter)
}

// recordedClaude is the Claude Code release the record names for h, ok
// false when it names none.
func (b Builder) recordedClaude(h policy.HarnessImage) (claudeRelease, bool) {
	if b.Recorded == nil || b.Recorded.Artifact != h.Artifact || b.Recorded.Ref != h.Ref {
		return claudeRelease{}, false
	}
	var r claudeRecord
	if err := json.Unmarshal(b.Recorded.Release, &r); err != nil || r.Wrapper != h.Artifact || h.Ref != claudeRef(r.Version) {
		return claudeRelease{}, false
	}
	return claudeRelease{version: r.Version, wrapper: r.Wrapper, native: r.Native}, true
}

// recordedCodex is the Codex release the record names for h.
func (b Builder) recordedCodex(h policy.HarnessImage) (codexRelease, bool) {
	if b.Recorded == nil || b.Recorded.Artifact != h.Artifact || b.Recorded.Ref != h.Ref {
		return codexRelease{}, false
	}
	var r codexRelease
	if err := json.Unmarshal(b.Recorded.Release, &r); err != nil || h.Ref != codexRef(r.Version) {
		return codexRelease{}, false
	}
	for _, sum := range r.Binary {
		if "sha256:"+sum == h.Artifact {
			return r, true
		}
	}
	return codexRelease{}, false
}

// claudeRef is the local tag of the Claude Code image of version, as the
// default pin's is.
func claudeRef(version string) string {
	ref := policy.DefaultHarnessImages[ClaudeImageAdapter].Ref
	return ref[:strings.LastIndex(ref, ":")+1] + version
}

// npmManifest is the part of a version's manifest the newest Claude Code
// is read from.
type npmManifest struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Optional map[string]string `json:"optionalDependencies"`
	Dist     struct {
		Integrity  string         `json:"integrity"`
		Signatures []npmSignature `json:"signatures"`
	} `json:"dist"`
}

var semverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// newestClaude reads the npm registry's latest Claude Code and the musl
// native package it installs for the daemon's platform, each held to the
// registry's signature over its integrity.
func (b Builder) newestClaude(ctx context.Context) (claudeRelease, error) {
	arch, err := b.Docker.ServerArch(ctx)
	if err != nil {
		return claudeRelease{}, err
	}
	cpu := npmCPU[arch]
	if cpu == "" {
		return claudeRelease{}, fmt.Errorf("no Claude Code image for this docker daemon's platform (linux/%s); it builds for linux/amd64 and linux/arm64", arch)
	}
	reg := strings.TrimRight(b.Registry, "/")
	if reg == "" {
		reg = policy.DefaultNPMRegistry
	}
	var w npmManifest
	if err := b.getJSON(ctx, reg+"/"+claudeWrapper+"/latest", &w, "Claude Code's newest release", "the npm registry"); err != nil {
		return claudeRelease{}, err
	}
	if w.Name != claudeWrapper || !semverRE.MatchString(w.Version) {
		return claudeRelease{}, fmt.Errorf("the npm registry's newest %s is %s@%s, not a release", claudeWrapper, w.Name, w.Version)
	}
	if err := checkNPMSignature(npmKeys, w.Name, w.Version, w.Dist.Integrity, w.Dist.Signatures); err != nil {
		return claudeRelease{}, err
	}
	nativePkg := claudeWrapper + "-linux-" + cpu + "-musl"
	if w.Optional[nativePkg] != w.Version {
		return claudeRelease{}, fmt.Errorf("%s@%s installs %s@%q, not that version", claudeWrapper, w.Version, nativePkg, w.Optional[nativePkg])
	}
	var nat npmManifest
	if err := b.getJSON(ctx, reg+"/"+nativePkg+"/"+w.Version, &nat, nativePkg+"@"+w.Version, "the npm registry"); err != nil {
		return claudeRelease{}, err
	}
	if nat.Name != nativePkg || nat.Version != w.Version {
		return claudeRelease{}, fmt.Errorf("the npm registry answered %s@%s for %s@%s", nat.Name, nat.Version, nativePkg, w.Version)
	}
	if err := checkNPMSignature(npmKeys, nat.Name, nat.Version, nat.Dist.Integrity, nat.Dist.Signatures); err != nil {
		return claudeRelease{}, err
	}
	return claudeRelease{version: w.Version, wrapper: w.Dist.Integrity, native: map[string]string{arch: nat.Dist.Integrity}}, nil
}

// newerVersion reports whether version a is newer than b, both
// <major>.<minor>.<patch>.
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	if len(pa) != 3 || len(pb) != 3 {
		return false
	}
	for i := range 3 {
		x, err1 := strconv.Atoi(pa[i])
		y, err2 := strconv.Atoi(pb[i])
		if err1 != nil || err2 != nil {
			return false
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// NewestNote is what a receipt says of an image built from a vendor's
// newest release, "" for any other image.
func NewestNote(adapterName string, h policy.HarnessImage) string {
	if !policy.TriesNewest(adapterName) || h.Artifact == "" {
		return ""
	}
	if adapterName == ClaudeImageAdapter {
		if _, err := claudeReleaseFor(h); err == nil {
			return ""
		}
	}
	version := h.Ref[strings.LastIndex(h.Ref, ":")+1:]
	return fmt.Sprintf("built from the vendor's newest release, %s, newer than the release this build of agentry tested (%s); held to the vendor's signature, not to a hash this build carries",
		version, TestedVersion(adapterName))
}
