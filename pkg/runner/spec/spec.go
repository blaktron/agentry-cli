// Package spec defines the run plan — the deterministic artifact the capability
// compiler produces at install time and the engine executes. A plan is pure
// data: no secret values, no machine-specific paths (run-scoped locations
// appear as placeholders), no timestamps. Same package digest + same policy ⇒
// byte-identical plan.json, which is what makes the plan hash in every receipt
// meaningful.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// PlanVersion is the current plan schema major. The engine refuses plans
// with a different version (recompile instead of guessing).
// PlanVersion changes whenever plan.json's shape changes; installed plans
// with another version are recompiled. "2": run modes (Modes/Mode replace
// the v1 kind) and the interactive Session block.
const PlanVersion = "2"

// Run-scoped placeholders. Plans and the generated gateway files must not
// bake in per-run host paths or secrets, or they would stop being
// deterministic and start being sensitive; the engine substitutes these at
// execute time. The @@…@@ shape is chosen not to collide with the
// gateway's own {{server.key}} templating or docker-agent's ${env.VAR}.
const (
	// PHWorkspace is the host path of the per-run writable workspace copy.
	PHWorkspace = "@@RUN_WORKSPACE@@"
	// PHOut is the host path of the run's output folder.
	PHOut = "@@RUN_OUT@@"
	// PHCfg is the host path of the per-run rendered gateway config dir.
	PHCfg = "@@RUN_CFG@@"
	// PHSecrets is the host path of the per-run secrets env file.
	PHSecrets = "@@RUN_SECRETS@@"
	// PHRunID is the run identifier (container names, labels).
	PHRunID = "@@RUN_ID@@"
	// PHSecretsAbsent is the list of optional secrets this run was not
	// given, by name ("none" when it was given them all), for the System
	// Prompt: only the run knows which it holds, and the plan stays
	// the same bytes whichever they are. Unlike the others it stands for
	// text, never a path, an id or an address.
	PHSecretsAbsent = "@@RUN_SECRETS_ABSENT@@"
	// PHRunMode is the run's mode, "autonomous" or "interactive", for the
	// System Prompt: a release's two plans carry one instruction, so the text
	// guides both modes and only the run says which one it is. Text, like
	// PHSecretsAbsent.
	PHRunMode = "@@RUN_MODE@@"
	// PHInputs is the host path of the per-run read-only inputs dir —
	// operator-provided files, mounted :ro into the tools container, empty when
	// the run got no --file.
	PHInputs = "@@RUN_INPUTS@@"
	// PHInputsText is the host path of the per-run read-only decoded inputs
	// dir: the decode step's index and texts, mounted :ro into the tools
	// container, empty when the run's inputs were not decoded. Always compiled
	// in, like the inputs.
	PHInputsText = "@@RUN_INPUTS_TEXT@@"
	// PHKnowledge is the host path of the per-run read-only knowledge dir: one
	// subdirectory per attached pack, mounted :ro into the tools container,
	// empty when the run got no --knowledge. Always compiled in, so attaching a
	// pack never moves the plan.
	PHKnowledge = "@@RUN_KNOWLEDGE@@"
	// PHServers is the host path of the signature's installed MCP-server store:
	// profile/servers/, one subdirectory per declared server, mounted :ro into
	// that server's runtime container.
	PHServers = "@@RUN_SERVERS@@"
	// PHSession is the per-run named volume an interactive session's harness
	// store lives in; mounted only by interactive plans.
	PHSession = "@@RUN_SESSION@@"
	// PHShare is the host path of a persistent agent's share folder, the
	// person's own, mounted read-write and live at AgentRoot/AgentShareDir;
	// only a persistent plan whose agent has its share names it.
	PHShare = "@@RUN_SHARE@@"
	// PHShareMirror is the host path of a persistent agent's share mirror:
	// a folder of the agent's own holding a copy of each share file whose
	// bytes the share check passed, mounted read-only at
	// AgentRoot/AgentShareDir into each MCP server the person let see the
	// share (MCPServerSpec.Share); only such a plan names it.
	PHShareMirror = "@@RUN_SHARE_MIRROR@@"
	// PHShareOut is the host path of a segment's share outbox: a folder of
	// the segment's own, beside its cfg, mounted writable at
	// AgentRoot/AgentShareOutDir into each MCP server that sees the share
	// (MCPServerSpec.Share), whose regular files the runner copies into the
	// share after each turn under new names, never over a file; only such
	// a plan names it.
	PHShareOut = "@@RUN_SHARE_OUT@@"
	// PHSkills is the host path of a persistent agent's skills for one
	// segment: a folder the supervisor fills with the skills its roster has
	// on, mounted read-only at AgentRoot/AgentSkillsDir into the tools
	// server alone; only a persistent plan with a skill on names it.
	PHSkills = "@@RUN_SKILLS@@"
	// PHDependencyDB is the host path of OSV's databases the Coding agent's
	// dependency scan reads, kept under the runner home and mounted
	// read-only at DependencyDBMount into the tools server alone; only a
	// plan whose Coding agent has its dependency scan on names it.
	PHDependencyDB = "@@RUN_DEPENDENCY_DB@@"
	// PHData is a persistent agent's data volume, by name, mounted at
	// AgentRoot/AgentDataDir; only a persistent plan names it.
	PHData = "@@RUN_DATA@@"
	// PHMemory is a persistent agent's memory volume, by name, mounted at
	// AgentRoot/AgentMemoryDir; only a persistent plan whose agent keeps a
	// memory names it.
	PHMemory = "@@RUN_MEMORY@@"
	// PHAppLink is the host path of the socket a persistent agent's
	// supervisor answers its app link on, bound into the app link's
	// container alone; only a plan with an app link names it.
	PHAppLink = "@@RUN_APPLINK@@"
	// PHAppLinkMount is the app link container's one mount, source and
	// target: the supervisor's socket at the socket's path where the
	// engine's daemon runs on this computer, and the run's relay volume at
	// the socket's folder where it runs in a VM, chosen at run time so the
	// plan does not depend on the engine.
	PHAppLinkMount = "@@RUN_APPLINK_MOUNT@@"
	// PHAppLinkUser is the user the app link runs as: the invoking uid:gid,
	// whose socket it connects to, as the host link runs.
	PHAppLinkUser = "@@RUN_APPLINK_USER@@"
	// PHHostGatewayURL / PHHostProxyURL are the published 127.0.0.1 addresses a
	// host-run harness reaches the gateway and the proxy at; known only once
	// the run's containers are up, so they are rendered into harness files and
	// argv per run.
	PHHostGatewayURL = "@@HOST_GATEWAY_URL@@"
	PHHostProxyURL   = "@@HOST_PROXY_URL@@"
	// PHModelSignIn is the host path of what a model sign-in hands the agent
	// container: the run's own folder holding the one token file the engine
	// writes and rewrites before it expires (ModelSignInToken), or the Claude
	// Code config directory the person's sign-in lives in
	// (ModelSignInConfigDir).
	PHModelSignIn = "@@RUN_MODEL_SIGNIN@@"
	// PHHostLinkUser is the user the host link runs as:
	// the invoking uid:gid, as the files image runs, or "" where the
	// machine has none (Windows), which leaves the image's own. The host
	// link's passwd file names it (ssh refuses a uid it has no entry for),
	// and its credential files stay the person's own, 0600.
	PHHostLinkUser = "@@RUN_HOSTLINK_USER@@"
	// PHBrowserUser is the user the browser server runs as:
	// the invoking uid:gid, as the host link runs, so its output folder and
	// its secrets file stay the person's own, 0700 and 0600; the image's
	// own user where the machine has none (Windows).
	PHBrowserUser = "@@RUN_BROWSER_USER@@"
	// PHBrowseProxy is the browse proxy's run-unique container name:
	// the browser server's one allowHosts entry and its
	// --proxy-server, known only to the run.
	PHBrowseProxy = "@@RUN_BROWSE_PROXY@@"
)

