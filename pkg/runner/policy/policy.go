// Package policy loads the operator policy (RUNNER-PLAN §R9) — the
// machine-side half of every run plan. The manifest says what an agent
// wants; the policy says what this operator's machine will provide: image
// pins, the model endpoint and its egress allowlist, whether wildcard
// egress is ever runnable, and where secret values come from. The policy
// participates in the plan hash, so changing it invalidates installed
// plans instead of silently changing what they mean.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/dirpublish"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/websearch"
)

// runtimeNameRE is the shape of a daemon-registered runtime name (runc,
// runsc, io.containerd.runc.v2): something `--runtime` accepts verbatim.
var runtimeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Version is the current policy schema major.
const Version = "1"

// Default image pins. These are Agentry's own mirrors, published from
// blaktron/agentry-dockerimages: each one's destination digest was verified
// equal to the upstream digest it mirrors, so the bytes are what the upstream
// tag named when it was pinned.
//
// They are tags, and that is a real limit. The tags are ours, so they cannot
// move without us — but nothing enforces that: a push by us, by anyone holding
// a token that can write to the namespace, or by a mistaken re-mirror would
// move one, and a run would not be able to tell. Say so rather than claiming
// immutability.
//
// The exception is DefaultGoBuilder, one of the two bases of the runner image,
// which is pinned by digest (name:tag@sha256:…, the digest from images.tsv)
// since agentry-cli#269 closed DR-32's deferral: the image is built, not
// pulled, so the ref only ever reaches a Dockerfile FROM, and a live build
// proved the digest form there. The runtime base is pinned the same way in
// runnerimage/Dockerfile. The pulled images (the gateway, the harness, the
// two MCP runtimes) are still tags: every digest is in images.tsv, but the
// digest form changes the ref the runner hands the daemon to pull and to
// probe, and that wants its own live acceptance run (agentry-cli#236).
//
// Versions match the survey of 2026-08-30, re-verified 2026-09-23.
const (
	DefaultGatewayImage = "ghcr.io/blaktron/agentry-mcp-gateway:v0.43.3"
	DefaultHarnessImage = "ghcr.io/blaktron/agentry-docker-agent:1.128.0"
	DefaultGoBuilder    = "ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414"
	// Runtime images that install and run declared npm/PyPI MCP servers
	// (RUNNER-PLAN-3 §P2).
	DefaultNodeRuntime   = "ghcr.io/blaktron/agentry-node-alpine:22-alpine"
	DefaultPythonRuntime = "ghcr.io/blaktron/agentry-python-alpine:3.12-alpine"
	// DefaultUnsafeImage is the Unsafe Mode base (agentry-notes
	// plans/unsafe-mode.md §4.1; agentry-dockerimages#15): Kali with a
	// command index and an extras list, pinned by digest, amd64 and arm64.
	// It moves with a CLI release, re-pinned monthly (D10). Since
	// 2026.09.30.3 the index also holds the programs the base itself carries
	// through alternatives links or that Contents left ambiguous
	// (agentry-dockerimages#23; agentry-cli#516).
	DefaultUnsafeImage = "ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.3@sha256:7c3da264fd0e8420f0367fa179e6b89110fa2f86bef8850f48d5f89ae21ab869"
	// Default trusted repositories for capabilities.mcpServers.
	DefaultNPMRegistry  = "https://registry.npmjs.org"
	DefaultPyPIRegistry = "https://pypi.org"
)

// Policy is the whole operator policy file (closed schema; unknown keys
// are errors so a typo cannot silently disable a control).
type Policy struct {
	// PolicyVersion pins the schema ("1").
	PolicyVersion string `yaml:"policyVersion"`
	// Images pins every image the runner may pull or build.
	Images Images `yaml:"images"`
	// Model configures the LLM the default adapter drives.
	Model Model `yaml:"model"`
	// AllowWildcardEgress gates manifest `network.egress: "*"` (§R4).
	// Default false: a wildcard agent is refused at compile time.
	AllowWildcardEgress bool `yaml:"allowWildcardEgress"`
	// SecretSource is where declared secret values are resolved from.
	SecretSource SecretSource `yaml:"secretSource"`
	// DataAccessServers maps a manifest dataAccess category to operator
	// catalog server-entry YAML files to attach for agents declaring it
	// (RUNNER-PLAN §R3; default empty).
	DataAccessServers map[string][]string `yaml:"dataAccessServers,omitempty"`
	// Budgets are the default hard stops. A run's flags override them, and
	// a built-in package run on this computer starts from a longer timeout
	// of its own (localrun.Builtin.LocalBudgets, DR-54).
	Budgets Budgets `yaml:"budgets"`
	// Sessions caps interactive sessions (plan §6.7).
	Sessions Sessions `yaml:"sessions"`
	// Profiles configures MCP-profile behavior (RUNNER-PLAN-2 §P2).
	Profiles Profiles `yaml:"profiles,omitempty"`
	// MCPRegistries is where declared capabilities.mcpServers resolve
	// (RUNNER-PLAN-3 §P2): npm and PyPI by default, private mirrors here.
	MCPRegistries MCPRegistries `yaml:"mcpRegistries,omitempty"`
	// MCPServers governs the registry's vulnerability verdict on declared
	// servers (agentry#177, DR-16).
	MCPServers MCPServersPolicy `yaml:"mcpServers,omitempty"`
	// Harness holds per-adapter settings for the adapters that run the
	// person's own installation of a harness (plan runner-m-r4-adapters
	// §5), keyed by adapter name.
	Harness map[string]HarnessPolicy `yaml:"harness,omitempty"`
	// Exec is the operator's exec catalog: which binaries this machine
	// provides to a QuickAgent that declares `exec: {commands}`, and from
	// where (plan examples-declared-servers §5.2, D1).
	Exec ExecPolicy `yaml:"exec,omitempty"`
	// Docker is how the runner drives the daemon beyond the images: the
	// OCI runtime a run's containers are created with (DR-23).
	Docker DockerPolicy `yaml:"docker,omitempty"`
	// Judge names where the person's own AI Review (TypeSafe Jev) key
	// comes from, for local work that needs a scan: a draft made on this
	// computer (plan builder-rework.md D25; agentry-cli#345). Unset takes
	// $TYPESAFE_API_KEY from the environment.
	Judge JudgeKey `yaml:"judge,omitempty"`
	// WebSearch names the person's own search provider and where its key
	// comes from, for a local draft's web_search. Unset, a local draft
	// has no web search (its web_read and registry_search still work).
	WebSearch WebSearchKey `yaml:"webSearch,omitempty"`
}

