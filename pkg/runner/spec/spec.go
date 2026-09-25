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
	"sort"
	"strings"
)

// PlanVersion is the current plan schema major. The engine refuses plans
// with a different version (recompile instead of guessing).
// PlanVersion changes whenever plan.json's shape changes; installed plans
// with another version are recompiled. "2": run modes (Modes/Mode replace
// the v1 kind) and the interactive Session block (DR-15).
const PlanVersion = "2"

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
	// PHKnowledge is the host path of the per-run read-only knowledge dir
	// (plan knowledge.md §4.5): one subdirectory per attached pack,
	// mounted :ro into the tools container, empty when the run got no
	// --knowledge. Always compiled in, so attaching a pack never moves the
	// plan (DR-37).
	PHKnowledge = "@@RUN_KNOWLEDGE@@"
	// PHServers is the host path of the signature's installed MCP-server
	// store (RUNNER-PLAN-3): profile/servers/, one subdirectory per
	// declared server, mounted :ro into that server's runtime container.
	PHServers = "@@RUN_SERVERS@@"
	// PHSession is the per-run named volume an interactive session's
	// harness store lives in (DR-15; a volume since agentry-cli#90);
	// mounted only by interactive plans.
	PHSession = "@@RUN_SESSION@@"
	// PHHostGatewayURL / PHHostProxyURL are the published 127.0.0.1
	// addresses a host-run harness reaches the gateway and the proxy at
	// (plan runner-m-r4-adapters §5); known only once the run's containers
	// are up, so they are rendered into harness files and argv per run.
	PHHostGatewayURL = "@@HOST_GATEWAY_URL@@"
	PHHostProxyURL   = "@@HOST_PROXY_URL@@"
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
	// KnowledgeMount is where tools see the attached knowledge packs, each
	// at KnowledgeMount/<name> (plan knowledge.md §4.5).
	KnowledgeMount = "/knowledge"
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
	// HostGatewayURLEnv / HostProxyURLEnv carry the same published
	// addresses to a host-run harness as environment variables.
	HostGatewayURLEnv = "AGENTRY_GATEWAY_URL"
	HostProxyURLEnv   = "AGENTRY_PROXY_URL"
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
	// CompiledBy names the CLI build that compiled the plan. The schema
	// can stay the same while the compiler's output changes, so a plan
	// from another build is recompiled rather than reused.
	CompiledBy string `json:"compiledBy,omitempty"`
	// Ref is the canonical ns/name@version this plan was compiled from.
	Ref string `json:"ref"`
	// Digest is the verified archive digest (sha256:…), or the computed
	// digest of a --local package tree.
	Digest string `json:"digest"`
	// Source is "registry" or "local" (§R5).
	Source string `json:"source"`
	// Modes are the run modes the release declares (manifest v2 modes; a
	// v1 skill or prompt-agent is [autonomous]).
	Modes []string `json:"modes"`
	// Mode is the run mode this plan was compiled for, one of Modes. A
	// release compiles to one plan per mode (plan/<mode>/); the plans are
	// identical except Harness.Agent.Argv/StdinTask and Harness.Session
	// (plan §6.10 line 1, pinned by a golden test).
	Mode string `json:"mode"`
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
	// ExecImage is the tools container's image when the manifest declares
	// exec: the runner image plus the operator's catalogued packages,
	// minimized (agentry-cli#100). Nil when no exec is declared.
	ExecImage *ExecImageSpec `json:"execImage,omitempty"`
	// Harness describes the adapter and the agent container.
	Harness HarnessSpec `json:"harness"`
	// Proxy describes the LLM egress proxy sidecar.
	Proxy ProxySpec `json:"proxy"`
	// Docker is the daemon-facing part of the policy the plan was compiled
	// under; absent when the policy set none, so a plan compiled before the
	// knob and one compiled without it are the same bytes (DR-23).
	Docker *DockerSpec `json:"docker,omitempty"`
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
	// Agent is the agent container. Empty (no image) when Host is set.
	Agent ContainerSpec `json:"agent"`
	// Host runs the harness as a process on this machine from the person's
	// own installation instead of a container (plan runner-m-r4-adapters
	// §5, decision D2). Exactly one of Agent.Image and Host is set.
	Host *HostProcessSpec `json:"host,omitempty"`
	// ResultKind hints how the final result is extracted: "text" or
	// "json" (when Output.SchemaFile is set).
	ResultKind string `json:"resultKind"`
	// Session says how the harness is driven when Mode is interactive;
	// nil for an autonomous plan.
	Session *SessionSpec `json:"session,omitempty"`
}

