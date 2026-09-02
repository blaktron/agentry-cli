// Package spec defines the run plan — the deterministic artifact the
// capability compiler produces at install time and the engine executes
// (RUNNER-PLAN §R4, RQ5). A plan is pure data: no secret values, no
// machine-specific paths (run-scoped locations appear as placeholders,
// §R6), no timestamps. Same package digest + same policy ⇒ byte-identical
// plan.json, which is what makes the plan hash in every receipt meaningful.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// PlanVersion is the current plan schema major. The engine refuses plans
// with a different version (recompile instead of guessing).
const PlanVersion = "1"

// Run-scoped placeholders. Plans and the generated gateway files must not
// bake in per-run host paths or secrets, or they would stop being
// deterministic and start being sensitive; the engine substitutes these at
// execute time (§R6). The @@…@@ shape is chosen not to collide with the
// gateway's own {{server.key}} templating or docker-agent's ${env.VAR}.
const (
	// PHWorkspace is the host path of the per-run writable workspace copy.
	PHWorkspace = "@@RUN_WORKSPACE@@"
	// PHOut is the host path of the run's output folder (RQ3).
	PHOut = "@@RUN_OUT@@"
	// PHCfg is the host path of the per-run rendered gateway config dir.
	PHCfg = "@@RUN_CFG@@"
	// PHSecrets is the host path of the per-run secrets env file.
	PHSecrets = "@@RUN_SECRETS@@"
	// PHRunID is the run identifier (container names, labels).
	PHRunID = "@@RUN_ID@@"
	// PHInputs is the host path of the per-run read-only inputs dir
	// (RUNNER-PLAN-2 §P4) — operator-provided files, mounted :ro into the
	// tools container, empty when the run got no --file.
	PHInputs = "@@RUN_INPUTS@@"
	// PHServers is the host path of the signature's installed MCP-server
	// store (RUNNER-PLAN-3): profile/servers/, one subdirectory per
	// declared server, mounted :ro into that server's runtime container.
	PHServers = "@@RUN_SERVERS@@"
)

// MCPStorePrefix is the fixed in-container prefix a declared server's
// dependency closure is installed under AND mounted at — the same path in
// the builder and the runtime container, so interpreter paths (venv
// shebangs, .bin shims) stay valid.
const MCPStorePrefix = "/opt/agentry-mcp"

// Fixed container-side paths and network names shared by the compiler, the
// adapters, and the tools server configuration.
const (
	// WorkspaceMount is where tools see the workspace (§R2 contract text).
	WorkspaceMount = "/workspace"
	// OutMount is where tools see the output folder.
	OutMount = "/out"
	// InputsMount is where tools see the run-inputs channel (P2 §P4).
	InputsMount = "/inputs"
	// PlanMount is where the agent container sees its harness files.
	PlanMount = "/plan"
	// ToolSpecMount is where the tools container sees its spec file.
	ToolSpecMount = "/etc/agentry/toolspec.json"
	// GatewayAlias and GatewayPort locate the MCP gateway on the run
	// network; ProxyAlias/ProxyPort locate the LLM egress proxy.
	GatewayAlias = "gateway"
	GatewayPort  = 8811
	ProxyAlias   = "llm-proxy"
	ProxyPort    = 3128
	// ToolServerName is the gateway catalog name of the companion server.
	ToolServerName = "agentry-tools"
	// GatewayTokenEnv is the env var carrying the per-run Bearer token in
	// the agent container; GatewayAuthTokenEnv is what the gateway itself
	// reads.
	GatewayTokenEnv     = "MCP_GATEWAY_TOKEN"
	GatewayAuthTokenEnv = "MCP_GATEWAY_AUTH_TOKEN"
)

// GatewayURL is the MCP endpoint agents connect to on the run network.
func GatewayURL() string {
	return fmt.Sprintf("http://%s:%d/mcp", GatewayAlias, GatewayPort)
}

// ProxyURL is the CONNECT proxy address for the agent container's
// HTTPS_PROXY.
func ProxyURL() string {
	return fmt.Sprintf("http://%s:%d", ProxyAlias, ProxyPort)
}

