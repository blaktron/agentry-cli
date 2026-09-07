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
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the current policy schema major.
const Version = "1"

// Default image pins. Tags, not digests: the CLI cannot know digest values
// ahead of time, and inventing them would be worse than honesty — the
// generated default file says, loudly, to pin digests. Versions match the
// research annex survey of 2026-08-30.
const (
	DefaultGatewayImage = "docker/mcp-gateway:v0.43.3"
	DefaultHarnessImage = "docker/docker-agent:1.128.0"
	DefaultGoBuilder    = "golang:1.27-alpine"
	// Runtime images that install and run declared npm/PyPI MCP servers
	// (RUNNER-PLAN-3 §P2).
	DefaultNodeRuntime   = "node:22-alpine"
	DefaultPythonRuntime = "python:3.12-alpine"
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
	// Budgets are the default hard stops, overridable per run (tighter
	// only) by flags.
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
}

// HarnessPolicy configures one host-run harness adapter — an adapter that
// drives the person's own installation of a harness rather than an image
// the runner pins (plan runner-m-r4-adapters §5).
type HarnessPolicy struct {
	// Path is the executable to run instead of looking the harness up on
	// PATH.
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
// file written before this block existed keeps working unchanged.
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
}

// Model configures the operator-owned model endpoint (annex Q-R2: the key
// is always the operator's; packages never carry or suggest credentials).
type Model struct {
	// Provider is the harness provider name (anthropic, openai, …).
	Provider string `yaml:"provider"`
	// Name is the model id.
	Name string `yaml:"name"`
	// APIKeyEnv names the host env var holding the API key. The value is
	// injected only into the agent container and disclosed in receipts.
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// BaseURL points provider-compatible adapters at a custom endpoint
	// (e.g. a local OpenAI-compatible server); empty = provider default.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// ProxyAllow is the llm-proxy CONNECT allowlist (host:port). This is
	// the ONLY egress the agent container has (§R4); it must cover the
	// model endpoint and nothing else.
	ProxyAllow []string `yaml:"proxyAllow"`
}

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
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil { //nolint:gosec // policy holds no secret values
		return fmt.Errorf("write policy: %w", err)
	}
	return nil
}

// DefaultPath is ~/.agentry/runner/policy.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".agentry", "runner", "policy.yaml"), nil
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
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
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
func EnsureDefault(path string) (*Policy, string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, "", fmt.Errorf("create policy dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(defaultFileText), 0o644); err != nil { //nolint:gosec // policy holds no secret values
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
	if p.Sessions.IdleTimeoutSeconds < 0 || p.Sessions.MaxWallSeconds < 0 || p.Sessions.MaxUserTurns < 0 {
		return errors.New("sessions.idleTimeoutSeconds, sessions.maxWallSeconds and sessions.maxUserTurns must not be negative")
	}
	if p.Model.APIKeyEnv == "" {
		return errors.New("model.apiKeyEnv is required (the runner never embeds keys; it names the env var to read)")
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
  # Pin by digest (repo@sha256:...) once you have verified one. Tags are
  # the out-of-box default only because digests cannot be invented.
  gateway: docker/mcp-gateway:v0.43.3
  harnessDockerAgent: docker/docker-agent:1.128.0
  goBuilder: golang:1.27-alpine
  # Runtimes for MCP servers a BriefAgent declares (capabilities.mcpServers).
  nodeRuntime: node:22-alpine
  pythonRuntime: python:3.12-alpine

model:
  provider: anthropic
  name: claude-sonnet-4-5
  # The env var holding YOUR api key. Keys are operator-owned; packages
  # never carry them.
  apiKeyEnv: ANTHROPIC_API_KEY
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
# Everything here is optional: without it the harness is looked up on PATH
# and given model.name with model.apiKeyEnv.
# harness:
#   claude:
#     # path: /opt/claude/bin/claude
#     # An endpoint that speaks the Anthropic Messages API — Anthropic
#     # itself, or a compatible one. Its host must be in model.proxyAllow.
#     # baseUrl: https://api.anthropic.com
#     # model: claude-sonnet-4-5
#     # apiKeyEnv: ANTHROPIC_API_KEY

profiles:
  # A BriefAgent's MCP profile stores the keys/identities guided setup
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
`