// SessionSpec is the interactive drive of the harness (DR-15, plan §6.3):
// the runner is the Agent Client Protocol client on the agent container's
// attached stdio. Everything else about the container is the autonomous
// plan's.
type SessionSpec struct {
	// Transport is "acp-stdio".
	Transport string `json:"transport"`
	// ProtocolVersion is the ACP protocol version the client negotiates.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionDB is the container path of the harness's session store.
	SessionDB string `json:"sessionDb"`
	// Mounts are added to the agent container for a session only: the
	// writable session store under PHSession, which the engine resolves to
	// a per-run named volume (agentry-cli#90).
	Mounts []Mount `json:"mounts,omitempty"`
	// StoreOwner is the user, as the harness image knows it, that must
	// own the session store: a fresh volume is root-owned wherever it is
	// mounted, and the harness runs unprivileged, so the engine gives the
	// store to this user once, as root in the harness image on no network,
	// before the harness starts. Empty leaves the volume as docker made
	// it.
	StoreOwner string `json:"storeOwner,omitempty"`
	// Safety is the harness safety mode the generated config declares
	// (the ACP server has no flag): always "autonomous" — the gateway,
	// not in-harness prompting, is the boundary, and the runner answers
	// any permission request with a denial.
	Safety string `json:"safety"`
	// Budgets are the session caps the engine enforces (policy §6.7,
	// tightened by the manifest's interactive hints).
	Budgets SessionBudgets `json:"budgets"`
}

// SessionBudgets are the interactive caps: the engine ends the session
// with a session_end receipt when any is reached, never hangs.
type SessionBudgets struct {
	// IdleTimeoutSeconds is the longest silence from the user.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
	// MaxWallSeconds is the whole-session deadline.
	MaxWallSeconds int `json:"maxWallSeconds"`
	// MaxUserTurns caps user messages (0 = unlimited).
	MaxUserTurns int `json:"maxUserTurns"`
}

// SessionMount is the container path of the interactive session store.
const SessionMount = "/session"

// ProxySpec is the LLM egress proxy sidecar (§R4 agent topology).
type ProxySpec struct {
	// Image is the locally built runner image tag.
	Image string `json:"image"`
	// Allow is the host:port allowlist (policy model endpoints only).
	Allow []string `json:"allow"`
}

// DockerSpec is what the engine asks of the daemon for every container of
// the run beyond the image: the OCI runtime (docker run --runtime).
type DockerSpec struct {
	Runtime string `json:"runtime"`
}

// Runtime is the OCI runtime the plan asks for, or "" for the daemon's
// default.
func (p *Plan) Runtime() string {
	if p == nil || p.Docker == nil {
		return ""
	}
	return p.Docker.Runtime
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
	// ReadOnlyRoot runs the container with a read-only root filesystem
	// and Tmpfs as its only writable paths besides its mounts
	// (agentry-cli#163). The adapter sets both, since where a harness
	// writes is the image's to say.
	ReadOnlyRoot bool     `json:"readOnlyRoot,omitempty"`
	Tmpfs        []string `json:"tmpfs,omitempty"`
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
	// FromKeyring, beside FromHostEnv, names the OS keychain account read
	// when that variable is not set (policy apiKeySource keyring,
	// agentry-cli#148): the account, never the value.
	FromKeyring string `json:"fromKeyring,omitempty"`
	// FromRunToken injects the per-run gateway Bearer token.
	FromRunToken bool `json:"fromRunToken,omitempty"`
	// ExpectedKind, beside FromHostEnv, is the credential kind the plan
	// was compiled for (policy apiKeyKind, agentry-cli#171): the engine
	// checks the value it resolves against it and refuses a mismatch by
	// name, so a plan rendered for a subscription token never runs with
	// an API key and the reverse — the kind decides the harness's argv
	// and the variable it reads, which the plan fixed at compile time. A
	// consistency check between the policy and the key, not a boundary:
	// plan.json is the operator's own file. Nil when the provider has one
	// kind of key.
	ExpectedKind *CredentialKind `json:"expectedKind,omitempty"`
}