// ExecPolicy is the exec catalog (agentry-cli#100). The manifest keeps
// declaring bare command names; the operator, not the manifest, says
// which apk package provides each on this machine — the same stance as
// mcpRegistries. A declared command the catalog does not carry is a
// plan-time refusal, never a run-time "absent". Busybox applets the runner
// image already has (sh, sed, sort, …) are catalogued as `busybox`.
// DockerPolicy is the daemon-facing part of the policy.
type DockerPolicy struct {
	// Runtime names the daemon's OCI runtime (`docker run --runtime`) for
	// a run's containers: gVisor's "runsc" on a host that has it
	// registered. With Resolvers named it also holds the llm-proxy, the
	// MCP server install proxy and the relays (agentry-cli#337), save a
	// host-run harness's proxy and, where the docker filter does not run,
	// the relays; without them those three stay on the daemon's default,
	// because they resolve hosts through docker's embedded DNS, which a
	// sandbox does not reach (DR-23, agentry-cli#221). The gateway takes
	// GatewayRuntime. The receipt's runtime line names each class. Empty
	// means the daemon's default —
	// every run before the knob existed. A set value is enforced, not
	// preferred: a daemon without it refuses the run at the ensure step
	// (exit 13) rather than falling back, so the receipt's runtime line
	// is never a claim the run did not have.
	Runtime string `yaml:"runtime,omitempty"`
	// GatewayRuntime names the runtime the gateway is created with: a
	// second registration of gVisor with `--host-uds=open` (agentry-cli
	// #337), since the gateway reaches the run's docker filter through a
	// bind-mounted host Unix socket, which runsc without that flag does
	// not open (measured on dev, DR-23). It needs Runtime: the gateway is
	// never sandboxed while the QuickAgent's code is not. Empty leaves the
	// gateway on the daemon's default. Enforced like Runtime (exit 13).
	GatewayRuntime string `yaml:"gatewayRuntime,omitempty"`
	// Resolvers are the name servers the built-in Drafter's web tools
	// resolve through, by address (agentry-cli#386), and, under Runtime,
	// the llm-proxy, the MCP server install proxy and the relays too
	// (agentry-cli#337): at most three, as a resolv.conf reads them. A
	// sandboxed runtime's own network stack does not reach docker's
	// embedded DNS on a user-defined network (DR-23,
	// measured under gVisor), and `--dns` only sets that server's upstream,
	// so the runner writes these into a resolv.conf of the run's own that
	// the tools server reads in place of docker's. They are the host's to
	// name: the host's upstream resolvers, never a loopback stub. Empty
	// keeps docker's DNS, which a run on the daemon's default reaches; a
	// run of the web tools under a runtime with none is refused (exit 11)
	// rather than left to read nothing.
	Resolvers []string `yaml:"resolvers,omitempty"`
	// Limits bound every container the runner starts for a run
	// (agentry-cli#163). The engine also drops every capability and sets
	// no-new-privileges on each. A zero field takes its default
	// (Limits.WithDefaults), so a policy written before the field exists
	// is bounded all the same. The gateway passes memory and CPUs on to
	// the tool and server containers it launches.
	Limits Limits `yaml:"limits,omitempty"`
}

// Limits are the per-container resource bounds of a run (agentry-cli#163,
// DR-30): memory in docker's notation (`2g`), whole CPUs, and processes.
type Limits struct {
	Memory string `yaml:"memory,omitempty"`
	CPUs   int    `yaml:"cpus,omitempty"`
	Pids   int    `yaml:"pids,omitempty"`
}

// The defaults: enough for the docker-agent harness and a declared
// server's install; not a sizing for several runs on one host, which is
// the hosted plan's to choose (hosted-runs-production D7). One CPU because
// docker refuses more than the host has, and because it is what the
// gateway already gave the containers it launches.
const (
	DefaultMemory = "2g"
	DefaultCPUs   = 1
	DefaultPids   = 512
)

// WithDefaults fills every zero field with its default.
func (l Limits) WithDefaults() Limits {
	if l.Memory == "" {
		l.Memory = DefaultMemory
	}
	if l.CPUs == 0 {
		l.CPUs = DefaultCPUs
	}
	if l.Pids == 0 {
		l.Pids = DefaultPids
	}
	return l
}

// memoryRE is a docker memory size with its unit, at least docker's own
// floor of 6m: 64m, 512m, 2g.
var memoryRE = regexp.MustCompile(`^([6-9]|[1-9][0-9]{1,5})m$|^[1-9][0-9]{0,3}g$`)

type ExecPolicy struct {
	// Packages maps a command name to the apk package that provides it,
	// optionally pinned: `git` or `git=2.49.1-r0`. Alpine's index drops a
	// version once a security update replaces it, so a pin can turn an
	// example's every run into a failed image build; the build records
	// the resolved versions into the receipt either way (D11).
	Packages map[string]string `yaml:"packages,omitempty"`
	// APKMirror replaces the runner image's Alpine repositories at exec
	// image build time (`<mirror>/v<release>/main` and `/community`), for
	// an air-gapped machine. Empty: the image's own /etc/apk/repositories.
	// This is build-time egress of the image build only; a run's network
	// posture is unchanged.
	APKMirror string `yaml:"apkMirror,omitempty"`
}

// execCommandRE is a bare command name; apkPackageRE an apk package name
// with an optional exact version — the only shapes the exec image build
// interpolates into its Dockerfile.
var (
	execCommandRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	apkPackageRE  = regexp.MustCompile(`^[A-Za-z0-9._+-]+(=[A-Za-z0-9._+~-]+)?$`)
)

// Package returns the catalogued apk package for a command, "" when the
// catalog does not carry it.
func (e ExecPolicy) Package(command string) string {
	return e.Packages[command]
}

// HarnessPolicy configures one host-run harness adapter — an adapter that
// drives the person's own installation of a harness rather than an image
// the runner pins (plan runner-m-r4-adapters §5).
type HarnessPolicy struct {
	// Path is the executable to run instead of looking the harness up —
	// on PATH, then in its usual install locations (the native
	// installer's directory, a desktop app's bundled build; the
	// adapter's WellKnownPaths, agentry-cli#167). A path set here wins
	// over both, and one that does not exist is a refusal, not a
	// fallback.
	Path string `yaml:"path,omitempty"`
	// Model is the model id this harness is asked for (empty: model.name).
	// A harness that speaks its own vendor's API needs a model named in
	// that vendor's terms, which is not always the one model.name holds
	// for the default adapter.
	Model string `yaml:"model,omitempty"`
	// BaseURL points the harness at an endpoint that speaks its own API
	// (for claude, ANTHROPIC_BASE_URL). Empty: the harness's default
	// endpoint. The host must also be in model.proxyAllow, or the run
	// cannot reach it.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// APIKeyEnv names the host env var holding this harness's key (empty:
	// model.apiKeyEnv). The value is read at run time and never stored.
	APIKeyEnv string `yaml:"apiKeyEnv,omitempty"`
	// APIKeyKind is as model.apiKeyKind, for this harness's key (empty:
	// model.apiKeyKind).
	APIKeyKind string `yaml:"apiKeyKind,omitempty"`
	// APIKeySource is as model.apiKeySource, for this harness's key
	// (empty: model.apiKeySource); the keychain entry is the model
	// provider's.
	APIKeySource string `yaml:"apiKeySource,omitempty"`
}

