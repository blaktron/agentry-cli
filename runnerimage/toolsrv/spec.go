// Package toolsrv implements the Agentry companion tools server
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
// the run-inputs channel (RUNNER-PLAN-2 §P4); "3" the knowledge packs a
// run may search (plan knowledge.md §4.5, agentry-cli#317); "4" the
// built-in Drafter's web tools (plan builder-rework.md §7.3,
// agentry-cli#345); "5" the review grant (plan
// desktop-review-key-and-run-cards.md §6.2, agentry-cli#503); "6" the
// decoded inputs and search_inputs (plan file-handling.md §5,
// agentry-cli#560); "7" the calls ledger and end_blocked (plan
// quickagents-that-run.md M9, agentry-cli#629); "8" the scratch folder
// (plan scratch-and-pdf-reports.md §4, agentry-cli#729); "9" export_pdf
// (plan scratch-and-pdf-reports.md §5, agentry-cli#730); "10" the report
// design, theme document version 2, and export_pdf's form (plan
// report-designs.md §4, agentry-cli#784); "11" the result budget
// (Limits.MaxResultBytes and MaxResultLines; plan input-sources.md §4,
// agentry-cli#831). The server
// refuses other versions, so a stale runner image meets a newer compiler
// loudly — and the image tag derives from the CLI version, so rebuilds
// happen on their own at upgrade time.
const SpecVersion = "11"

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
	// InputsTextDir is the container path of the read-only decoded-inputs
	// mount (conventionally /inputs-text; plan file-handling.md §5,
	// agentry-cli#560): the engine's decode step writes the index of every
	// input there and the text decoded from each (inputs.go).
	// search_inputs is granted with it beside the inputs channel, except
	// under the review grant, where every text reaches the model only
	// through read_input's scan.
	InputsTextDir string `json:"inputsTextDir,omitempty"`
	// KnowledgeDir is the container path of the read-only knowledge mount
	// (conventionally /knowledge; plan knowledge.md §4.5), one directory
	// per attached pack. Knowledge names those packs. Like the inputs, a
	// pack the person attaches is task material and not a manifest
	// capability, so the compiled plan never carries either field: the
	// engine stamps both into the run's own copy of this file when packs
	// are attached (DR-38), and the three knowledge tools exist only then.
	KnowledgeDir string          `json:"knowledgeDir,omitempty"`
	Knowledge    []KnowledgePack `json:"knowledge,omitempty"`
	// Voice is the person's Writing Voice when one is attached (plan
	// writing-voice.md §5, D4; agentry-cli#531): its VOICE.md's container
	// path, under the knowledge mount, and its digest. Like a pack it is
	// chosen at run time and never in the compiled plan: the engine stamps
	// it into the run's own copy of this file, and voice_read exists only
	// then.
	Voice *VoiceFile `json:"voice,omitempty"`
	// ScratchDir is the container path of the run's scratch folder
	// (conventionally /scratch; plan scratch-and-pdf-reports.md §4,
	// agentry-cli#729): an empty tmpfs the docker filter mounts, private
	// to the run and gone with the container. scratch_read, scratch_list
	// and scratch_write are granted whenever it is set; the compiler sets
	// it for every package but a built-in. It is a grant of the runner,
	// not a line of the contract, and never aliases the workspace, which
	// FSRead and FSWrite govern.
	ScratchDir string `json:"scratchDir,omitempty"`
	// TmpfsBytes, when set, is the size the run's docker filter caps the
	// tools container's /tmp and ScratchDir at, each a tmpfs (Linux; the
	// filter runs nowhere else). The engine stamps it into the run's own
	// copy of this file, never into the compiled plan, and the tools role
	// then refuses to serve unless both are tmpfs no larger than it
	// (PrepareMounts).
	TmpfsBytes int64 `json:"tmpfsBytes,omitempty"`
	// FSRead gates fs_read and fs_list.
	FSRead Access `json:"fsRead"`
	// FSWrite gates fs_write.
	FSWrite Access `json:"fsWrite"`
	// Fetch gates the fetch tool.
	Fetch Fetch `json:"fetch"`
	// Exec gates the run_command tool.
	Exec Exec `json:"exec"`
	// Web gates the built-in Drafter's web tools (web.go). The compiler
	// sets Enabled for the Drafter alone; the engine stamps the rest into
	// the run's own copy of this file.
	Web Web `json:"web,omitzero"`
	// Review gates the review grant (review.go): ai_review, and read_input
	// answering only what AI Review passed. The compiler sets Enabled for a
	// built-in granted it; the engine stamps the ledger.
	Review Review `json:"review,omitzero"`
	// Calls is the calls ledger (calls.go): stamped by the engine into a
	// release's one-shot run alone, with end_blocked granted beside it.
	Calls Calls `json:"calls,omitzero"`
	// PDF is export_pdf's grant (pdf.go): Enabled by the compiler with the
	// scratch folder, the report design, the author line and the ledger
	// stamped by the engine.
	PDF PDF `json:"pdf,omitzero"`
	// Limits bounds tool I/O; zero values take the package defaults.
	Limits Limits `json:"limits"`
	// Owner, when set, is the host user who ran the command: every file
	// and directory write_report and fs_write create is chowned to it.
	// The tools container runs as root (see runnerimage/Dockerfile) and
	// writes into bind mounts the host user owns, so without this the
	// reports come out root-owned and the user cannot delete their own
	// output folder (agentry-cli#55). The engine stamps it into the
	// per-run copy of this file, never into the compiled plan; absent on
	// hosts without POSIX ids.
	Owner *Owner `json:"owner,omitempty"`
}