// PHTargetHost is a target host's relay target, address:port, by its question's
// id: the address the person's machine resolved the answer to, which only the
// run knows, so the plan carries the placeholder and its hash is the release's,
// not the run's.
func PHTargetHost(id string) string { return "@@RUN_HOST_" + id + "@@" }

// The two ways a model sign-in reaches the agent container
// (ModelSignInSpec.Delivery).
const (
	// ModelSignInToken: a short-lived access token in one file, in a
	// folder of the run's own mounted read-only at ModelSignInMount, so a
	// token rewritten by a rename is seen (a single-file bind would keep
	// the old inode). No refresh token ever enters the run.
	ModelSignInToken = "token-file"
	// ModelSignInConfigDir: the harness's own config directory, which only
	// the harness writes (Claude Code), mounted read-write. The
	// one exception to "no refresh token in a run".
	ModelSignInConfigDir = "config-dir"
	// ModelSignInCallback: the harness asks the session's driver for a token
	// over its own protocol, at the session's start and before the token
	// expires (Copilot CLI's token callback). Nothing is mounted and nothing is
	// in the environment: the access token passes the harness's stdio alone,
	// and no refresh token enters the run.
	ModelSignInCallback = "session-callback"
	// ModelSignInMount is the container folder a token file is in, and
	// ModelSignInTokenFile its name there.
	ModelSignInMount     = "/run/agentry/model-signin"
	ModelSignInTokenFile = "token"
)

// ModelSignInSpec says how a run spends a person's plan instead of a key: which
// kind of sign-in the plan was compiled for, which the engine holds the
// caller's grant to at run start, and how it reaches the agent container. The
// plan never carries the token, the account or a host path.
type ModelSignInSpec struct {
	// Kind is the policy's apiKeyKind, a model sign-in kind
	// (policy.IsModelSignIn), and Provider the provider it spends.
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	// Delivery is ModelSignInToken, ModelSignInConfigDir or
	// ModelSignInCallback.
	Delivery string `json:"delivery"`
	// Path is where the harness reads it in the container: the token
	// file's path, or the config directory's; empty for a callback.
	Path string `json:"path,omitempty"`
	// Env, on the restart token route, is the variable the harness is
	// started with the token in, read from that file when it starts
	// (policy signInTokenRoute restart); "" when the harness reads the
	// file itself.
	Env string `json:"env,omitempty"`
}

// ImagePin says how the harness image that ran was pinned:
// by "digest" (a published image, Value its sha256 digest), "artifact" (an
// image built on the machine, Value the vendor artifact's sha256) or "tag"
// (a policy naming docker-agent's harness image by tag alone, Value empty).
type ImagePin struct {
	By    string `json:"by"`
	Value string `json:"value,omitempty"`
	// Note is what the pin does not prove, when the build knows: an image
	// built from a download its vendor publishes a checksum of and does
	// not sign is "pinned by checksum, not signed".
	Note string `json:"note,omitempty"`
}

// String is the pin as a receipt line says it.
func (p *ImagePin) String() string {
	if p == nil {
		return ""
	}
	if p.Value == "" {
		return p.By + " (not pinned: the tag can move)"
	}
	if p.Note != "" {
		return p.By + " " + p.Value + " (" + p.Note + ")"
	}
	return p.By + " " + p.Value
}

// A remote server's credential reaches its bridge as a file: the engine writes
// it into ConnectionsDir/<server name>/BearerFile under the run's cfg
// directory, and the compiler mounts that folder, a folder and never the file
// alone so a value rewritten mid-run is seen, read-only at ConnectionMount. The
// gateway never holds it: it is not in the gateway's secrets file. A package
// server's OAuth sign-in uses the same folder at the same mount, holding the
// files its oauth block declares instead of BearerFile, mounted writable.
const (
	ConnectionsDir  = "connections"
	ConnectionMount = "/run/agentry/connection"
	BearerFile      = "bearer"
)

// SourceImage is an image server's Source: a built-in's server run from an
// image the CLI pins, its Package and Image the image, no version and no
// store; its Volumes are named volumes of the agent's that reach it alone.
const SourceImage = "image"

// SourceURL is a remote server's Source: no package, no version, no image
// and no store; the compiler runs the runner image's bridge for it.
const SourceURL = "url"

// MCPStorePrefix is the fixed in-container prefix a declared server's
// dependency closure is installed under AND mounted at — the same path in
// the builder and the runtime container, so interpreter paths (venv
// shebangs, .bin shims) stay valid.
const MCPStorePrefix = "/opt/agentry-mcp"