// Plan is the whole compiled run plan (§R4). Every field is exported and
// JSON-stable; Hash() covers the canonical encoding.
type Plan struct {
	// PlanVersion pins the schema.
	PlanVersion string `json:"planVersion"`
	// Ref is the canonical ns/name@version this plan was compiled from.
	Ref string `json:"ref"`
	// Digest is the verified archive digest (sha256:…), or the computed
	// digest of a --local package tree.
	Digest string `json:"digest"`
	// Source is "registry" or "local" (§R5).
	Source string `json:"source"`
	// Kind is the manifest kind (skill | prompt-agent).
	Kind string `json:"kind"`
	// Summary is the manifest one-liner (harness description field).
	Summary string `json:"summary"`
	// PolicySHA256 fingerprints the operator policy the plan was compiled
	// under; a policy change invalidates installed plans (§R9).
	PolicySHA256 string `json:"policySha256"`
	// Instruction records where the harness instruction came from.
	Instruction InstructionSpec `json:"instruction"`
	// Capabilities is the normalized manifest contract.
	Capabilities CapSummary `json:"capabilities"`
	// Tools are the companion-server tools this agent is granted.
	Tools []string `json:"tools"`
	// Enforcement maps each contract line to the mechanism string that
	// lands verbatim in the result envelope (§R8, honest-posture rule).
	Enforcement map[string]string `json:"enforcement"`
	// Gateway describes the gateway container launch.
	Gateway GatewaySpec `json:"gateway"`
	// Harness describes the adapter and the agent container.
	Harness HarnessSpec `json:"harness"`
	// Proxy describes the LLM egress proxy sidecar.
	Proxy ProxySpec `json:"proxy"`
	// Secrets lists the manifest secret env names (values are resolved at
	// run time and never stored, §R4).
	Secrets []string `json:"secrets,omitempty"`
	// MCPServers are the declared, resolved MCP servers this plan loads
	// alongside the tools server (RUNNER-PLAN-3). Empty for agents that
	// declare none — such plans are byte-identical to pre-PLAN-3 output.
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
	// Budgets are the compiled defaults; run flags may tighten them and
	// the receipt records what actually applied.
	Budgets Budgets `json:"budgets"`
	// Output describes the optional structured-output convention (§R2).
	Output OutputSpec `json:"output"`
	// Files maps plan-dir-relative generated file paths to their sha256,
	// so a tampered installed plan is detectable before execution.
	Files map[string]string `json:"files"`
}

// InstructionSpec records the instruction's provenance, not its text — the
// text lives in the adapter's harness file, hashed via Files.
type InstructionSpec struct {
	// SourcePath is the archive-relative file the instruction came from.
	SourcePath string `json:"sourcePath"`
	// SHA256 is the hash of the fully rendered instruction (body +
	// runner-contract postscript), for receipts.
	SHA256 string `json:"sha256"`
}

// CapSummary is the manifest capability contract, normalized to closed
// enums so the engine and receipts never re-parse manifest unions.
type CapSummary struct {
	// EgressMode is none | hosts | all.
	EgressMode string `json:"egressMode"`
	// EgressHosts is set for mode hosts.
	EgressHosts []string `json:"egressHosts,omitempty"`
	// FSReadMode / FSWriteMode are none | workspace | paths.
	FSReadMode   string   `json:"fsReadMode"`
	FSReadPaths  []string `json:"fsReadPaths,omitempty"`
	FSWriteMode  string   `json:"fsWriteMode"`
	FSWritePaths []string `json:"fsWritePaths,omitempty"`
	// ExecCommands is the exec allowlist (empty = none).
	ExecCommands []string `json:"execCommands,omitempty"`
	// DataAccess is the declared semantic category list.
	DataAccess []string `json:"dataAccess,omitempty"`
}

// GatewaySpec is the gateway container launch description (§R4). Flags are
// compiled, not chosen at run time, so tests can assert --block-network is
// always present (§R11 invariant 1).
type GatewaySpec struct {
	// Image is the pinned gateway image from policy.
	Image string `json:"image"`
	// Args is the full gateway argv (after the image).
	Args []string `json:"args"`
	// Env is the gateway container environment.
	Env []EnvVar `json:"env"`
	// Mounts includes the rendered config dir and the docker socket.
	Mounts []Mount `json:"mounts"`
	// Servers is the enabled-server list (also in registry.yaml).
	Servers []string `json:"servers"`
}

// HarnessSpec names the adapter and carries the agent container spec.
type HarnessSpec struct {
	// Adapter is the harness adapter name (§R10).
	Adapter string `json:"adapter"`
	// Model is the provider/model string for receipts.
	Model string `json:"model"`
	// Agent is the agent container.
	Agent ContainerSpec `json:"agent"`
	// ResultKind hints how the final result is extracted: "text" or
	// "json" (when Output.SchemaFile is set).
	ResultKind string `json:"resultKind"`
}

// ProxySpec is the LLM egress proxy sidecar (§R4 agent topology).
type ProxySpec struct {
	// Image is the locally built runner image tag.
	Image string `json:"image"`
	// Allow is the host:port allowlist (policy model endpoints only).
	Allow []string `json:"allow"`
}