// MCPServersPolicy is the operator's stance on declared servers the
// registry knows to be vulnerable.
type MCPServersPolicy struct {
	// AllowVulnerable lets a run proceed when the registry reports a high
	// or critical advisory for a declared server version. Off by default:
	// such a run is refused (exit 11) unless --allow-vulnerable-mcp is
	// given; either override is recorded in the receipt.
	AllowVulnerable bool `yaml:"allowVulnerable,omitempty"`
}

// MCPRegistries names the trusted repositories for declared MCP servers.
// Empty fields resolve to the public defaults at load time, so a policy
// file written before this block existed keeps working unchanged. They
// must be https: the install builder reaches them only through the
// install proxy, which tunnels TLS, with the registry's host (and the
// artifact hosts its metadata names for the pin) as its whole allowlist
// (agentry-cli#92).
type MCPRegistries struct {
	// NPM is the npm registry base URL (metadata + tarballs).
	NPM string `yaml:"npm,omitempty"`
	// PyPI is the PyPI base URL (JSON API at /pypi/, index at /simple).
	PyPI string `yaml:"pypi,omitempty"`
	// PyPIAllowSourceDists lets the builder fall back to source
	// distributions (which run the package's own build code inside the
	// runtime image at install time). Off by default: only wheels are
	// installed, for the declared server and its dependencies alike
	// (agentry-cli#4). Turn on for a server that ships no wheel, knowing
	// what it costs.
	PyPIAllowSourceDists bool `yaml:"pypiAllowSourceDists,omitempty"`
}

// Profiles is the MCP-profile policy block.
type Profiles struct {
	// KeepByDefault is the default completion choice when neither the
	// run flag nor guided setup decided: keep (true) or remove. A
	// pointer so a policy file written before this field existed reads
	// as "unset" (→ true), not as false.
	KeepByDefault *bool `yaml:"keepByDefault,omitempty"`
}

// KeepProfilesByDefault resolves the default keep choice (unset → true:
// authenticated resources are the point of profiles; discarding is the
// explicit choice, P2 §P8).
func (p *Policy) KeepProfilesByDefault() bool {
	if p.Profiles.KeepByDefault == nil {
		return true
	}
	return *p.Profiles.KeepByDefault
}

// Images pins the container images (§R9). Digest-pinned refs
// (repo@sha256:…) are recommended in the generated default file.
type Images struct {
	// Gateway is the docker/mcp-gateway image.
	Gateway string `yaml:"gateway"`
	// HarnessDockerAgent is the docker-agent harness image.
	HarnessDockerAgent string `yaml:"harnessDockerAgent"`
	// GoBuilder builds the local runner image (§R7).
	GoBuilder string `yaml:"goBuilder"`
	// Runner overrides the local runner image tag ("" = derived from the
	// CLI version).
	Runner string `yaml:"runner,omitempty"`
	// NodeRuntime / PythonRuntime install and run declared npm/PyPI MCP
	// servers (RUNNER-PLAN-3). Empty resolves to the pinned defaults.
	NodeRuntime   string `yaml:"nodeRuntime,omitempty"`
	PythonRuntime string `yaml:"pythonRuntime,omitempty"`
	// Unsafe is the base Unsafe Mode builds a QuickAgent's exec image on,
	// pinned by digest ("" = DefaultUnsafeImage; see UnsafeImage). It
	// names an image and turns nothing on: only a client that passes
	// compile.Inputs.Unsafe uses Unsafe Mode, and no policy key or
	// `agentry run` flag does (plan unsafe-mode.md D5).
	Unsafe string `yaml:"unsafe,omitempty"`
}

// UnsafeImage is the Unsafe Mode base this policy names: images.unsafe,
// else the CLI's pinned default. Resolved at use rather than written into
// a generated policy file, so a saved file never holds back a re-pin.
func (i Images) UnsafeImage() string {
	if i.Unsafe != "" {
		return i.Unsafe
	}
	return DefaultUnsafeImage
}

// Model configures the operator-owned model endpoint (annex Q-R2: the key
// is always the operator's; packages never carry or suggest credentials).
type Model struct {
	// Provider is the provider name: one of the setup table's
	// (anthropic, openai, google), "custom" for an OpenAI-compatible
	// endpoint of the operator's own (BaseURL required; the docker-agent
	// adapter renders it as the harness's openai client pointed at that
	// URL, agentry-cli#169), or a provider name the harness itself knows.
	Provider string `yaml:"provider"`
	// Name is the model id.
	Name string `yaml:"name"`
	// APIKeyEnv names the host env var holding the API key. The value is
	// injected only into the agent container and disclosed in receipts.
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// APIKeySource is where the key comes from when the variable is not
	// set: "" or "env" (the environment only) or "keyring" (the OS
	// keychain entry provider:<provider>, written by `agentry setup` or
	// the desktop; agentry-cli#148). The variable wins whenever it is set;
	// the value is never in this file.
	APIKeySource string `yaml:"apiKeySource,omitempty"`
	// APIKeyKind is what the credential is, for the one provider whose
	// keys come in two kinds (agentry-cli#171): "" or "api" (an API key,
	// any harness) or "subscription" (an Anthropic subscription OAuth
	// token, which Anthropic admits from Claude Code alone — the claude
	// adapter passes it as CLAUDE_CODE_OAUTH_TOKEN, the container harness
	// refuses it). Written by `agentry setup` or the desktop from the
	// value's shape at store time; a run checks the value it resolves
	// against this and refuses a mismatch by name.
	APIKeyKind string `yaml:"apiKeyKind,omitempty"`
	// BaseURL points provider-compatible adapters at a custom endpoint
	// (e.g. a local OpenAI-compatible server); empty = provider default.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// ProxyAllow is the llm-proxy CONNECT allowlist (host:port). This is
	// the ONLY egress the agent container has (§R4); it must cover the
	// model endpoint and nothing else.
	ProxyAllow []string `yaml:"proxyAllow"`
}

