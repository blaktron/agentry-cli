package policy

// Model sign-ins and per-adapter harness images. A model sign-in is a person's
// own grant that lets their local runs spend a model vendor's plan: a
// ChatGPT plan, a Claude subscription through Claude Code, a Copilot plan.
// It is a credential kind beside the api and subscription kinds, and the
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
	// never reads.
	KeyKindClaudeSignIn = "claude-signin"
	// KeyKindCopilotSignIn spends a Copilot plan through a GitHub sign-in.
	KeyKindCopilotSignIn = "copilot-signin"

	ProviderOpenAI  = "openai"
	ProviderCopilot = "copilot"

	// The providers whose account a person may connect:
	// each is paid by an ordinary API key (or, Hugging Face, an inference
	// token), pasted or minted by a connect.
	ProviderOpenRouter  = "openrouter"
	ProviderPoe         = "poe"
	ProviderHuggingFace = "huggingface"
)

// DockerAgentAdapter is the default adapter's name, whose image is
// images.harnessImages.docker-agent (the adapter package names it too; it
// imports this one).
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

// signInEndpoints are the endpoints a run paid by each sign-in kind may
// reach through the egress proxy, and nothing else. For a
// token-file sign-in, the vendor's inference API alone, never its token
// endpoint, which only the CLI talks to. Claude Code keeps and refreshes
// its own sign-in in its directory, so a Claude run reaches
// Anthropic's API and the host its refresh goes to (platform.claude.com,
// Claude Code's TOKEN_URL in 2.1.296), and nothing of claude.ai.
var signInEndpoints = map[string][]string{
	KeyKindChatGPTPlan:  {"api.openai.com:443"},
	KeyKindClaudeSignIn: {"api.anthropic.com:443", "platform.claude.com:443"},
}

// CopilotGitHubAPI is GitHub's API, where Copilot CLI reads the account's
// entitlement and its organization's managed settings: one of the two
// hosts a Copilot run reaches.
const CopilotGitHubAPI = "api.github.com:443"

// The Copilot plans, by the Copilot API host each is served from, the other
// host a Copilot run reaches (GitHub's Copilot allowlist reference:
// *.individual, *.business and *.enterprise.githubcopilot.com). Copilot's
// telemetry and experiment hosts are hosts of their own, so the proxy denies
// them. Which host each plan uses is read from GitHub's docs, not measured.
const (
	CopilotPlanIndividual = "individual"
	CopilotPlanBusiness   = "business"
	CopilotPlanEnterprise = "enterprise"
)

var copilotAPIHosts = map[string]string{
	CopilotPlanIndividual: "api.individual.githubcopilot.com:443",
	CopilotPlanBusiness:   "api.business.githubcopilot.com:443",
	CopilotPlanEnterprise: "api.enterprise.githubcopilot.com:443",
}

// CopilotPlans lists the Copilot plans by the name setup takes: individual
// (Free, Pro, Pro+ and Max), business and enterprise.
func CopilotPlans() []string {
	return []string{CopilotPlanIndividual, CopilotPlanBusiness, CopilotPlanEnterprise}
}

// CopilotProxyAllow is the proxy allowlist of a Copilot run on plan:
// GitHub's API and that plan's Copilot API host.
func CopilotProxyAllow(plan string) ([]string, error) {
	host, ok := copilotAPIHosts[plan]
	if !ok {
		return nil, fmt.Errorf("no Copilot plan %q (one of %s)", plan, strings.Join(CopilotPlans(), ", "))
	}
	return []string{CopilotGitHubAPI, host}, nil
}

// CopilotPlanOf is the plan a Copilot run's allowlist names, "" when it is
// not one CopilotProxyAllow makes.
func CopilotPlanOf(allow []string) string {
	for _, plan := range CopilotPlans() {
		if want, _ := CopilotProxyAllow(plan); slices.Equal(allow, want) {
			return plan
		}
	}
	return ""
}

// SignInProxyAllow is the proxy allowlist a run paid by kind has by
// default (a Copilot run's on the individual plans), nil for a kind with no
// fixed endpoint (a key).
func SignInProxyAllow(kind string) []string {
	if kind == KeyKindCopilotSignIn {
		allow, _ := CopilotProxyAllow(CopilotPlanIndividual)
		return allow
	}
	return slices.Clone(signInEndpoints[kind])
}