// Fixed container-side paths and network names shared by the compiler, the
// adapters, and the tools server configuration.
const (
	// WorkspaceMount is where tools see the workspace.
	WorkspaceMount = "/workspace"
	// OutMount is where tools see the output folder.
	OutMount = "/out"
	// InputsMount is where tools see the run-inputs channel.
	InputsMount = "/inputs"
	// InputsTextMount is where tools see the decoded inputs.
	InputsTextMount = "/inputs-text"
	// KnowledgeMount is where tools see the attached knowledge packs, each
	// at KnowledgeMount/<name>.
	KnowledgeMount = "/knowledge"
	// ScratchMount is the run's scratch folder in the tools container: empty at
	// the start, a tmpfs of ToolsTmpfsBytes, gone with the container.
	ScratchMount = "/scratch"
	// ToolsTmpMount is the tools container's /tmp, a tmpfs of the same
	// size: the place a declared command may write.
	ToolsTmpMount = "/tmp"
	// AgentRoot is where a persistent agent's tools see its folders,
	// AgentShareDir, AgentDataDir and AgentMemoryDir beneath it: the filesystem tools,
	// grep and glob are rooted here in place of the workspace, which a
	// persistent agent does not have, so one root holds every path they
	// take.
	AgentRoot = "/agent"
	// AgentShareDir is the person's share folder beneath AgentRoot,
	// read-write and live: what either side writes, the other sees.
	AgentShareDir = "share"
	// AgentShareOutDir is where an MCP server that sees the share saves a
	// file for it, beneath AgentRoot: the share outbox (PHShareOut), which
	// only those servers mount. The agent's own tools do not see it.
	AgentShareOutDir = "share-out"
	// AgentShareOutPath is the share outbox's path in a server's container.
	AgentShareOutPath = AgentRoot + "/" + AgentShareOutDir
	// ShareChecksLog is the receipt a persistent agent's segment appends a
	// line to for each file of the share it checks, and for each entry of
	// the share outbox it copies or refuses.
	ShareChecksLog = "share-checks.ndjson"
	// ShareOutMaxFiles, ShareOutFileMax and ShareOutTotalMax bound what the
	// runner copies from the share outbox into the share after one turn:
	// the files, the bytes of each, and the bytes of all of them. What is
	// past them is not copied, and the outbox is emptied all the same.
	ShareOutMaxFiles = 100
	ShareOutFileMax  = 64 << 20
	ShareOutTotalMax = 256 << 20
	// AgentDataDir is the agent's own folder beneath AgentRoot, its data
	// volume, kept between its segments.
	AgentDataDir = "data"
	// AgentSkillsDir is where the agent's skills are beneath AgentRoot,
	// read-only: one Agent Skills folder (a SKILL.md and what it uses) each.
	AgentSkillsDir = "skills"
	// AgentMemoryDir is the agent's memory beneath AgentRoot, a volume of
	// its own kept between its segments: a wiki the agent writes, with the
	// share as its sources.
	AgentMemoryDir = "memory"
	// DependencyDBMount is where the Coding agent's tools server sees
	// OSV's databases (PHDependencyDB), read-only.
	DependencyDBMount = "/run/agentry/osv-db"
	// ShareChecksMount is the folder a persistent agent's tools server
	// reads the share's malware verdicts from, read-only, rewritten by the
	// runner before each turn (ShareChecksFile).
	ShareChecksMount = "/run/agentry/share-checks"
	// ShareChecksFile is the verdicts' file name in that folder.
	ShareChecksFile = "verdicts.json"
	// PlanMount is where the agent container sees its harness files.
	PlanMount = "/plan"
	// TurnsMount is the folder the tools server reads the run's turn count
	// from (TurnsFile). The runner rewrites it at each turn's start and each
	// compaction, and the re-read stub stands only for a read answered
	// earlier in the same turn.
	TurnsMount = "/run/agentry/turns"
	// TurnsFile is the turn count's file name in that folder.
	TurnsFile = "turn"
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

// Plan is the whole compiled run plan. Every field is exported and
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
	// Source is "registry" or "local".
	Source string `json:"source"`
	// Modes are the run modes the release declares (manifest v2 modes; a
	// v1 skill or prompt-agent is [autonomous]).
	Modes []string `json:"modes"`
	// Mode is the run mode this plan was compiled for, one of Modes. A
	// release compiles to one plan per mode (plan/<mode>/); the plans are
	// identical except Harness.Agent.Argv/StdinTask and Harness.Session
	// (pinned by a golden test).
	Mode string `json:"mode"`
	// Summary is the manifest one-liner (harness description field).
	Summary string `json:"summary"`
	// PolicySHA256 fingerprints the operator policy the plan was compiled
	// under; a policy change invalidates installed plans.
	PolicySHA256 string `json:"policySha256"`
	// Instruction records where the harness instruction came from.
	Instruction InstructionSpec `json:"instruction"`
	// Capabilities is the normalized manifest contract.
	Capabilities CapSummary `json:"capabilities"`
	// Tools are the companion-server tools this agent is granted.
	Tools []string `json:"tools"`
	// Enforcement maps each contract line to the mechanism string that
	// lands verbatim in the result envelope (honest-posture rule).
	Enforcement map[string]string `json:"enforcement"`
	// Gateway describes the gateway container launch.
	Gateway GatewaySpec `json:"gateway"`
	// ExecImage is the tools container's image when the manifest declares
	// exec: the runner image plus the operator's catalogued packages,
	// minimized. Nil when no exec is declared.
	ExecImage *ExecImageSpec `json:"execImage,omitempty"`
	// HostLink is the host link's image, where the release declares a
	// target host: built on this machine from the pinned
	// base and the runner image, as the files image is.
	HostLink *HostLinkSpec `json:"hostLink,omitempty"`
	// Browser is the browser server's image, where the release declares
	// capabilities.browser: pulled by digest, never built.
	Browser *BrowserSpec `json:"browser,omitempty"`
	// Render is the page renderer, for a plan with CapSummary.WebRender.
	Render *RenderSpec `json:"render,omitempty"`
	// Harness describes the adapter and the agent container.
	Harness HarnessSpec `json:"harness"`
	// Proxy describes the LLM egress proxy sidecar.
	Proxy ProxySpec `json:"proxy"`
	// Docker is the daemon-facing part of the policy the plan was compiled
	// under; absent when the policy set none, so a plan compiled before the
	// knob and one compiled without it are the same bytes.
	Docker *DockerSpec `json:"docker,omitempty"`
	// Secrets lists the manifest secret env names (values are resolved at
	// run time and never stored).
	Secrets []string `json:"secrets,omitempty"`
	// OptionalSecrets are the declared secrets, the release's own or a
	// declared server's, that its manifest marks optional
	// (capabilities.optionalSecrets): a run starts without
	// one it has no value for and the gateway sets it empty. Absent when the
	// manifest marks none, so such a plan carries no trace of the field.
	OptionalSecrets []string `json:"optionalSecrets,omitempty"`
	// InputView is the view the input tools serve an input in when the model
	// asks for none, from the manifest's inputs.view: "text" or "original".
	// Absent for a release that declares neither, whose inputs keep their own
	// defaults, so such a plan carries no trace of the field.
	InputView string `json:"inputView,omitempty"`
	// MCPServers are the declared, resolved MCP servers this plan loads
	// alongside the tools server. Empty for agents that
	// declare none, whose plans carry no trace of the field.
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
	// Budgets are the compiled defaults; run flags replace them, a
	// built-in package's run on this computer starts from its own
	// (localrun.Builtin.LocalBudgets), and the receipt records what
	// actually applied.
	Budgets Budgets `json:"budgets"`
	// Output describes the optional structured-output convention.
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
	// SHA256 is the hash of the fully rendered instruction (the System
	// Prompt, then the body), for receipts.
	SHA256 string `json:"sha256"`
	// BodySHA256 is the hash of the package's own instruction text alone,
	// frontmatter stripped: what ReadInstruction holds the file to.
	BodySHA256 string `json:"bodySha256,omitempty"`
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
	// WebTools is a built-in's grant of the web tools (web_search, web_read),
	// the Drafter's and the Knowledge Creator's. It is never the manifest's:
	// the compiler sets it for a built-in package the CLI grants it to, and
	// refuses it to everything else.
	WebTools bool `json:"webTools,omitempty"`
	// RegistrySearch adds registry_search to WebTools: the Drafter's alone.
	RegistrySearch bool `json:"registrySearch,omitempty"`
	// WebSearchedOnly holds web_read to the addresses a web_search of the
	// run returned: the Knowledge Creator's.
	WebSearchedOnly bool `json:"webSearchedOnly,omitempty"`
	// WebGivenOnly holds web_read to the addresses the person gave the run,
	// with no web_search: the Voice Writer's, reading a person's own pages.
	WebGivenOnly bool `json:"webGivenOnly,omitempty"`
	// WebRender lets web_read render a page in a headless browser the
	// engine runs once a page, behind a proxy that admits public sites
	// alone, and read the text the page draws: the Knowledge Creator's.
	// The model never drives that browser. Only with WebTools.
	WebRender bool `json:"webRender,omitempty"`
	// ReviewTools is a built-in's review grant (ai_review, and read_input
	// answering only what AI Review passed). Like WebTools it is never the
	// manifest's: the compiler sets it for a built-in package the CLI grants it
	// to, and refuses it to everything else.
	ReviewTools bool `json:"reviewTools,omitempty"`
	// TargetHosts are the release's target hosts, by the id of the host
	// question that asks for each (capabilities.targetHosts): the host link and
	// its tools, reached through the gateway.
	TargetHosts []string `json:"targetHosts,omitempty"`
	// Browser is the release's browser line (capabilities.browser): the browser
	// server and the browse proxy that holds its scope.
	Browser *BrowserCap `json:"browser,omitempty"`
	// Scratch is the runner's grant of the scratch folder (scratch_list,
	// scratch_read, scratch_write over ScratchMount), and of grep and glob, to
	// every run and session, a built-in package's included. It is never the
	// manifest's.
	Scratch bool `json:"scratch,omitempty"`
	// PDF is the runner's grant of export_pdf to every release's run and
	// session. It is never the manifest's, and never a built-in's: what a
	// built-in writes is read by the application, not handed to a person as
	// a document.
	PDF bool `json:"pdf,omitempty"`
	// AgentFiles is a persistent agent's grant of its own folders under
	// AgentRoot: its data volume always, and the person's share folder when
	// the agent has it on. It is never the manifest's: the compiler sets it
	// for a persistent built-in alone, and the filesystem tools, grep and
	// glob are then rooted at AgentRoot in place of the workspace.
	AgentFiles *AgentFiles `json:"agentFiles,omitempty"`
	// AppLink is a persistent built-in's app link, by the tool set it lists
	// (runnerimage/applink): the gateway runs it with no network and the
	// supervisor's socket alone. Never the manifest's: the compiler sets it
	// for a persistent built-in alone.
	AppLink string `json:"appLink,omitempty"`
	// Coder is the Coding agent's grant, what its roster has on: any
	// program in its own exec image, open egress, the browser on any public
	// site and the person's dev server. Never the manifest's: the compiler
	// sets it for a persistent built-in the CLI grants it to alone, and the
	// person's agreement to it for this agent is the gate, not the machine
	// policy's allowWildcardEgress or browser.allowPublicWeb.
	Coder *CoderGrant `json:"coder,omitempty"`
	// Calendar is a persistent Assistant's calendar feeds, what its roster
	// has on: the tools server is given each feed's address and reaches the
	// feeds' hosts on 443 alone. Never the manifest's: the compiler sets it
	// for a persistent built-in the CLI grants it to alone.
	Calendar *CalendarGrant `json:"calendar,omitempty"`
}