// JudgeKey is where the person's own Jev key comes from. The value is
// never in this file: the variable, else, with apiKeySource keyring, the
// OS keychain entry provider:typesafe (which the desktop writes).
type JudgeKey struct {
	// APIKeyEnv names the variable; empty is DefaultJudgeKeyEnv.
	APIKeyEnv string `yaml:"apiKeyEnv,omitempty"`
	// APIKeySource is "" or "env" (the environment only) or "keyring".
	APIKeySource string `yaml:"apiKeySource,omitempty"`
}

// WebSearchKey is the person's own search provider, and where its key
// comes from, as JudgeKey (keychain entry provider:<provider>).
type WebSearchKey struct {
	// Provider is "" or "none" (no search), or one of WebSearchProviders.
	Provider string `yaml:"provider,omitempty"`
	// APIKeyEnv names the variable; empty is the provider's own
	// (DefaultWebSearchKeyEnv for youcom, BraveSearchKeyEnv for brave, and
	// so on through webSearchProviders).
	APIKeyEnv    string `yaml:"apiKeyEnv,omitempty"`
	APIKeySource string `yaml:"apiKeySource,omitempty"`
}

// The default variables the person's own keys are read from, and the
// keychain provider name of the Jev key. DefaultWebSearchKeyEnv is
// You.com's, the first search provider.
const (
	DefaultJudgeKeyEnv     = "TYPESAFE_API_KEY"
	DefaultWebSearchKeyEnv = "YDC_API_KEY"
	BraveSearchKeyEnv      = "BRAVE_API_KEY"
	TavilySearchKeyEnv     = "TAVILY_API_KEY"
	ExaSearchKeyEnv        = "EXA_API_KEY"
	PerplexitySearchKeyEnv = "PERPLEXITY_API_KEY"
	JudgeKeyProvider       = "typesafe"
)

// webSearchProviders is each search provider a person may name: a name the
// runner image's copy of the core's pkg/websearch builds, the variable its
// key is read from by default (each provider's own, the name its own
// tools document), and its name as a person knows it. Brave's LLM Context
// API joined You.com with agentry#631 (DEV-157); Tavily, Exa and Perplexity
// joined for a person's own key with agentry-cli#513 (DEV-185, whose terms
// question is the person's to weigh).
var webSearchProviders = map[string]struct{ keyEnv, name string }{
	websearch.YouComName:     {DefaultWebSearchKeyEnv, "You.com"},
	websearch.BraveName:      {BraveSearchKeyEnv, "Brave"},
	websearch.TavilyName:     {TavilySearchKeyEnv, "Tavily"},
	websearch.ExaName:        {ExaSearchKeyEnv, "Exa"},
	websearch.PerplexityName: {PerplexitySearchKeyEnv, "Perplexity"},
}

// WebSearchProviders is the names webSearch.provider admits besides none,
// sorted.
func WebSearchProviders() []string {
	names := make([]string, 0, len(webSearchProviders))
	for name := range webSearchProviders {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// KeyEnv is the variable the Jev key is read from.
func (j JudgeKey) KeyEnv() string {
	if j.APIKeyEnv != "" {
		return j.APIKeyEnv
	}
	return DefaultJudgeKeyEnv
}

// On reports whether a search provider is named.
func (w WebSearchKey) On() bool { return w.Provider != "" && w.Provider != "none" }

// KeyEnv is the variable the search key is read from: the one the policy
// names, else the provider's own, so a You.com key never serves Brave.
func (w WebSearchKey) KeyEnv() string {
	if w.APIKeyEnv != "" {
		return w.APIKeyEnv
	}
	if p, ok := webSearchProviders[w.Provider]; ok {
		return p.keyEnv
	}
	return DefaultWebSearchKeyEnv
}

// ProviderName is the provider as a person knows it ("Brave", "Tavily").
func (w WebSearchKey) ProviderName() string {
	if p, ok := webSearchProviders[w.Provider]; ok {
		return p.name
	}
	return w.Provider
}

// envVarRE is an environment variable name.
var envVarRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretSource resolves declared secret names to values (§R9, Q-R3).
type SecretSource struct {
	// Mode is "env" (host environment) or "envFile".
	Mode string `yaml:"mode"`
	// EnvFile is the dotenv path for mode envFile.
	EnvFile string `yaml:"envFile,omitempty"`
}

// Sessions are the interactive-session caps (plan §6.7). A manifest's
// interactive.idleTimeoutSeconds may ask for less idle time, never more.
type Sessions struct {
	// IdleTimeoutSeconds ends a session after this much silence from the
	// user (default 900).
	IdleTimeoutSeconds int `yaml:"idleTimeoutSeconds"`
	// MaxWallSeconds is the whole-session deadline (default 7200).
	MaxWallSeconds int `yaml:"maxWallSeconds"`
	// MaxUserTurns caps user messages per session (default 200; 0 = unlimited).
	MaxUserTurns int `yaml:"maxUserTurns"`
}

// Budgets are the policy-default hard stops.
type Budgets struct {
	// TimeoutSeconds is the whole-run deadline (default 900).
	TimeoutSeconds int `yaml:"timeoutSeconds"`
	// MaxTurns caps tool-call events (default 60; 0 = unlimited).
	MaxTurns int `yaml:"maxTurns"`
	// MaxCostUSD kills the run once the harness's reported running cost
	// exceeds this many US dollars (exit 16); 0 = unlimited. Enforcement
	// is only as good as the harness's usage events (DR-4).
	MaxCostUSD float64 `yaml:"maxCostUsd"`
}

// Default returns the built-in policy.
func Default() *Policy {
	return &Policy{
		PolicyVersion: Version,
		Images: Images{
			Gateway:            DefaultGatewayImage,
			HarnessDockerAgent: DefaultHarnessImage,
			GoBuilder:          DefaultGoBuilder,
			NodeRuntime:        DefaultNodeRuntime,
			PythonRuntime:      DefaultPythonRuntime,
		},
		MCPRegistries: MCPRegistries{NPM: DefaultNPMRegistry, PyPI: DefaultPyPIRegistry},
		Model: Model{
			Provider:   "anthropic",
			Name:       "claude-sonnet-4-5",
			APIKeyEnv:  "ANTHROPIC_API_KEY",
			ProxyAllow: []string{"api.anthropic.com:443"},
		},
		SecretSource: SecretSource{Mode: "env"},
		Budgets:      Budgets{TimeoutSeconds: 900, MaxTurns: 60},
		Sessions:     Sessions{IdleTimeoutSeconds: 900, MaxWallSeconds: 7200, MaxUserTurns: 200},
		Profiles:     Profiles{KeepByDefault: boolPtr(true)},
	}
}

func boolPtr(b bool) *bool { return &b }

// Save writes the policy back to path as generated YAML. Used by
// `agentry setup` (P2 §P1); hand-written comments in an existing file are
// not preserved, and the header says so.
func Save(path string, p *Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Agentry Runner operator policy (RUNNER-PLAN §R9, RUNNER-PLAN-2 §P1).\n")
	buf.WriteString("# Written by `agentry setup` — regenerated as a whole; comments are not preserved.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create policy dir: %w", err)
	}
	// Staged beside the file and renamed over it: a run loading the policy
	// while setup writes it reads the old file or the new one, never a
	// truncated one (agentry-cli#299).
	if err := dirpublish.ReplaceFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	return nil
}

// HomeEnv relocates the runner home — the policy, the install cache, the
// profiles, the image context — as one tree, for tests and odd deployments.
const HomeEnv = "AGENTRY_RUNNER_HOME"

// DefaultPath is the operator policy under the runner home:
// $AGENTRY_RUNNER_HOME/policy.yaml when the home is relocated, else
// ~/.agentry/runner/policy.yaml (see Home).
func DefaultPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return PathIn(home), nil
}