// KnowledgePack is one pack mounted under KnowledgeDir/<Name>. The name is
// what the tools take as their pack argument; the digest is the one the
// receipt records, so the run's own tool spec names what was attached in
// the same terms as the receipt.
type KnowledgePack struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// VoiceFile is an attached Writing Voice: the file voice_read returns, and
// the digest the receipt records for it.
type VoiceFile struct {
	File   string `json:"file"`
	Digest string `json:"digest"`
}

// Owner is a POSIX uid/gid pair.
type Owner struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
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
	// ImagePath is Unsafe Mode's (agentry-notes plans/unsafe-mode.md;
	// agentry-cli#490, the operator's decision of 2026-09-30): a declared
	// command runs with the image's own PATH after the shim directory,
	// so a declared script finds its interpreter and the programs it
	// calls (Ghidra's analyzeHeadless is a `#!/usr/bin/env bash` script
	// that runs java). The argv[0] allowlist is unchanged: only a declared
	// command can be called. Off, the PATH is the shim directory alone.
	ImagePath bool `json:"imagePath,omitempty"`
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
	// MaxResultBytes and MaxResultLines are the result budget: the most
	// one tool result may hold, so that the harness never cuts it (page.go;
	// plan input-sources.md §4, agentry-cli#831). The compiler stamps them
	// from the run's harness adapter, its measured limit less a margin
	// (adapter.ResultBudget); zero takes docker-agent's
	// (DefaultMaxResultBytes, DefaultMaxResultLines). The read tools page
	// within it, and the caps above still apply beneath it.
	MaxResultBytes int64 `json:"maxResultBytes,omitempty"`
	MaxResultLines int   `json:"maxResultLines,omitempty"`
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
	ToolFSRead     = "fs_read"
	ToolFSList     = "fs_list"
	ToolFSWrite    = "fs_write"
	ToolFetch      = "fetch"
	ToolRunCommand = "run_command"
	ToolListInputs = "list_inputs"
	ToolReadInput  = "read_input"
	// ToolSearchInputs searches the inputs' text (plan file-handling.md
	// §5), granted with InputsTextDir.
	ToolSearchInputs = "search_inputs"
	ToolWriteReport  = "write_report"
	// The scratch folder's tools (plan scratch-and-pdf-reports.md §4),
	// granted with ScratchDir.
	ToolScratchList  = "scratch_list"
	ToolScratchRead  = "scratch_read"
	ToolScratchWrite = "scratch_write"
	// The knowledge tools (plan knowledge.md §4.5), granted only while a
	// pack is attached.
	ToolKnowledgeList   = "knowledge_list"
	ToolKnowledgeSearch = "knowledge_search"
	ToolKnowledgeRead   = "knowledge_read"
	// The Writing Voice's one tool (plan writing-voice.md D4), granted
	// only while a voice is attached.
	ToolVoiceRead = "voice_read"
	// The web tools (plan builder-rework.md §7.3), the built-in Drafter's
	// alone: web_search only when the run has a search to use.
	ToolWebSearch      = "web_search"
	ToolWebRead        = "web_read"
	ToolRegistrySearch = "registry_search"
	// The review grant's tool (plan desktop-review-key-and-run-cards.md
	// §6.2): a built-in granted review alone.
	ToolAIReview = "ai_review"
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
		if s.Review.Enabled {
			names = append(names, ToolAIReview)
		} else if s.InputsTextDir != "" {
			names = append(names, ToolSearchInputs)
		}
	}
	if s.ScratchDir != "" {
		names = append(names, ToolScratchList, ToolScratchRead, ToolScratchWrite)
	}
	if s.KnowledgeDir != "" && len(s.Knowledge) > 0 {
		names = append(names, ToolKnowledgeList, ToolKnowledgeSearch, ToolKnowledgeRead)
	}
	if s.Voice != nil && s.Voice.File != "" {
		names = append(names, ToolVoiceRead)
	}
	if s.Web.Enabled {
		if s.Web.Search != "" {
			names = append(names, ToolWebSearch)
		}
		names = append(names, ToolWebRead)
		if s.Web.RegistrySearch && s.Web.Registry != "" {
			names = append(names, ToolRegistrySearch)
		}
	}
	names = append(names, ToolWriteReport)
	if s.PDF.Enabled {
		names = append(names, ToolExportPDF)
	}
	if s.Calls.Ledger != "" {
		names = append(names, ToolEndBlocked)
	}
	return names
}