// SignInProxyAllowed reports whether allow is an allowlist a run paid by
// kind may have, and says what it must be when it is not: exactly the
// kind's endpoints, or for Copilot GitHub's API and one plan's Copilot API
// host. Any allowlist is allowed for a kind with no fixed endpoint.
func SignInProxyAllowed(kind string, allow []string) (ok bool, want string) {
	switch {
	case kind == KeyKindCopilotSignIn:
		if CopilotPlanOf(allow) != "" {
			return true, ""
		}
		var hosts []string
		for _, plan := range CopilotPlans() {
			hosts = append(hosts, copilotAPIHosts[plan]+" ("+plan+")")
		}
		return false, CopilotGitHubAPI + " and one plan's Copilot API host: " + strings.Join(hosts, ", ")
	case signInEndpoints[kind] == nil:
		return true, ""
	}
	return slices.Equal(allow, signInEndpoints[kind]), strings.Join(signInEndpoints[kind], ", ")
}

// signInProviders maps each model sign-in kind to the one provider it
// spends.
var signInProviders = map[string]string{
	KeyKindChatGPTPlan:   ProviderOpenAI,
	KeyKindClaudeSignIn:  ProviderAnthropic,
	KeyKindCopilotSignIn: ProviderCopilot,
}

// IsModelSignIn reports whether kind is a model sign-in: a plan the person
// holds, which a hosted run never uses and which reports tokens,
// never dollars.
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
// a model sign-in: the plan pays, so the run reports tokens.
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

// HarnessImage pins one adapter's harness image: either a published image
// pinned by digest, or an image built on this machine from a vendor artifact,
// pinned by that artifact's hash. Never a bare tag.
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
	// PinTag is a harness image named by tag alone: only docker-agent's
	// may be, for a policy file written before the default was pinned. The
	// receipt says so.
	PinTag = "tag"
)

// artifactRE is a vendor artifact's hash as a pin carries it: sha256 in
// hex (a release download's checksum), or an npm package's integrity as
// the registry gives it, sha512 in base64 (Subresource Integrity).
var artifactRE = regexp.MustCompile(`^(sha256:[0-9a-f]{64}|sha512-[A-Za-z0-9+/]{86}==)$`)

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
			return fmt.Errorf("%s.artifact %q must be sha256:<64 hex> or an npm integrity (sha512-<base64>), the hash of the vendor artifact the image is built from", field, h.Artifact)
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
// adapters beyond docker-agent, by adapter name, each by digest or by the
// artifact it is built from.
//
// codex-image: agentry-dockerimages' Codex harness image, OpenAI's
// Codex CLI 0.162.0 release binary verified against OpenAI's signature,
// amd64 and arm64, signed keyless by that repository's codex workflow on
// main (agentry-cli's CI verifies this digest against that identity,
// scripts/verify-codex-image.sh). A Codex bump re-records the adapter's
// conformance before this pin moves (README "Bumping Codex" there).
//
// claude-image: Claude Code built on this machine, never published: the
// artifact is the npm integrity of @anthropic-ai/claude-code at the tag's
// version, and the build (pkg/runner/harnessbuild) holds the native package it
// installs to the integrity it carries for that release. A bump adds the
// release there and re-records the adapter's conformance before this pin moves.
//
// copilot-image: GitHub's Copilot CLI built on this machine, never
// published: the artifact is the SHA-256 of the release's
// SHA256SUMS.txt, which names each platform's musl download's checksum;
// GitHub signs neither, so the receipt says the image is pinned by
// checksum, not signed. The build (pkg/runner/harnessbuild) fetches that
// file, holds it to the artifact, and holds the daemon's platform's
// download to the checksum it names. A bump adds the release there and
// re-records the adapter's conformance before this pin moves.
var DefaultHarnessImages = map[string]HarnessImage{
	"copilot-image": {Ref: "agentry-copilot:1.0.95", Artifact: "sha256:fd36c0e57ae6285ccd9cff306fbcdf114343c44f968ff2364fa7fd1a4dc4b7bf"},
	"codex-image":   {Ref: "ghcr.io/blaktron/agentry-codex:0.162.0-2026.10.22.2@sha256:020effde1c2c7b5b3aa2b72e6864c1ebb5a64fcb78cc96f9ee2a6f61bc1d0d14"},
	"claude-image":  {Ref: "agentry-claude-code:2.1.296", Artifact: "sha512-OX/k/rpcthMqnFKOWcpNcbiLsMKtAjLOpQNJHlYiDCFUznWCIwdRTyl/p9SdHVrsrzZuPMCNqZjnpakIni6upw=="},
}

// HarnessImageFor is the harness image adapterName runs in, from
// images.harnessImages (docker-agent's as Load resolved it into
// HarnessDockerAgent), else the CLI's default. ok is false for an adapter with none (a host
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