// File is the operator policy file a run reads and how its path was
// chosen: what the receipt records beside the digest (agentry-cli#152).
// Source is one of the receipt's values (receipt.PolicySource*).
type File struct {
	Path   string
	Source string
}

// Resolve is the operator policy a run reads: flag when --policy named
// one, else the runner home's policy.yaml — source "env" when
// AGENTRY_RUNNER_HOME relocated the home, "default" otherwise. The path
// is made absolute, so a record of it names one file.
func Resolve(flag string) (File, error) {
	path, source := flag, receipt.PolicySourceFlag
	if flag == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			return File{}, err
		}
		source = receipt.PolicySourceDefault
		if os.Getenv(HomeEnv) != "" {
			source = receipt.PolicySourceEnv
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return File{}, fmt.Errorf("locate the policy file %s: %w", path, err)
	}
	return File{Path: abs, Source: source}, nil
}

// PathIn is the operator policy's path under a given runner home.
func PathIn(home string) string { return filepath.Join(home, "policy.yaml") }

// Home is the runner home: $AGENTRY_RUNNER_HOME when set, else
// ~/.agentry/runner. Every path under the runner home derives from here,
// the policy's included: a caller that relocates the home relocates the
// policy with it (agentry-cli#149 — before this, DefaultPath read the
// user's home regardless, so an isolated test wrote the operator's own
// policy).
func Home() (string, error) {
	if h := os.Getenv(HomeEnv); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory (set %s): %w", HomeEnv, err)
	}
	return filepath.Join(home, ".agentry", "runner"), nil
}

// Load reads and validates a policy file, also returning the sha256 of the
// exact bytes read (the plan's policySha256). Strict decoding: unknown
// keys are errors.
func Load(path string) (*Policy, string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-chosen policy path
	if err != nil {
		return nil, "", err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, "", explainParseError(path, err)
	}
	// Exactly one YAML document: a trailing document would silently not
	// apply, which for a policy file is a hole, not a convenience.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("parse %s: trailing YAML document", path)
	}
	// Fields added after a policy file was written resolve to their
	// defaults rather than failing validation — additions must not brick
	// existing machines (the file's sha still changes only when the
	// OPERATOR edits it, so plan invalidation semantics are unchanged).
	if p.Images.NodeRuntime == "" {
		p.Images.NodeRuntime = DefaultNodeRuntime
	}
	if p.Images.PythonRuntime == "" {
		p.Images.PythonRuntime = DefaultPythonRuntime
	}
	if p.MCPRegistries.NPM == "" {
		p.MCPRegistries.NPM = DefaultNPMRegistry
	}
	if p.MCPRegistries.PyPI == "" {
		p.MCPRegistries.PyPI = DefaultPyPIRegistry
	}
	if err := p.Validate(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	return &p, hex.EncodeToString(sum[:]), nil
}

// EnsureDefault writes the commented default policy file if path does not
// exist, then loads it. First `agentry run` on a machine goes through
// here, so the file the operator later edits is the file that ran.
//
// Two first runs on a fresh runner home (a CLI run and a desktop run
// started together) both come here: the default is staged beside path and
// linked into place only if no file is there yet, so neither ever reads
// it half-written and a policy the operator wrote meanwhile is never
// replaced (agentry-cli#299).
func EnsureDefault(path string) (*Policy, string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, "", fmt.Errorf("create policy dir: %w", err)
		}
		if _, err := dirpublish.CreateFile(path, []byte(defaultFileText), 0o644); err != nil {
			return nil, "", fmt.Errorf("write default policy: %w", err)
		}
	}
	return Load(path)
}