// CoderGrant is the Coding agent's grant (CapSummary.Coder), each part
// one switch of its roster.
type CoderGrant struct {
	// Commands: run_command runs any program in the Coding agent's exec
	// image (ExecImageSpec.Coder), in a folder beneath AgentRoot.
	Commands bool `json:"commands,omitempty"`
	// OpenEgress: the tools server, and the programs it runs, reach any
	// address, from a network of the segment's own.
	OpenEgress bool `json:"openEgress,omitempty"`
	// Browser: the browser server, on any public site.
	Browser bool `json:"browser,omitempty"`
	// DevHost: the person's dev server, as the target host DevHostID.
	DevHost bool `json:"devHost,omitempty"`
	// Ports: the tools server's ports published on the person's computer,
	// each at 127.0.0.1 and the same number, while its entry is on.
	Ports []CoderPort `json:"ports,omitempty"`
	// Secrets: the names of the person's stored secrets its roster has on,
	// set in the environment of the programs run_command runs (with
	// Commands); their values are the segment's, never the plan's.
	Secrets []string `json:"secrets,omitempty"`
	// DependencyScan: dependency_scan, OSV-Scanner offline over OSV's
	// databases on the person's computer (with Commands).
	DependencyScan bool `json:"dependencyScan,omitempty"`
	// Accounts: the person's git host accounts its roster has on, whose
	// tokens the programs run_command runs sign in with (with Commands);
	// the tokens are the segment's, never the plan's.
	Accounts []CoderAccount `json:"accounts,omitempty"`
}

// CoderPort is one of the Coding agent's published ports.
type CoderPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// DevHostID is the target host id the Coding agent's dev server is readied
// and reached as.
const DevHostID = "dev"

// CoderHome is HOME for the programs the Coding agent runs: a hidden folder
// of its data volume, so their caches and settings last between segments.
const CoderHome = AgentRoot + "/" + AgentDataDir + "/.home"

// AgentFiles is a persistent agent's folders (CapSummary.AgentFiles).
type AgentFiles struct {
	// Share says the person's share folder is mounted at
	// AgentRoot/AgentShareDir.
	Share bool `json:"share,omitempty"`
	// CheckShare says a file in the share is read only once the runner has
	// checked it: the runner runs what is new or changed in the share
	// through the decode step before each turn and writes the verdicts, and
	// the text read out of each document, that the tools server reads at
	// ShareChecksMount; the tools server refuses a file whose bytes have no
	// verdict, or one ClamAV found, and serves a document by its text.
	// Unset, the share is read as it is.
	CheckShare bool `json:"checkShare,omitempty"`
	// MalwareCheck says that check runs ClamAV over each file: the policy's
	// malwareCheck when the plan was compiled. Unset, the check reads the
	// files without ClamAV and every file is read unchecked.
	MalwareCheck bool `json:"malwareCheck,omitempty"`
	// Skills are the skills the agent has on, by name and description, in
	// the order the System Prompt lists them; their folders are mounted
	// read-only at AgentRoot/AgentSkillsDir.
	Skills []Skill `json:"skills,omitempty"`
	// Memory says the agent's memory volume is mounted read-write at
	// AgentRoot/AgentMemoryDir.
	Memory bool `json:"memory,omitempty"`
}

// Skill is one skill a persistent agent has: its folder's name beneath
// AgentRoot/AgentSkillsDir and the description its SKILL.md gives.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ReadFolders are the folders beneath AgentRoot the agent's tools read:
// its folders, and its skills, which are read-only.
func (a *AgentFiles) ReadFolders() []string {
	f := a.Folders()
	if a != nil && len(a.Skills) > 0 {
		f = append(f, AgentSkillsDir)
	}
	return f
}