// ContainerSpec is a docker container the engine will run. Sources and env
// values may contain the PH* placeholders.
type ContainerSpec struct {
	Image string `json:"image"`
	// Argv is the command+args passed after the image (entrypoint's argv).
	Argv []string `json:"argv,omitempty"`
	Env  []EnvVar `json:"env,omitempty"`
	// Mounts are bind mounts (host source may hold placeholders).
	Mounts []Mount `json:"mounts,omitempty"`
	// StdinTask: the engine pipes the task text into stdin (§R2).
	StdinTask bool `json:"stdinTask,omitempty"`
}

// EnvVar is one environment variable. Exactly one of Value / FromHostEnv /
// FromRunToken is meaningful: literal (possibly placeholder-bearing),
// resolved from the operator's host environment at run time, or the
// per-run gateway token. Secret VALUES therefore never appear in a plan.
type EnvVar struct {
	Name string `json:"name"`
	// Value is a literal (may contain PH* placeholders).
	Value string `json:"value,omitempty"`
	// FromHostEnv resolves from the invoking host's environment at run
	// time (e.g. the operator's model API key, §R4).
	FromHostEnv string `json:"fromHostEnv,omitempty"`
	// FromRunToken injects the per-run gateway Bearer token.
	FromRunToken bool `json:"fromRunToken,omitempty"`
}

// Mount is one bind mount.
type Mount struct {
	// Source is the host path (may hold placeholders).
	Source string `json:"source"`
	// Target is the container path.
	Target string `json:"target"`
	// ReadOnly appends :ro.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// Budgets are the hard stops (§R8; DR-4 for cost).
type Budgets struct {
	// TimeoutSeconds kills the run (exit 15).
	TimeoutSeconds int `json:"timeoutSeconds"`
	// MaxTurns kills the run after this many tool-call events (exit 16);
	// 0 = unlimited.
	MaxTurns int `json:"maxTurns"`
	// MaxCostUSD kills the run once the reported running cost exceeds it
	// (exit 16); 0 = unlimited.
	MaxCostUSD float64 `json:"maxCostUsd,omitempty"`
}

// MCPServerSpec is one declared MCP server, resolved to its exact
// artifacts (RUNNER-PLAN-3). The store contents live under the
// signature's profile — per-machine — so the plan carries the RESOLUTION
// (source, version, digest, launch argv), which is deterministic given
// the manifest pins, and mounts the store via the PHServers placeholder.
type MCPServerSpec struct {
	// Name is the gateway catalog name ("mcp-" + sanitized package id).
	Name string `json:"name"`
	// Source is "npm" or "pypi"; Package and Version are the manifest pin.
	Source  string `json:"source"`
	Package string `json:"package"`
	Version string `json:"version"`
	// Integrity is the registry-advertised artifact integrity (npm SRI or
	// "sha256:<hex>") recorded at resolve time; receipts disclose it.
	Integrity string `json:"integrity,omitempty"`
	// Image is the pinned runtime image the server runs in.
	Image string `json:"image"`
	// Command is the argv inside the runtime container (store-relative
	// paths under MCPStorePrefix).
	Command []string `json:"command"`
	// StoreSubdir is the server's directory name under PHServers.
	StoreSubdir string `json:"storeSubdir"`
	// Tools optionally restricts the server to these tool names.
	Tools []string `json:"tools,omitempty"`
	// Egress is the server's own allowlist ("*" = unrestricted,
	// policy-gated at compile like agent egress).
	Egress []string `json:"egress,omitempty"`
	// Secrets are the env names this server needs (values resolved at run
	// time into the gateway secrets file under this server's keys).
	Secrets []string `json:"secrets,omitempty"`
}

// OutputSpec is the optional structured-output convention (§R2).
type OutputSpec struct {
	// SchemaFile is the archive-relative schema path ("" = none).
	SchemaFile string `json:"schemaFile,omitempty"`
	// SchemaSHA256 hashes the schema content for receipts.
	SchemaSHA256 string `json:"schemaSha256,omitempty"`
}

// MarshalCanonical renders the plan as stable, human-diffable JSON. Go's
// encoding/json emits struct fields in declaration order and sorts map
// keys, which is exactly the determinism the golden tests pin.
func (p *Plan) MarshalCanonical() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal plan: %w", err)
	}
	return append(b, '\n'), nil
}

// Hash returns the sha256 hex of the canonical encoding — the
// run_plan_sha256 every receipt carries (§R8).
func (p *Plan) Hash() (string, error) {
	b, err := p.MarshalCanonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ParsePlan decodes and version-checks a stored plan.json.
func ParsePlan(b []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse plan.json: %w", err)
	}
	if p.PlanVersion != PlanVersion {
		return nil, fmt.Errorf("plan version %q not supported (want %q) — re-run to recompile", p.PlanVersion, PlanVersion)
	}
	return &p, nil
}