// Validate enforces the closed vocabulary. It runs at load time and again
// (via Load) before every compile, so a hand-edited policy fails before a
// container starts, with the field named.
func (p *Policy) Validate() error {
	if p.PolicyVersion != Version {
		return fmt.Errorf("policyVersion %q not supported (want %q)", p.PolicyVersion, Version)
	}
	if p.Images.Gateway == "" || p.Images.HarnessDockerAgent == "" || p.Images.GoBuilder == "" {
		return errors.New("images.gateway, images.harnessDockerAgent, and images.goBuilder are all required")
	}
	if p.Model.Provider == "" || p.Model.Name == "" {
		return errors.New("model.provider and model.name are required")
	}
	if strings.ContainsAny(p.Model.Provider, ": \t\n") {
		return fmt.Errorf("model.provider %q must not contain ':' or whitespace (it names the keychain account provider:<provider>)", p.Model.Provider)
	}
	if p.Sessions.IdleTimeoutSeconds < 0 || p.Sessions.MaxWallSeconds < 0 || p.Sessions.MaxUserTurns < 0 {
		return errors.New("sessions.idleTimeoutSeconds, sessions.maxWallSeconds and sessions.maxUserTurns must not be negative")
	}
	if p.Model.APIKeyEnv == "" {
		return errors.New("model.apiKeyEnv is required (the runner never embeds keys; it names the env var to read)")
	}
	if !validKeySource(p.Model.APIKeySource) {
		return fmt.Errorf("model.apiKeySource %q must be env or keyring", p.Model.APIKeySource)
	}
	if !validKeyKind(p.Model.APIKeyKind) {
		return fmt.Errorf("model.apiKeyKind %q must be api or subscription", p.Model.APIKeyKind)
	}
	if p.Model.APIKeyKind == KeyKindSubscription && p.Model.Provider != ProviderAnthropic {
		return fmt.Errorf("model.apiKeyKind subscription needs model.provider anthropic (it is %q): only Anthropic has such tokens, and only Claude Code may use one", p.Model.Provider)
	}
	for name, hp := range p.Harness {
		if !validKeySource(hp.APIKeySource) {
			return fmt.Errorf("harness.%s.apiKeySource %q must be env or keyring", name, hp.APIKeySource)
		}
		if !validKeyKind(hp.APIKeyKind) {
			return fmt.Errorf("harness.%s.apiKeyKind %q must be api or subscription", name, hp.APIKeyKind)
		}
		if hp.APIKeyKind == KeyKindSubscription && name != "claude" {
			return fmt.Errorf("harness.%s.apiKeyKind subscription: %s", name, SubscriptionTokenSentence)
		}
	}
	if len(p.Model.ProxyAllow) == 0 {
		return errors.New("model.proxyAllow must list the model endpoint host:port (it is the agent container's only egress)")
	}
	for _, hp := range p.Model.ProxyAllow {
		if strings.TrimSpace(hp) == "" || strings.ContainsAny(hp, " /") {
			return fmt.Errorf("model.proxyAllow entry %q must be host or host:port", hp)
		}
	}
	switch p.SecretSource.Mode {
	case "env":
		if p.SecretSource.EnvFile != "" {
			return errors.New("secretSource.envFile is only valid with mode: envFile")
		}
	case "envFile":
		if p.SecretSource.EnvFile == "" {
			return errors.New("secretSource.envFile is required with mode: envFile")
		}
	default:
		return fmt.Errorf("secretSource.mode %q must be env or envFile", p.SecretSource.Mode)
	}
	if p.Budgets.TimeoutSeconds <= 0 {
		return errors.New("budgets.timeoutSeconds must be positive")
	}
	if p.Budgets.MaxTurns < 0 {
		return errors.New("budgets.maxTurns must be >= 0 (0 = unlimited)")
	}
	for name, u := range map[string]string{
		"mcpRegistries.npm": p.MCPRegistries.NPM, "mcpRegistries.pypi": p.MCPRegistries.PyPI,
	} {
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return fmt.Errorf("%s %q must be an http(s) URL", name, u)
		}
	}
	for cmd, pkg := range p.Exec.Packages {
		// Both reach a Dockerfile RUN line verbatim: the grammar is apk's,
		// nothing looser.
		if !execCommandRE.MatchString(cmd) {
			return fmt.Errorf("exec.packages: command %q must be a bare command name (%s)", cmd, execCommandRE)
		}
		if !apkPackageRE.MatchString(pkg) {
			return fmt.Errorf("exec.packages.%s: package %q must be an apk package name, optionally =version (%s)", cmd, pkg, apkPackageRE)
		}
	}
	for name, k := range map[string]struct{ env, src string }{
		"judge": {p.Judge.APIKeyEnv, p.Judge.APIKeySource}, "webSearch": {p.WebSearch.APIKeyEnv, p.WebSearch.APIKeySource},
	} {
		if k.env != "" && !envVarRE.MatchString(k.env) {
			return fmt.Errorf("%s.apiKeyEnv %q must be an environment variable name", name, k.env)
		}
		if !validKeySource(k.src) {
			return fmt.Errorf("%s.apiKeySource %q must be env or keyring", name, k.src)
		}
	}
	if _, ok := webSearchProviders[p.WebSearch.Provider]; p.WebSearch.On() && !ok {
		names := WebSearchProviders()
		return fmt.Errorf("webSearch.provider %q must be none, %s or %s", p.WebSearch.Provider,
			strings.Join(names[:len(names)-1], ", "), names[len(names)-1])
	}
	if rt := p.Docker.Runtime; rt != "" && !runtimeNameRE.MatchString(rt) {
		return fmt.Errorf("docker.runtime %q must be a runtime name as the daemon registers it (%s)", rt, runtimeNameRE)
	}
	if rt := p.Docker.GatewayRuntime; rt != "" {
		if !runtimeNameRE.MatchString(rt) {
			return fmt.Errorf("docker.gatewayRuntime %q must be a runtime name as the daemon registers it (%s)", rt, runtimeNameRE)
		}
		if p.Docker.Runtime == "" {
			return fmt.Errorf("docker.gatewayRuntime %q needs docker.runtime: the gateway is sandboxed only where the QuickAgent's code is", rt)
		}
	}
	if err := validResolvers(p.Docker.Resolvers); err != nil {
		return err
	}
	if l := p.Docker.Limits; l.Memory != "" && !memoryRE.MatchString(l.Memory) {
		return fmt.Errorf("docker.limits.memory %q must be a size in m or g, such as 512m or 2g", l.Memory)
	}
	if c := p.Docker.Limits.CPUs; c < 0 || c > 64 {
		return fmt.Errorf("docker.limits.cpus must be 1–64 (0 takes the default), got %d", c)
	}
	if n := p.Docker.Limits.Pids; n < 0 || (n > 0 && n < 32) || n > 65536 {
		return fmt.Errorf("docker.limits.pids must be 32–65536 (0 takes the default), got %d", n)
	}
	if m := p.Exec.APKMirror; m != "" {
		u, err := url.Parse(m)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || u.RawQuery != "" {
			return fmt.Errorf("exec.apkMirror %q must be an http(s) URL with no query or fragment", m)
		}
	}
	return nil
}

// RunnerImageTag returns the local runner image tag: the policy override,
// or a tag derived from version — which every client passes as
// runnerimage.ContentVersion() (agentry-cli#6), so the CLI and the desktop
// app share one image for one embedded build context.
func (p *Policy) RunnerImageTag(version string) string {
	if p.Images.Runner != "" {
		return p.Images.Runner
	}
	v := version
	ok := v != ""
	for _, r := range v {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_'
		if !safe {
			ok = false
			break
		}
	}
	if !ok {
		v = "dev"
	}
	return "agentry-runner:" + v
}