// Folders are the folders beneath AgentRoot the agent's tools read and
// write, in a fixed order.
func (a *AgentFiles) Folders() []string {
	if a == nil {
		return nil
	}
	f := []string{AgentDataDir}
	if a.Share {
		f = append(f, AgentShareDir)
	}
	if a.Memory {
		f = append(f, AgentMemoryDir)
	}
	return f
}

// BrowserCap is capabilities.browser, normalized: the site questions, the
// fixed hosts (lower case), whether the scope is any public site, and the
// declared secrets the browser may type.
type BrowserCap struct {
	Sites   []string `json:"sites,omitempty"`
	Hosts   []string `json:"hosts,omitempty"`
	Public  bool     `json:"public,omitempty"`
	Secrets []string `json:"secrets,omitempty"`
	// Agreed says the scope is the Coding agent's own, which the person
	// agreed to for that agent, not a release's declaration that the
	// machine policy allowed.
	Agreed bool `json:"agreed,omitempty"`
}

// BrowserSpec is the browser server's image, a digest-pinned reference.
type BrowserSpec struct {
	Image string `json:"image"`
}

// RenderSpec is a run's page renderer (CapSummary.WebRender): the browser
// image the engine runs once a page, never a server the model can call.
type RenderSpec struct {
	Image string `json:"image"`
}

// The page renderer's hand-off folder: under the run's cfg on the host,
// bound read-write into the tools server alone at RenderMount, where
// web_read leaves a request and the engine its answer.
const (
	RenderDir   = "render"
	RenderMount = "/render"
)

// The run folders the browser server reads and writes, under the run's cfg.
const (
	// BrowserOutDir is the browser server's output folder, bound
	// read-write into it alone and read-only into the tools server at
	// BrowserOutMount; the engine copies it into reports/browser/ when the
	// run ends. It is a folder of its own, not reports/browser/ itself:
	// the tools server writes reports/, and the docker filter binds no
	// folder inside one a container writes.
	BrowserOutDir = "browserout"
	// BrowserDir holds the server's config and, for a line that declares
	// browser secrets, the --secrets file (0600), bound read-only into the
	// browser server alone.
	BrowserDir = "browser"
	// BrowserSecretsFile is that file's name in BrowserDir.
	BrowserSecretsFile = "secrets.env"
	// BrowserReports is where the browser's files reach the person, under
	// the run's output folder: reports/browser/.
	BrowserReports = "reports/browser"
)

// BrowserOutMount is where the tools server reads the browser's files:
// inside its output folder, where reports/browser/ is, so a report can name
// them and export_pdf can draw them.
const BrowserOutMount = OutMount + "/" + BrowserReports

// HostLinkSpec is the host link's image: the local tag the gateway's
// catalog names, and the digest-pinned base it is built on.
type HostLinkSpec struct {
	Image string `json:"image"`
	Base  string `json:"base"`
}

// The run folders the host link reads and writes, under the run's cfg.
const (
	// HostsDir holds one folder per target host (credential, known_hosts,
	// target.json) and the passwd file, bound read-only into the host
	// link alone.
	HostsDir = "hosts"
	// HostLogDir holds each target host's call log, which the engine
	// copies into the receipts.
	HostLogDir = "hostlog"
	// HostFilesDir is the folder host_put and host_get copy through,
	// shared with the tools server at /scratch/hosts.
	HostFilesDir = "hostfiles"
)

// ToolsCmd is the one command the tools server runs with in its container:
// the runner image's tools role reading its spec. The compiler writes it
// into the gateway's catalog, and the docker filter knows the tools server
// by it (the web network and the bounded tmpfs), so there is one copy.
func ToolsCmd() []string { return []string{"tools", "--spec", ToolSpecMount} }

// ToolsImage is the image the plan's tools server runs in: the
// QuickAgent's exec image when it declares commands, the
// runner image otherwise. The compiler chooses it; the engine reads it here.
func (p *Plan) ToolsImage() string {
	if p.ExecImage != nil && p.ExecImage.Tag != "" {
		return p.ExecImage.Tag
	}
	return p.Proxy.Image
}

// ToolsTmpfsBytes bounds each of the tools container's two tmpfs mounts, /tmp
// and the scratch folder. Each counts against the container's memory limit, and
// a write past it fails with ENOSPC.
const ToolsTmpfsBytes = 256 << 20

// Fetches reports whether the contract grants fetch: egress to named
// hosts, or to any.
func (c CapSummary) Fetches() bool { return c.EgressMode == "hosts" || c.EgressMode == "all" }

// ToolsReachPastHost reports whether the tools server reaches past the
// host on a network of the run's own, with no host list for the gateway to
// hold it to: for the web tools, whose tools hold their own address rule,
// or for unrestricted egress, which the operator's policy permitted.
func (c CapSummary) ToolsReachPastHost() bool { return c.WebTools || c.EgressMode == "all" }

// GatewaySpec is the gateway container launch description. Flags are
// compiled, not chosen at run time, so tests can assert --block-network is
// always present.
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

// IsOptionalSecret reports whether the plan's manifest marks the declared
// secret name optional: a run starts without it.
func (p *Plan) IsOptionalSecret(name string) bool {
	return slices.Contains(p.OptionalSecrets, name)
}

// HarnessSpec names the adapter and carries the agent container spec.
type HarnessSpec struct {
	// Adapter is the harness adapter name.
	Adapter string `json:"adapter"`
	// Model is the provider/model string for receipts.
	Model string `json:"model"`
	// Agent is the agent container. Empty (no image) when Host is set.
	Agent ContainerSpec `json:"agent"`
	// Host runs the harness as a process on this machine from the person's own
	// installation instead of a container. Exactly one of Agent.Image and Host
	// is set.
	Host *HostProcessSpec `json:"host,omitempty"`
	// ResultKind hints how the final result is extracted: "text" or
	// "json" (when Output.SchemaFile is set).
	ResultKind string `json:"resultKind"`
	// Session says how the harness is driven when Mode is interactive;
	// nil for an autonomous plan.
	Session *SessionSpec `json:"session,omitempty"`
	// OneTurn, on an autonomous plan, says the harness is driven as one turn of
	// this session protocol rather than read from its stdout: the engine opens
	// the session, sends the task as its one user turn and ends it (the codex
	// adapters on a model sign-in, whose vendor serves app-server's requests
	// and not exec's). Nil for a harness whose autonomous run prints its
	// events.
	OneTurn *SessionSpec `json:"oneTurn,omitempty"`
	// ImagePin is how Agent.Image is pinned; nil for a host
	// harness, which runs no image.
	ImagePin *ImagePin `json:"imagePin,omitempty"`
	// ModelSignIn is set when the run spends a person's plan through a
	// model sign-in instead of a key; nil for a key.
	ModelSignIn *ModelSignInSpec `json:"modelSignIn,omitempty"`
}

