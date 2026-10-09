package policy

// Model sign-ins and per-adapter harness images (plan model-sign-ins.md
// §4.2, D3–D5, D13; agentry-cli#738, DR-86). A model sign-in is a person's
// own grant that lets their local runs spend a model vendor's plan: a
// ChatGPT plan, a Claude subscription through Claude Code, a Copilot plan.
// It is a credential kind beside DR-26's api and subscription, and the
// policy names only the kind: the grant lives in the person's model
// sign-in store (pkg/runner/modelsignin), never in this file, a plan or a
// receipt.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The model sign-in kinds, and the providers they belong to.
const (
	// KeyKindChatGPTPlan spends a ChatGPT plan through OpenAI's "Sign in
	// with ChatGPT" plan usage: the run is handed a short-lived access
	// token in a read-only file, rewritten before it expires.
	KeyKindChatGPTPlan = "chatgpt-plan"
	// KeyKindClaudeSignIn spends a Claude subscription through Claude
	// Code's own sign-in, kept in a config directory Agentry owns and
	// never reads (plan D19, D20).
	KeyKindClaudeSignIn = "claude-signin"
	// KeyKindCopilotSignIn spends a Copilot plan through a GitHub sign-in.
	KeyKindCopilotSignIn = "copilot-signin"

	ProviderOpenAI  = "openai"
	ProviderCopilot = "copilot"
)

// DockerAgentAdapter is the default adapter's name, whose image is
// images.harnessDockerAgent (the adapter package names it too; it imports
// this one).
const DockerAgentAdapter = "docker-agent"

// The token routes of HarnessPolicy.SignInTokenRoute.
const (
	TokenRouteAuthCommand = "auth-command"
	TokenRouteRestart     = "restart"
)

// SignInTokenRoute is the token route hp names, the default when it names
// none.
func SignInTokenRoute(hp HarnessPolicy) string {
	if hp.SignInTokenRoute == "" {
		return TokenRouteAuthCommand
	}
	return hp.SignInTokenRoute
}

// signInEndpoints are the endpoints a run paid by each token-file sign-in
// kind may reach through the egress proxy, and nothing else (invariant
// I2): the vendor's inference API alone, never its token endpoint, which
// only the CLI talks to.
var signInEndpoints = map[string][]string{
	KeyKindChatGPTPlan: {"api.openai.com:443"},
}

// SignInProxyAllow is the proxy allowlist a run paid by kind must have,
// nil for a kind with no fixed endpoint (a key; a sign-in whose harness is
// not built yet).
func SignInProxyAllow(kind string) []string { return slices.Clone(signInEndpoints[kind]) }

// signInProviders maps each model sign-in kind to the one provider it
// spends.
var signInProviders = map[string]string{
	KeyKindChatGPTPlan:   ProviderOpenAI,
	KeyKindClaudeSignIn:  ProviderAnthropic,
	KeyKindCopilotSignIn: ProviderCopilot,
}

// IsModelSignIn reports whether kind is a model sign-in: a plan the person
// holds, which a hosted run never uses (plan D2) and which reports tokens,
// never dollars (D13).
func IsModelSignIn(kind string) bool {
	_, ok := signInProviders[kind]
	return ok
}

// SignInProvider is the provider a model sign-in kind spends, "" for any
// other kind.
func SignInProvider(kind string) string { return signInProviders[kind] }

// SignInSentence is what every surface says when a model sign-in meets
// something only a key can do, for kind.
func SignInSentence(kind string) string {
	switch kind {
	case KeyKindChatGPTPlan:
		return "a ChatGPT plan sign-in"
	case KeyKindClaudeSignIn:
		return "a Claude subscription through Claude Code"
	case KeyKindCopilotSignIn:
		return "a Copilot plan sign-in"
	}
	return "a model sign-in"
}

// MaxCostSignInSentence is why a cost budget is refused for a run paid by
// a model sign-in (plan D13): the plan pays, so the run reports tokens.
const MaxCostSignInSentence = "a run paid by a model sign-in spends the person's plan and reports tokens, not dollars, so a cost budget has nothing to measure; set maxCostUsd to 0 (or leave --max-cost off) and bound the run with --max-turns and --timeout"

// checkKind holds one apiKeyKind against the provider it is set beside:
// field names the key for the error.
func checkKind(field, kind, provider string) error {
	if !validKeyKind(kind) {
		return fmt.Errorf("%s %q must be api, subscription, %s, %s or %s", field, kind, KeyKindChatGPTPlan, KeyKindClaudeSignIn, KeyKindCopilotSignIn)
	}
	if want := SignInProvider(kind); want != "" && provider != want {
		return fmt.Errorf("%s %s needs model.provider %s (it is %q): %s spends that provider's plan alone", field, kind, want, provider, SignInSentence(kind))
	}
	return nil
}