// defaultFileText is the generated first-use policy. It must parse to
// exactly Default() (a unit test holds it to that) and carry the warnings
// an operator needs at the moment they edit it.
const defaultFileText = `# Agentry Runner operator policy (RUNNER-PLAN §R9).
# This file is machine policy, not agent configuration: it pins the images
# the runner may pull or build, names the model endpoint (and the ONLY
# egress the agent container gets), and decides what declared capabilities
# this machine will honor. Changing it invalidates installed run plans;
# they recompile on next use.
policyVersion: "1"

images:
  # Agentry's own mirrors of the upstream images, published from
  # blaktron/agentry-dockerimages. Every digest is recorded there, and each was
  # verified equal to the upstream digest it mirrors when it was copied.
  # goBuilder is pinned by digest: the @sha256 part fixes the bytes, and the
  # tag before it is only a label. The others are tags. They are ours, so they
  # cannot move without us, but nothing enforces that — a push to one would
  # move it and a run could not tell. For bytes that cannot move at all, pin
  # the digest from that repository's images.tsv (repo:tag@sha256:...), or
  # point these at any registry you prefer: a private one, or your own mirror.
  gateway: ghcr.io/blaktron/agentry-mcp-gateway:v0.43.3
  harnessDockerAgent: ghcr.io/blaktron/agentry-docker-agent:1.128.0
  goBuilder: ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
  # Runtimes for MCP servers a QuickAgent declares (capabilities.mcpServers).
  nodeRuntime: ghcr.io/blaktron/agentry-node-alpine:22-alpine
  pythonRuntime: ghcr.io/blaktron/agentry-python-alpine:3.12-alpine
  # The base of the desktop's Unsafe Mode exec images: Kali Linux with a
  # command index and an extras list, pinned by digest. Unset, the CLI's
  # pinned default is used, and it moves with the CLI. This key names the
  # image and turns nothing on: only the desktop's Unsafe Mode switch uses
  # it, for runs on that computer, and nothing in this file enables it.
  # unsafe: ghcr.io/blaktron/agentry-unsafe-kali:<tag>@sha256:<digest>

model:
  # anthropic | openai | google, or custom for an OpenAI-compatible
  # endpoint of your own (then baseUrl is required and proxyAllow names
  # its host:port; the harness reaches it through its openai client).
  provider: anthropic
  name: claude-sonnet-4-5
  # The env var holding YOUR api key. Keys are operator-owned; packages
  # never carry them. The run reads the value from this variable (or the
  # keychain, apiKeySource: keyring) and hands it to the harness under
  # the name its client expects for the provider.
  apiKeyEnv: ANTHROPIC_API_KEY
  # apiKeyKind: subscription   # an Anthropic subscription token (claude
  #   setup-token): Claude Code only, so anthropic only; written by setup
  #   from the key's shape (or harness.claude.apiKeyKind, beside an API
  #   key for the container harness here)
  # baseUrl: https://llm.internal:8443/v1   # custom only
  # The llm-proxy CONNECT allowlist: the agent container can reach these
  # host:port pairs and nothing else. Keep it to model endpoints.
  proxyAllow:
    - api.anthropic.com:443

# Manifest 'network.egress: "*"' is refused unless you set this true.
allowWildcardEgress: false

secretSource:
  # env: declared secret names resolve from the invoking environment.
  # envFile: resolve from a dotenv file instead (mode: envFile + envFile: path).
  mode: env

budgets:
  timeoutSeconds: 900
  maxTurns: 60
  # maxCostUsd: 0 = unlimited; a positive value is a hard stop (exit 16)
  # on the harness's reported running cost.
  maxCostUsd: 0
# Interactive sessions (DR-15): silence, wall clock and user-turn caps. A
# manifest may ask for a shorter idle timeout, never a longer one.
sessions:
  idleTimeoutSeconds: 900
  maxWallSeconds: 7200
  maxUserTurns: 200

# Adapters that drive YOUR installation of a harness (--adapter claude,
# and codex/goose as they land) rather than an image the runner pins.
# Everything here is optional: without it the harness is looked up on PATH,
# then in its usual install locations (agentry doctor --adapter <name>
# says which), and given model.name with model.apiKeyEnv.
# harness:
#   claude:
#     # path: /opt/claude/bin/claude
#     # An endpoint that speaks the Anthropic Messages API — Anthropic
#     # itself, or a compatible one. Its host must be in model.proxyAllow.
#     # baseUrl: https://api.anthropic.com
#     # model: claude-sonnet-4-5
#     # apiKeyEnv: ANTHROPIC_API_KEY

profiles:
  # A QuickAgent's MCP profile stores the keys/identities guided setup
  # collected, per exact release. keepByDefault: true retains it after a
  # run so the next run of the same version reuses the authenticated
  # resources; per-run --keep-profile overrides.
  keepByDefault: true

mcpRegistries:
  # Where capabilities.mcpServers resolve (RUNNER-PLAN-3). Point these at
  # private mirrors to control the supply chain; the manifest pins exact
  # versions and the install records resolved artifact digests either way.
  npm: https://registry.npmjs.org
  pypi: https://pypi.org

# docker:
#   # The OCI runtime a run's containers are created with (docker run
#   # --runtime): gVisor's runsc on a host that has it registered with the
#   # daemon. The llm-proxy, the install proxy and the relays take it only
#   # with resolvers below (never a host-run harness's proxy, and the relays
#   # only where the per-run docker filter runs, on Linux), and the gateway
#   # only with gatewayRuntime; each other stays on the daemon's default
#   # (DR-23). Unset, the daemon's default
#   # applies and the receipt says nothing about it. Set, it is enforced,
#   # not preferred: a daemon that does not have the runtime refuses the run
#   # at the ensure step (exit 13) and never falls back to runc; the receipt
#   # then names the runtime of each container class.
#   runtime: runsc
#   # The gateway's runtime: runsc registered again with --host-uds=open,
#   # since the gateway reaches the run's docker filter through a host
#   # Unix socket (agentry-cli#337). Needs runtime.
#   gatewayRuntime: runsc-uds
#   # The host's upstream name servers (at most three IPv4 addresses): the
#   # run's own resolv.conf for the classes under the runtime that resolve
#   # host names, since a sandbox does not reach docker's embedded DNS.
#   resolvers: [192.0.2.53]
#   # The bounds of every container a run starts (agentry-cli#163): memory
#   # (at least 6m, as 512m or 2g), whole CPUs (never more than this host
#   # has: docker refuses them), and processes. Unset fields take these
#   # defaults. The gateway passes memory and cpus on to the tool and server
#   # containers it launches. Every container also drops all capabilities
#   # and sets no-new-privileges; that is not configurable.
#   limits:
#     memory: 2g
#     cpus: 1
#     pids: 512

# judge:
#   # Your own AI Review (TypeSafe Jev) key, for local work that needs a
#   # scan: agentry draft has AI Review read the description and every
#   # search result, page and registry answer before the Drafter's model
#   # does, and does not run without it. The value is never in this file:
#   # the variable (default TYPESAFE_API_KEY), else, with apiKeySource
#   # keyring, the OS keychain entry provider:typesafe.
#   apiKeyEnv: TYPESAFE_API_KEY
#   apiKeySource: env

# webSearch:
#   # Your own web search, for agentry draft's web_search: none (the
#   # default: the Drafter reads the registry and the pages it knows),
#   # brave (Brave's LLM Context API), tavily, exa, perplexity (their
#   # search APIs) or youcom (You.com's Web Search API). Your searches go to
#   # the provider under its own terms. The key as for judge: the variable
#   # (default BRAVE_API_KEY, TAVILY_API_KEY, EXA_API_KEY,
#   # PERPLEXITY_API_KEY or YDC_API_KEY), else, with apiKeySource keyring,
#   # the OS keychain entry provider:<the provider>, e.g. provider:brave.
#   provider: brave
#   apiKeyEnv: BRAVE_API_KEY

# exec:
#   # The exec catalog: which binaries this machine provides to a QuickAgent
#   # that declares exec.commands, as command name → apk package (optionally
#   # =version). A declared command that is not catalogued is refused at plan
#   # time. Busybox applets the runner image already has (sh, sed, sort, …)
#   # are catalogued as busybox. The exec image build installs the packages
#   # and records their resolved versions into the run's receipt.
#   packages:
#     git: git
#     openssl: openssl
#     sort: busybox
#   # Alpine mirror for the image build (build-time egress only); empty uses
#   # the runner image's own repositories.
#   apkMirror: https://dl-cdn.alpinelinux.org/alpine
`