// SessionSpec is the interactive drive of the harness: the runner is the Agent
// Client Protocol client on the agent container's attached stdio. Everything
// else about the container is the autonomous plan's.
type SessionSpec struct {
	// Transport is "acp-stdio".
	Transport string `json:"transport"`
	// ProtocolVersion is the ACP protocol version the client negotiates.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionDB is the container path of the harness's session store.
	SessionDB string `json:"sessionDb"`
	// Mounts are added to the agent container for a session only: the
	// writable session store under PHSession, which the engine resolves to
	// a per-run named volume.
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
	// Budgets are the session caps the engine enforces (tightened by the
	// manifest's interactive hints).
	Budgets SessionBudgets `json:"budgets"`
	// RestartOnRenewal says the harness reads its model sign-in's access
	// token once, when it starts: before a turn, when the runner has
	// renewed the token since, the engine starts the harness again and the
	// driver resumes the conversation (policy signInTokenRoute restart).
	RestartOnRenewal bool `json:"restartOnRenewal,omitempty"`
	// ConversationArgs, for a harness told its conversation on its command
	// line, are the flags the engine adds to the agent's argv at each start
	// of a persistent agent's segment. Nil for a harness that names its
	// conversation over the session itself, and for every other plan.
	ConversationArgs *ConversationArgs `json:"conversationArgs,omitempty"`
}

