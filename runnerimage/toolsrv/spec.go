// Package toolsrv implements the SecureAgentry companion tools server
// (RUNNER-PLAN §R3): one MCP stdio server whose tool surface is constructed
// from the manifest's capability contract. An undeclared capability means
// the tool is ABSENT from tools/list, not present-but-refusing — the model
// never sees a tool it was not granted (RUNNER-PLAN §R11 invariant 4).
//
// This package is part of the embedded runner-image build context
// (RUNNER-PLAN §R7) and therefore MUST remain stdlib-only; a unit test in
// the parent package enforces that.
package toolsrv

// SpecVersion is the current tool-server spec schema version. "2" added
// the run-inputs channel (RUNNER-PLAN-2 §P4); the server refuses other
// versions, so a stale runner image meets a newer compiler loudly — and
// the image tag derives from the CLI version, so rebuilds happen on their
// own at upgrade time.
const SpecVersion = "2"

// Spec is the compiled configuration the runner's capability compiler hands
// to the tools server (written to plan/gateway/toolspec.json, bind-mounted
// read-only into the tools container). It is the only channel by which the
// manifest's capability contract reaches the server, so the compiler and
// this package must agree on it exactly — both sides import this type.
//
// All filesystem grants are expressed as paths RELATIVE to WorkspaceDir;
// the server never receives, and never honors, absolute host paths.
type Spec struct {
	// SpecVersion pins the schema ("1"); the server refuses others so a
	// stale image and a newer compiler fail loudly, not subtly.
	SpecVersion string `json:"specVersion"`
	// WorkspaceDir is the container path of the agent workspace mount
	// (conventionally /workspace). The mount's ro/rw flag is enforced by
	// the gateway's volume spec; the server additionally refuses writes
	// when FSWrite is disabled, so the two layers agree.
	WorkspaceDir string `json:"workspaceDir"`
	// OutDir is the container path of the run's output folder mount
	// (conventionally /out). write_report is rooted at OutDir/reports.
	OutDir string `json:"outDir"`
	// InputsDir is the container path of the read-only run-inputs mount
	// (conventionally /inputs; RUNNER-PLAN-2 §P4). Inputs are
	// operator-provided task material — a channel like stdin, not a
	// manifest capability — so list_inputs/read_input are granted
	// whenever the dir is configured, and the mount's :ro flag plus the
	// absence of any write tool keep the channel one-way. Empty means the
	// channel is not configured and its tools are absent.
	InputsDir string `json:"inputsDir,omitempty"`
	// FSRead gates fs_read and fs_list.
	FSRead Access `json:"fsRead"`
	// FSWrite gates fs_write.
	FSWrite Access `json:"fsWrite"`
	// Fetch gates the fetch tool.
	Fetch Fetch `json:"fetch"`
	// Exec gates the run_command tool.
	Exec Exec `json:"exec"`
	// Limits bounds tool I/O; zero values take the package defaults.
	Limits Limits `json:"limits"`
}

// Access is one direction of filesystem access inside the workspace.
type Access struct {
	// Enabled is false when the capability was not declared (mode none or
	// undeclared): the corresponding tools are not registered at all.
	Enabled bool `json:"enabled"`
	// Paths, when non-empty, restricts access to exactly these
	// workspace-relative paths (manifest filesystem {paths: [...]} mode).
	// An entry grants the file itself and, if it names a directory,
	// everything under it. Empty with Enabled=true means the whole
	// workspace (manifest "workspace" mode).
	Paths []string `json:"paths,omitempty"`
}

// Fetch configures the HTTP tool. The gateway enforces the same host list
// at the container level (allowHosts + --block-network); this server-side
// check is the belt to that braces, and the only place redirects are
// re-validated hop by hop.
type Fetch struct {
	// Enabled is true only when the manifest declared an egress host list
	// (or the policy-gated wildcard).
	Enabled bool `json:"enabled"`
	// Hosts are the exact DNS names requests may target.
	Hosts []string `json:"hosts,omitempty"`
	// AllowAll is the policy-gated "*" egress; the receipt discloses it.
	AllowAll bool `json:"allowAll"`
	// SecretEnv lists the environment variable NAMES (manifest secrets:)
	// the fetch tool may inject as auth headers. The gateway injects the
	// values into this container only; the tool never returns them to the
	// model (RUNNER-PLAN §R3).
	SecretEnv []string `json:"secretEnv,omitempty"`
}

// Exec configures run_command.
type Exec struct {
	// Commands is the manifest exec allowlist; empty disables the tool.
	// Enforcement is argv[0] membership plus a shim PATH containing only
	// these commands (RUNNER-DEVIATIONS DR-1 discloses the residual).
	Commands []string `json:"commands,omitempty"`
}

// Limits bounds tool I/O sizes and exec time.
type Limits struct {
	// MaxReadBytes caps fs_read (default DefaultMaxReadBytes).
	MaxReadBytes int64 `json:"maxReadBytes,omitempty"`
	// MaxWriteBytes caps fs_write and write_report content (default
	// DefaultMaxWriteBytes).
	MaxWriteBytes int64 `json:"maxWriteBytes,omitempty"`
	// MaxFetchBytes caps a fetch response body (default
	// DefaultMaxFetchBytes).
	MaxFetchBytes int64 `json:"maxFetchBytes,omitempty"`
	// MaxExecOutputBytes caps run_command combined output (default
	// DefaultMaxExecOutputBytes).
	MaxExecOutputBytes int64 `json:"maxExecOutputBytes,omitempty"`
	// ExecTimeoutSeconds caps one run_command invocation (default
	// DefaultExecTimeoutSeconds).
	ExecTimeoutSeconds int `json:"execTimeoutSeconds,omitempty"`
}

// Defaults for Limits zero values.
const (
	DefaultMaxReadBytes       = 256 << 10
	DefaultMaxWriteBytes      = 1 << 20
	DefaultMaxFetchBytes      = 512 << 10
	DefaultMaxExecOutputBytes = 256 << 10
	DefaultExecTimeoutSeconds = 120
)

// Tool names — the closed surface of RUNNER-PLAN §R3. The compiler writes
// these into the plan's tool-grant list and the gateway tools.yaml
// allowlist; the server registers exactly the granted subset.
const (
	ToolFSRead      = "fs_read"
	ToolFSList      = "fs_list"
	ToolFSWrite     = "fs_write"
	ToolFetch       = "fetch"
	ToolRunCommand  = "run_command"
	ToolListInputs  = "list_inputs"
	ToolReadInput   = "read_input"
	ToolWriteReport = "write_report"
)

// GrantedTools returns the tool names this spec exposes, in the fixed
// registration order. Both the compiler (tools.yaml, plan tool grants) and
// the server (tools/list) derive from this one function so they cannot
// disagree.
func (s *Spec) GrantedTools() []string {
	var names []string
	if s.FSRead.Enabled {
		names = append(names, ToolFSRead, ToolFSList)
	}
	if s.FSWrite.Enabled {
		names = append(names, ToolFSWrite)
	}
	if s.Fetch.Enabled {
		names = append(names, ToolFetch)
	}
	if len(s.Exec.Commands) > 0 {
		names = append(names, ToolRunCommand)
	}
	if s.InputsDir != "" {
		names = append(names, ToolListInputs, ToolReadInput)
	}
	names = append(names, ToolWriteReport)
	return names
}