// HarnessImage pins one adapter's harness image (agentry-cli#224's option
// A, widened by plan D5): either a published image pinned by digest, or an
// image built on this machine from a vendor artifact, pinned by that
// artifact's hash. Never a bare tag.
type HarnessImage struct {
	// Ref is the image reference: <name>[:<tag>]@sha256:<digest> for a
	// published image, the local <name>:<tag> the build tags for a built
	// one.
	Ref string `yaml:"ref"`
	// Artifact is sha256:<64 hex> of the vendor artifact a built image is
	// made from (an npm package's integrity, a release download's
	// checksum); empty for a published image.
	Artifact string `yaml:"artifact,omitempty"`
}

// The ways a harness image is pinned, as plans and receipts name them.
const (
	PinDigest   = "digest"
	PinArtifact = "artifact"
	// PinTag is a harness image named by tag alone: only
	// images.harnessDockerAgent may be, for a policy file written before
	// the default was pinned. The receipt says so.
	PinTag = "tag"
)

// artifactRE is a vendor artifact's hash as a pin carries it.
var artifactRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// localImageRE is the name of an image built on this machine: lowercase,
// no registry host (no dot or port before the first slash), a tag.
var localImageRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// namesRegistry reports whether ref's first path element is a registry
// host (it holds a dot or a port, or is localhost), as docker reads it.
func namesRegistry(ref string) bool {
	first, _, found := strings.Cut(ref, "/")
	return found && (strings.ContainsAny(first, ".:") || first == "localhost")
}

// Pin says how h is pinned and by what: the digest of a published image,
// the artifact hash of a built one, or the tag alone.
func (h HarnessImage) Pin() (by, value string) {
	if h.Artifact != "" {
		return PinArtifact, h.Artifact
	}
	if PinnedByDigest(h.Ref) {
		return PinDigest, h.Ref[strings.LastIndex(h.Ref, "@")+1:]
	}
	return PinTag, ""
}

// validate holds an images.harnessImages entry to its pin.
func (h HarnessImage) validate(adapterName string) error {
	field := "images.harnessImages." + adapterName
	switch {
	case h.Artifact != "":
		if !artifactRE.MatchString(h.Artifact) {
			return fmt.Errorf("%s.artifact %q must be sha256:<64 hex>, the hash of the vendor artifact the image is built from", field, h.Artifact)
		}
		if !localImageRE.MatchString(h.Ref) || namesRegistry(h.Ref) {
			return fmt.Errorf("%s.ref %q must be a local <name>:<tag> (an image built on this machine is never pulled)", field, h.Ref)
		}
	case !PinnedByDigest(h.Ref):
		return fmt.Errorf("%s.ref %q must be pinned by digest (<name>@sha256:<64 hex>), or carry the artifact it is built from: a harness image is never named by a tag alone", field, h.Ref)
	}
	return nil
}

// DefaultHarnessImages are the CLI's pinned harness images for the
// adapters beyond docker-agent, by adapter name, each by digest (I8).
//
// codex-image: agentry-dockerimages' Codex harness image (#53), OpenAI's
// Codex CLI 0.162.0 release binary verified against OpenAI's signature,
// amd64 and arm64, signed keyless by that repository's codex workflow on
// main (agentry-cli's CI verifies this digest against that identity,
// scripts/verify-codex-image.sh). A Codex bump re-records the adapter's
// conformance before this pin moves (README "Bumping Codex" there).
var DefaultHarnessImages = map[string]HarnessImage{
	"codex-image": {Ref: "ghcr.io/blaktron/agentry-codex:0.162.0-2026.10.09.1@sha256:e6b0c020b797d7941b4365f82e743f190f82342f087621bddf57b087e4d703f9"},
}

// HarnessImageFor is the harness image adapterName runs in: docker-agent's
// from images.harnessDockerAgent, any other's from images.harnessImages,
// else the CLI's default. ok is false for an adapter with none (a host
// adapter, which drives the person's own installation).
func (i Images) HarnessImageFor(adapterName string) (h HarnessImage, ok bool) {
	if adapterName == DockerAgentAdapter {
		return HarnessImage{Ref: i.HarnessDockerAgent}, i.HarnessDockerAgent != ""
	}
	if set, found := i.HarnessImages[adapterName]; found {
		return set, true
	}
	h, ok = DefaultHarnessImages[adapterName]
	return h, ok
}