// CredentialKind names a provider and the kind of credential expected.
type CredentialKind struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
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
	// Env is the server's plain configuration from the manifest (name →
	// literal value), rendered into its catalog entry beside — never
	// through — its secrets. Receipts name the keys only.
	Env map[string]string `json:"env,omitempty"`
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

// ExecImageSpec is the per-BriefAgent exec image a plan runs its tools
// server in when the manifest declares exec (agentry-cli#9, #100): the
// runner image with the operator's catalogued packages installed, then
// minimized to exactly the declared commands.
type ExecImageSpec struct {
	// Tag is the image name (ExecImageTag).
	Tag string `json:"tag"`
	// Commands are the declared commands the image is minimized to, sorted.
	Commands []string `json:"commands"`
	// Packages are the catalogued apk packages the declared commands map
	// to (optionally pinned "name=version"), sorted, deduplicated.
	Packages []string `json:"packages,omitempty"`
	// APKMirror replaces the image's Alpine repositories at build time;
	// empty means the image's own.
	APKMirror string `json:"apkMirror,omitempty"`
}

// ExecImageTag names the per-BriefAgent exec image (agentry-cli#9, DR-1
// closure; #100): the runner image with the catalogued packages installed
// and minimized to exactly the declared commands. Deterministic in
// (runner image, sorted commands, sorted packages, mirror) so the compiler
// can reference it before the install step builds it, and so a changed
// catalog entry or mirror is a new image. "" when no exec is declared —
// the shared runner image serves.
func ExecImageTag(runnerImage string, commands, packages []string, apkMirror string) string {
	if len(commands) == 0 {
		return ""
	}
	sorted := append([]string(nil), commands...)
	sort.Strings(sorted)
	pkgs := append([]string(nil), packages...)
	sort.Strings(pkgs)
	sum := sha256.Sum256([]byte(runnerImage + "\x00" + strings.Join(sorted, "\x00") + "\x01" + strings.Join(pkgs, "\x00") + "\x01" + apkMirror))
	name := runnerImage
	if i := strings.LastIndex(name, ":"); i > 0 && !strings.Contains(name[i:], "/") {
		name = name[:i] + "-exec:" + name[i+1:] + "-" + hex.EncodeToString(sum[:6])
	} else {
		name += "-exec:" + hex.EncodeToString(sum[:6])
	}
	return name
}

// HostProcessSpec runs the harness as a process of this user from the
// person's own installation of it (plan runner-m-r4-adapters §5, decision
// D2). The gateway and the proxy are published on 127.0.0.1 for it; their
// addresses reach it as HostGatewayURLEnv / HostProxyURLEnv and, in plan
// files and argv, through the PHHostGatewayURL / PHHostProxyURL
// placeholders. The process is not inside the run's network boundary: the
// proxy variables and the gateway's catalog are what bound it, which the
// plan's Enforcement map says in so many words.
type HostProcessSpec struct {
	// Command is the executable name, resolved on PATH at run time (the
	// operator policy's harness.<adapter>.path overrides the lookup); a
	// plan never carries a host path, so it stays deterministic.
	Command string `json:"command"`
	// Argv follows the command; placeholders are rendered per run.
	Argv []string `json:"argv,omitempty"`
	// Env is set on the process; the run token and the model key travel
	// by name exactly as for a container.
	Env []EnvVar `json:"env,omitempty"`
	// InheritEnv names host variables passed through unchanged (HOME,
	// PATH and the like: the harness's own login state lives under them).
	// Everything else in the runner's environment is dropped.
	InheritEnv []string `json:"inheritEnv,omitempty"`
	// VersionArgv, when set, is run once before the harness so the
	// receipts record which release of it ran (for example --version).
	VersionArgv []string `json:"versionArgv,omitempty"`
	// StdinTask feeds the task on stdin (autonomous mode).
	StdinTask bool `json:"stdinTask,omitempty"`
	// RefuseAncestorFiles names files (relative names) that must not
	// exist in any ancestor of the harness's working directory: the
	// engine walks up to the root before starting the process and
	// refuses the run by name if one is there. A harness that reads
	// context from its ancestors (Claude Code without --bare reads
	// CLAUDE.md up the tree) names them here, so "no ambient context" is
	// measured rather than assumed (agentry-cli#171, DR-26).
	RefuseAncestorFiles []string `json:"refuseAncestorFiles,omitempty"`
}