// ConversationArgs are the two ways a harness started on its command line
// is told its conversation: each flag is followed by the conversation's id.
type ConversationArgs struct {
	// New opens a new conversation under an id the engine chooses.
	New string `json:"new"`
	// Continue continues the stored conversation of that id.
	Continue string `json:"continue"`
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

// ProxySpec is the LLM egress proxy sidecar.
type ProxySpec struct {
	// Image is the locally built runner image tag.
	Image string `json:"image"`
	// Allow is the host:port allowlist (policy model endpoints only).
	Allow []string `json:"allow"`
	// PlainHTTP is the subset of Allow the proxy may forward plain http
	// to: the http model endpoints the operator's policy names. Empty for
	// a run whose model endpoints are all https.
	PlainHTTP []string `json:"plainHttp,omitempty"`
}

// DockerSpec is what the engine asks of the daemon for every container of
// the run beyond the image: the OCI runtime (docker run --runtime).
type DockerSpec struct {
	Runtime string `json:"runtime"`
	// GatewayRuntime is the gateway's own runtime (policy
	// docker.gatewayRuntime); absent, the daemon's default.
	GatewayRuntime string `json:"gatewayRuntime,omitempty"`
	// Resolvers are the name servers the web tools resolve through (policy
	// docker.resolvers), and under Runtime the classes that resolve host names;
	// absent, docker's DNS.
	Resolvers []string `json:"resolvers,omitempty"`
}

// Conversational says the plan runs as a conversation the harness serves on
// its stdio (Harness.Session): an interactive plan, or a persistent one,
// whose harness half is the interactive one and whose conversation outlives
// each start of it.
func (p *Plan) Conversational() bool {
	return p != nil && (p.Mode == "interactive" || p.Mode == "persistent")
}

// PaysWithCredits says the plan's model is paid for with Agentry credits:
// its harness reads a credits session token (EnvVar.FromInference), which
// the host must open before the run starts.
func (p *Plan) PaysWithCredits() bool {
	if p == nil {
		return false
	}
	for _, v := range p.Harness.Agent.Env {
		if v.FromInference {
			return true
		}
	}
	if h := p.Harness.Host; h != nil {
		for _, v := range h.Env {
			if v.FromInference {
				return true
			}
		}
	}
	return false
}

// Runtime is the OCI runtime the plan asks for, or "" for the daemon's
// default.
func (p *Plan) Runtime() string {
	if p == nil || p.Docker == nil {
		return ""
	}
	return p.Docker.Runtime
}

// GatewayRuntime is the OCI runtime the plan asks for the gateway, or ""
// for the daemon's default.
func (p *Plan) GatewayRuntime() string {
	if p == nil || p.Docker == nil {
		return ""
	}
	return p.Docker.GatewayRuntime
}

// SandboxResolvers are the name servers the run's containers under its runtime
// that resolve host names read: the llm-proxy, the MCP server install proxy and
// the relays. None without a runtime, or without resolvers, and those classes
// then stay on the daemon's default.
func (p *Plan) SandboxResolvers() []string {
	if p.Runtime() == "" {
		return nil
	}
	return p.Docker.Resolvers
}

// WebResolvers are the name servers the plan's tools server resolves
// through when it reaches past the host (CapSummary.ToolsReachPastHost),
// or none for docker's embedded DNS: only such a plan reads them here (the
// compiler also records them under a runtime, for SandboxResolvers).
func (p *Plan) WebResolvers() []string {
	if p == nil || p.Docker == nil || !p.Capabilities.ToolsReachPastHost() {
		return nil
	}
	return p.Docker.Resolvers
}

// ContainerSpec is a docker container the engine will run. Sources and env
// values may contain the PH* placeholders.
type ContainerSpec struct {
	Image string `json:"image"`
	// Entrypoint replaces the image's entrypoint; empty keeps it. The codex
	// image's runs start a shell that puts the plan's configuration into
	// the harness's writable home before the harness starts.
	Entrypoint string `json:"entrypoint,omitempty"`
	// Argv is the command+args passed after the image (entrypoint's argv).
	Argv []string `json:"argv,omitempty"`
	Env  []EnvVar `json:"env,omitempty"`
	// Mounts are bind mounts (host source may hold placeholders).
	Mounts []Mount `json:"mounts,omitempty"`
	// StdinTask: the engine pipes the task text into stdin.
	StdinTask bool `json:"stdinTask,omitempty"`
	// ReadOnlyRoot runs the container with a read-only root filesystem and
	// Tmpfs as its only writable paths besides its mounts. The adapter sets
	// both, since where a harness writes is the image's to say.
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
	// time (e.g. the operator's model API key).
	FromHostEnv string `json:"fromHostEnv,omitempty"`
	// FromKeyring, beside FromHostEnv, names the OS keychain account read
	// when that variable is not set (policy apiKeySource keyring):
	// the account, never the value.
	FromKeyring string `json:"fromKeyring,omitempty"`
	// FromRunToken injects the per-run gateway Bearer token.
	FromRunToken bool `json:"fromRunToken,omitempty"`
	// FromModelSignIn injects the run's model sign-in access token as it
	// is when the process starts (a host harness on the restart token
	// route, which reads its token from the environment): the access token
	// alone, never the sign-in.
	FromModelSignIn bool `json:"fromModelSignIn,omitempty"`
	// FromInference injects the run's Agentry credits session token (model
	// provider agentry): a token usable only on the registry's model
	// endpoint, which the host opens for the run and renews while it
	// lives. Never the person's registry sign-in.
	FromInference bool `json:"fromInference,omitempty"`
	// ExpectedKind, beside FromHostEnv, is the credential kind the plan was
	// compiled for (policy apiKeyKind): the engine checks the value it resolves
	// against it and refuses a mismatch by name, so a plan rendered for a
	// subscription token never runs with an API key and the reverse — the kind
	// decides the harness's argv and the variable it reads, which the plan
	// fixed at compile time. A consistency check between the policy and the
	// key, not a boundary: plan.json is the operator's own file. Nil when the
	// provider has one kind of key.
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

// Budgets are the hard stops.
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

// MCPServerSpec is one declared MCP server, resolved to its exact artifacts.
// The store contents live under the signature's profile — per-machine — so the
// plan carries the RESOLUTION (source, version, digest, launch argv), which is
// deterministic given the manifest pins, and mounts the store via the PHServers
// placeholder.
type MCPServerSpec struct {
	// Name is the gateway catalog name ("mcp-" + sanitized package id).
	Name string `json:"name"`
	// Source is "npm" or "pypi"; Package and Version are the manifest pin. For
	// a remote server Source is SourceURL, Package the server's address, and
	// Version, Integrity, Image, Command and StoreSubdir are empty: the
	// compiler runs the runner image's bridge.
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
	// Volumes are an image server's named volumes, each name:target, which
	// the docker filter holds to this server's container alone; empty for
	// any other server.
	Volumes []string `json:"volumes,omitempty"`
	// Args are the manifest's arguments for the server, appended to
	// Command in its catalog entry, each one argument as written: the
	// entry is a list, never a shell line.
	Args []string `json:"args,omitempty"`
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
	// Auth is a remote server's auth kind: "none"; "secret", its one Secrets
	// name sent as the bearer; or "oauth", the person's connection, whose
	// access token the runner hands its bridge at each start. Empty for a
	// package server.
	Auth string `json:"auth,omitempty"`
	// OAuth is a package server's OAuth sign-in: the issuer and scopes its
	// connection is made with, and the files and env the runner renders the
	// connection's access token into. Nil for a remote server and for a package
	// server without one.
	OAuth *OAuthSpec `json:"oauth,omitempty"`
	// Share is how the server sees a persistent agent's share folder, read
	// only, at AgentRoot/AgentShareDir: ServerShareMirror, the copies the
	// share check passed, or ServerShareLive, the folder itself. Empty for
	// a server that does not see it, which is every server but a package
	// server the person let see it, with no network.
	Share ServerShare `json:"share,omitempty"`
	// Account is the account an image server is signed in as, as the
	// server named it at its sign-in: the System Prompt names it, so the
	// agent can say whose mailbox or calendar it read. Empty for every
	// other server.
	Account string `json:"account,omitempty"`
}

// ServerShare is how a server sees a persistent agent's share folder
// (MCPServerSpec.Share).
type ServerShare string

// The ways a server sees the share.
const (
	// ServerShareMirror: the share mirror (PHShareMirror), for an agent
	// whose share is checked.
	ServerShareMirror ServerShare = "mirror"
	// ServerShareLive: the share folder itself, read only, for the Coding
	// agent, whose share is read as it is.
	ServerShareLive ServerShare = "live"
)

// SharesAny reports whether any server sees the share, however: each such
// server also saves into the share outbox.
func SharesAny(servers []MCPServerSpec) bool {
	for _, s := range servers {
		if s.Share != "" {
			return true
		}
	}
	return false
}

// SharesAs reports whether any server sees the share as how.
func SharesAs(servers []MCPServerSpec, how ServerShare) bool {
	for _, s := range servers {
		if s.Share == how {
			return true
		}
	}
	return false
}

// OAuthSpec is a package server's oauth block as the plan carries it: the
// manifest's templates, never a token. The engine renders Files into the
// server's connection folder (mounted writable at ConnectionMount) and the
// Env entries holding {access_token} into the gateway's secrets file; an
// entry naming only {dir} is plain env, rendered by the compiler.
type OAuthSpec struct {
	Issuer string            `json:"issuer"`
	Scopes []string          `json:"scopes,omitempty"`
	Files  []OAuthFile       `json:"files,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
}

// OAuthFile is one file of a package server's connection folder.
type OAuthFile struct {
	Path     string `json:"path"`
	Template string `json:"template"`
}

// OAuthTokenPlaceholder is the placeholder an oauth template or env value
// names the access token with (the manifest's grammar).
const OAuthTokenPlaceholder = "{access_token}"

// TokenEnv reports whether an oauth env entry carries the access token,
// which makes it a secret of the run rather than plain configuration.
func (o *OAuthSpec) TokenEnv(name string) bool {
	return o != nil && strings.Contains(o.Env[name], OAuthTokenPlaceholder)
}

// SourceID is the declared server's ecosystem-qualified id, as the
// manifest's MCPServer.SourceID gives it ("npm:@scope/x", "url:https://…").
func (s MCPServerSpec) SourceID() string { return s.Source + ":" + s.Package }

// Remote reports whether the server is declared by address.
func (s MCPServerSpec) Remote() bool { return s.Source == SourceURL }

// FromImage reports whether the spec is an image server's: a built-in's
// server run from an image the CLI pins, with no package and no store.
func (s MCPServerSpec) FromImage() bool { return s.Source == SourceImage }

// ImageServers are the image servers among servers.
func ImageServers(servers []MCPServerSpec) []MCPServerSpec {
	var out []MCPServerSpec
	for _, s := range servers {
		if s.FromImage() {
			out = append(out, s)
		}
	}
	return out
}

// SendsBearer reports whether the server is a remote one whose bridge
// sends a bearer from its connection folder: auth: secret (its one secret)
// or auth: oauth (the connection's access token).
func (s MCPServerSpec) SendsBearer() bool {
	return s.Remote() && (s.Auth == "secret" || s.Auth == "oauth")
}

// SignsIn reports whether a run of the server needs the person's OAuth
// connection: a remote server signed in to by OAuth (manifest.AuthOAuth),
// or a package server with an oauth block.
func (s MCPServerSpec) SignsIn() bool {
	if s.Remote() {
		return s.Auth == "oauth"
	}
	return s.OAuth != nil
}

// ConnectionKey is how a run's connections name the server: a remote
// server's address, or a package server's ecosystem-qualified id
// ("pypi:workspace-mcp"), as the manifest's MCPServer.SourceID gives it.
func (s MCPServerSpec) ConnectionKey() string {
	if s.Remote() {
		return s.Package
	}
	return s.SourceID()
}

// Ref names the server in messages and receipts: the package pin
// ("@scope/x@1.2.3"), or a remote server's address.
func (s MCPServerSpec) Ref() string {
	if s.Remote() || s.FromImage() {
		return s.Package
	}
	return s.Package + "@" + s.Version
}

// RemoteHost is a remote server's host name, its one egress host; "" for
// a package server.
func (s MCPServerSpec) RemoteHost() string {
	if !s.Remote() {
		return ""
	}
	u, err := url.Parse(s.Package)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// HasPackageServers reports whether any declared server is a package,
// installed into the signature's server store.
func HasPackageServers(servers []MCPServerSpec) bool {
	for _, s := range servers {
		if !s.Remote() && !s.FromImage() {
			return true
		}
	}
	return false
}

// OutputSpec is the optional structured-output convention.
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
// run_plan_sha256 every receipt carries.
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

// ExecImageSpec is the per-QuickAgent exec image a plan runs its tools server
// in when the manifest declares exec: the runner image with the operator's
// catalogued packages installed, then minimized to exactly the declared
// commands.
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
	// Unsafe is set for an Unsafe Mode exec image: built FROM the unsafe
	// base rather than the runner image, with Packages as Kali packages
	// installed one at a time, the extras applied, and no minimize.
	// Nil for the catalogue's exec image, whose plan bytes it leaves
	// unchanged.
	Unsafe *UnsafeExecSpec `json:"unsafe,omitempty"`
	// Coder is set for the Coding agent's exec image: its toolchain image,
	// pinned by digest (policy images.coder), with the runner binary
	// copied in, not minimized, and no command declared or installed.
	Coder string `json:"coder,omitempty"`
}

// ExecNotInstalledPrefix marks an entry of an exec image build's result
// that did not install (an Unsafe Mode build goes on without it), so
// the receipt names it apart from what did.
const ExecNotInstalledPrefix = "not installed: "

// UnsafeExecSpec is what an Unsafe Mode exec image adds to the catalogue's:
// the base it is built from and how each declared command was placed.
// This machine's exec catalogue was not consulted.
type UnsafeExecSpec struct {
	// Base is the unsafe base image, pinned by digest (policy
	// images.unsafe), whose command index and extras list placed the
	// commands.
	Base string `json:"base"`
	// Apt are the declared commands the base's index placed, each with its
	// Kali package, sorted by command: a package that fails to install
	// names the commands it leaves absent.
	Apt []AptPlacement `json:"apt,omitempty"`
	// Extras are the declared commands placed by the base's extras list,
	// sorted by command.
	Extras []UnsafeExtra `json:"extras,omitempty"`
	// Unplaced are the declared commands neither the extras list nor the
	// index could place: the run goes ahead without them, and the
	// plan, the consent and the receipt name them. Sorted by command.
	Unplaced []UnplacedCommand `json:"unplaced,omitempty"`
}

// AptPlacement is a declared command and the Kali package the index says
// ships it.
type AptPlacement struct {
	Command string `json:"command"`
	Package string `json:"package"`
}

// ExtraKind is an extras-list recipe's kind; there are exactly two.
type ExtraKind string

const (
	// ExtraPip installs a PyPI name==version into a venv and links its
	// command onto PATH.
	ExtraPip ExtraKind = "pip"
	// ExtraLink links a path inside a Kali package onto PATH.
	ExtraLink ExtraKind = "link"
)

// UnsafeExtra is one extras-list recipe for a declared command.
type UnsafeExtra struct {
	Command string    `json:"command"`
	Kind    ExtraKind `json:"kind"`
	// Spec is the pip requirement, name==version.
	Spec string `json:"spec,omitempty"`
	// Package and Path are a link's package and its path inside it.
	Package string `json:"package,omitempty"`
	Path    string `json:"path,omitempty"`
}

// String is the recipe as the plan and the receipt name it.
func (e UnsafeExtra) String() string {
	switch e.Kind {
	case ExtraPip:
		return "pip " + e.Spec
	case ExtraLink:
		return "link " + e.Package + ":/" + e.Path
	}
	return string(e.Kind) + " (unknown)"
}

// UnplacedCommand is a declared command Unsafe Mode's image cannot place,
// with the packages that ship it when there are several (the base's
// ambiguous list), so the plan can say why.
type UnplacedCommand struct {
	Command    string   `json:"command"`
	Candidates []string `json:"candidates,omitempty"`
}

// ExecImageTag names the per-QuickAgent exec image: the runner image with the
// catalogued packages installed and minimized to exactly the declared commands.
// Deterministic in (runner image, sorted commands, sorted packages, mirror) so
// the compiler can reference it before the install step builds it, and so a
// changed catalog entry or mirror is a new image. "" when no exec is declared —
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
	return execImageName(runnerImage, "", sum)
}

// UnsafeExecImageTag names an Unsafe Mode exec image: deterministic in the base
// (pinned by digest), the runner image whose binary it copies, the sorted
// commands, the sorted packages and the extras, so a re-pin of the base or a
// changed index or extras list is a new image. It lives in the same repository
// as the catalogue's exec images (<runner>-exec), under a "u" tag, so the
// desktop's image removal (internal/dockerimages.Select in agentry-desktop)
// takes it with them.
func UnsafeExecImageTag(runnerImage string, u UnsafeExecSpec, commands, packages []string) string {
	if len(commands) == 0 {
		return ""
	}
	sorted := append([]string(nil), commands...)
	sort.Strings(sorted)
	pkgs := append([]string(nil), packages...)
	sort.Strings(pkgs)
	extras := make([]string, 0, len(u.Extras))
	for _, e := range u.Extras {
		extras = append(extras, e.Command+"="+e.String())
	}
	sort.Strings(extras)
	sum := sha256.Sum256([]byte("unsafe\x00" + u.Base + "\x00" + runnerImage + "\x00" + strings.Join(sorted, "\x00") +
		"\x01" + strings.Join(pkgs, "\x00") + "\x01" + strings.Join(extras, "\x00")))
	return execImageName(runnerImage, "u", sum)
}

// CoderExecImageTag names the Coding agent's exec image: deterministic in its
// toolchain image (pinned by digest) and the runner image whose binary it
// copies, under a "c" tag in the catalogue's exec repository, as Unsafe Mode's
// is under "u".
func CoderExecImageTag(runnerImage, base string) string {
	sum := sha256.Sum256([]byte("coder\x00" + base + "\x00" + runnerImage))
	return execImageName(runnerImage, "c", sum)
}

// execImageName is <runner repository>-exec:<runner tag>-<prefix><hash>.
func execImageName(runnerImage, prefix string, sum [32]byte) string {
	name := runnerImage
	if i := strings.LastIndex(name, ":"); i > 0 && !strings.Contains(name[i:], "/") {
		return name[:i] + "-exec:" + name[i+1:] + "-" + prefix + hex.EncodeToString(sum[:6])
	}
	return name + "-exec:" + prefix + hex.EncodeToString(sum[:6])
}

// HostProcessSpec runs the harness as a process of this user from the person's
// own installation of it. The gateway and the proxy are published on 127.0.0.1
// for it; their addresses reach it as HostGatewayURLEnv / HostProxyURLEnv and,
// in plan files and argv, through the PHHostGatewayURL / PHHostProxyURL
// placeholders. The process is not inside the run's network boundary: the proxy
// variables and the gateway's catalog are what bound it, which the plan's
// Enforcement map says in so many words.
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
	// measured rather than assumed.
	RefuseAncestorFiles []string `json:"refuseAncestorFiles,omitempty"`
}