// validKeySource admits the apiKeySource values: unset, env, keyring.
func validKeySource(v string) bool { return v == "" || v == "env" || v == "keyring" }

// The apiKeyKind values: what an Anthropic credential is, read from its
// shape (agentry-cli#171). ProviderAnthropic is the one provider whose
// keys come in two kinds.
const (
	KeyKindAPI          = "api"
	KeyKindSubscription = "subscription"
	ProviderAnthropic   = "anthropic"
)

// The Anthropic prefixes, as the Console and `claude setup-token` mint
// them.
const (
	anthropicAPIPrefix          = "sk-ant-api"
	anthropicSubscriptionPrefix = "sk-ant-oat"
)

// CredentialKindOf reads the kind of a credential from its shape: a
// subscription OAuth token for Anthropic by its prefix, an API key for
// everything else — a shape this build does not know included, the
// endpoint being the judge. Pure; records nothing. Setup and the desktop
// write its answer into apiKeyKind at store time, a form uses it to say
// what a credential can drive before anything is stored, the models
// listing to send an Anthropic token in the header shape it needs, and
// the engine to hold a resolved value against the plan's expectation.
func CredentialKindOf(provider, key string) string {
	if strings.ToLower(strings.TrimSpace(provider)) != ProviderAnthropic {
		return KeyKindAPI
	}
	if strings.HasPrefix(strings.TrimSpace(key), anthropicSubscriptionPrefix) {
		return KeyKindSubscription
	}
	return KeyKindAPI
}

// SubscriptionTokenSentence is the one sentence every surface says about
// an Anthropic subscription token: what it drives and what it does not.
// Anthropic admits one from Claude Code alone (its terms since February
// 2026, and the API), so a run through any other harness would fail at
// the first model call.
const SubscriptionTokenSentence = "an Anthropic subscription token works with Claude Code only (agentry run --adapter claude); runs in the container harness need a Console API key (sk-ant-api…)"

// validKeyKind admits the apiKeyKind values: unset, api, subscription.
func validKeyKind(v string) bool {
	return v == "" || v == KeyKindAPI || v == KeyKindSubscription
}

// KeyKind is what the credential a run resolves is expected to be: the
// harness block's apiKeyKind, else the model's; unset is api. One rule,
// beside KeyEnv and KeySource (agentry-cli#171).
func KeyKind(hp HarnessPolicy, m Model) string {
	k := hp.APIKeyKind
	if k == "" {
		k = m.APIKeyKind
	}
	if k == "" {
		return KeyKindAPI
	}
	return k
}

// KeyEnv is the variable a run reads the model key from for an adapter:
// the harness block's, else the model's.
func KeyEnv(hp HarnessPolicy, m Model) string {
	if hp.APIKeyEnv != "" {
		return hp.APIKeyEnv
	}
	return m.APIKeyEnv
}

// KeySource is where that key comes from when the variable is unset: the
// harness block's apiKeySource, else the model's; "" and "env" both mean
// the environment only. One rule, so the adapters, the compiler's receipt
// line and doctor cannot disagree (agentry-cli#148).
func KeySource(hp HarnessPolicy, m Model) string {
	src := hp.APIKeySource
	if src == "" {
		src = m.APIKeySource
	}
	if src == "env" {
		return ""
	}
	return src
}

// maxResolvers is how many name servers a resolv.conf reads.
const maxResolvers = 3

// ResolvConfTarget is where a container reads its name servers.
const ResolvConfTarget = "/etc/resolv.conf"

// ResolvConf is the resolv.conf naming rs, the policy's docker.resolvers:
// the one the runner binds over docker's wherever a container under a
// sandboxed runtime resolves host names (agentry-cli#386, #337).
func ResolvConf(rs []string) []byte {
	var b strings.Builder
	b.WriteString("# The runner policy's docker.resolvers (agentry-cli#386, #337).\n")
	for _, r := range rs {
		b.WriteString("nameserver " + r + "\n")
	}
	return []byte(b.String())
}

// SandboxesProxies says whether the proxies run under the policy's
// runtime: only where the host's name servers are named for them to read
// (agentry-cli#337).
func (d DockerPolicy) SandboxesProxies() bool {
	return d.Runtime != "" && len(d.Resolvers) > 0
}

// validResolvers holds docker.resolvers to addresses a container can
// reach: IPv4 (the run's networks have no IPv6), not loopback (a host's stub, such as
// systemd-resolved's 127.0.0.53, is the container's own loopback inside
// it), unspecified or multicast, and at most maxResolvers, each once.
func validResolvers(rs []string) error {
	if len(rs) > maxResolvers {
		return fmt.Errorf("docker.resolvers names %d servers; a resolv.conf reads at most %d", len(rs), maxResolvers)
	}
	seen := map[netip.Addr]bool{}
	for _, r := range rs {
		a, err := netip.ParseAddr(r)
		switch {
		case err != nil || a.Zone() != "":
			return fmt.Errorf("docker.resolvers %q must be a name server's IP address", r)
		case a.IsLoopback() || a.IsUnspecified() || a.IsMulticast():
			return fmt.Errorf("docker.resolvers %q is not an address a container reaches: name the host's upstream resolvers, not a loopback stub", r)
		case !a.Is4():
			// The run's web network is docker's default IPv4 bridge
			// network, and gVisor's IPv6 is not what a draft should
			// depend on: an IPv6 server would validate and answer nothing.
			return fmt.Errorf("docker.resolvers %q: name IPv4 name servers; the run's networks have no IPv6", r)
		case seen[a]:
			return fmt.Errorf("docker.resolvers names %q twice", r)
		}
		seen[a] = true
	}
	return nil
}
