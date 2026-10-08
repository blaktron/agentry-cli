// Package compile is the capability compiler (RUNNER-PLAN §R4): it turns
// manifest + operator policy into the run plan — generated gateway file
// set, tools-server spec, harness definition, and container topology.
// This is the layer that makes the registry's declared contract mean
// something at run time, so its two properties are non-negotiable:
// deterministic output (golden tests pin plan bytes) and one testable
// control per contract line (the Enforcement map states each mechanism).
package compile

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/dirpublish"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/unsafeexec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// Inputs is everything a compile consumes. No run-scoped values belong
// here (§R4 determinism); the engine substitutes those at execute time.
type Inputs struct {
	// CompiledBy is the CLI build recorded in the plan (see
	// spec.Plan.CompiledBy); empty in tests and goldens.
	CompiledBy string
	// Manifest is the verified package manifest.
	Manifest *manifest.Manifest
	// Mode is the run mode to compile for; empty means autonomous. A mode
	// the manifest does not declare is refused (exit 12: not runnable
	// this way).
	Mode manifest.Mode
	// Digest is the verified archive digest (or local-tree digest).
	Digest string
	// Source is "registry", "local", or "builtin" for a package this binary
	// embeds (pkg/runner/localrun).
	Source string
	// WorkspaceDir is the extracted, verified archive (read-only here:
	// the compiler reads prompt files and the optional output schema).
	WorkspaceDir string
	// Policy is the validated operator policy; PolicySHA fingerprints
	// the exact bytes it was loaded from.
	Policy    *policy.Policy
	PolicySHA string
	// AdapterName selects the harness adapter ("" = default).
	AdapterName string
	// RunnerImage is the local runner image tag (§R7).
	RunnerImage string
	// MCPServers are the declared servers, already resolved against the
	// trusted registries (RUNNER-PLAN-3). Resolution happens in the
	// install step, not here: the compiler stays offline and
	// deterministic over its inputs.
	MCPServers []spec.MCPServerSpec
	// Web grants a built-in the web tools, web_search and web_read (plan
	// builder-rework.md §7.3, D6; agentry-cli#345; the Knowledge Creator's
	// since agentry-cli#523). Only a built-in package may be granted them
	// (Source "builtin", set by pkg/runner/localrun for the Drafter and the
	// Knowledge Creator alone), and only one that declares no egress, exec,
	// filesystem write, secret or MCP server: their container has no
	// network allowlist of its own, so nothing else may run beside them.
	// Anything else asking is refused by policy (exit 11).
	Web bool
	// RegistrySearch adds registry_search to the web tools: the Drafter's
	// alone (plan knowledge-and-drafter.md §7). It is compiled, never the
	// run's, so a run that hands the tools a registry for its scans (a
	// hosted build) does not get the tool with it. Only with Web.
	RegistrySearch bool
	// WebSearchedOnly holds web_read to the addresses a web_search of the
	// run returned (toolsrv.Web.SearchedOnly): the Knowledge Creator's,
	// whose model reads documents no one scanned. Only with Web.
	WebSearchedOnly bool
	// Review grants a built-in the review grant (plan
	// desktop-review-key-and-run-cards.md §6.2; agentry-cli#503): ai_review
	// and scanned reads of its inputs, with the person's own TypeSafe key
	// in the tools container alone, whose network reaches TypeSafe's API and
	// nothing else. As with Web, only a built-in package may be granted it,
	// and only one that declares no egress, exec, filesystem write, secret
	// or MCP server; never beside the web tools, whose container reaches
	// any public address. Anything else asking is refused by policy.
	Review bool
	// IntegrityKey is the runner home's plan-integrity key (IntegrityKey).
	// It is not part of the plan: when set, the compile also writes
	// plan.json.hmac, so a later run can reuse the plan only as compiled
	// (ReadInstalledPlan, agentry-cli#266). Nil writes no record, and a
	// reader then recompiles rather than trust the plan.
	IntegrityKey []byte
	// Unsafe turns on Unsafe Mode for this compile (agentry-notes
	// plans/unsafe-mode.md §4.3; agentry-cli#490): the declared exec
	// commands are placed through the unsafe base's extras list and
	// command index (unsafeexec.Load) instead of the policy's exec
	// catalogue, which is not consulted; a command neither places is
	// named, never refused (D8). Only a client passes it — the desktop,
	// for runs on this computer, when the person has turned Unsafe Mode
	// on — and no policy key or CLI flag sets it (D5). Nil compiles
	// exactly as before.
	Unsafe *unsafeexec.Data
}

// PolicyRefusal is a run refused by operator policy — the §R8 exit-11
// class, distinct from a malformed package (exit 12).
type PolicyRefusal struct{ Reason string }

func (e *PolicyRefusal) Error() string { return "refused by policy: " + e.Reason }

// OutputSchemaFile is the §R2 structured-output convention.
const OutputSchemaFile = "output.schema.json"

// Compile builds the plan and writes the generated file set under planDir:
//
//	planDir/plan.json
//	planDir/gateway/{catalog,registry,config,tools}.yaml + toolspec.json
//	planDir/harness/<adapter files>
//
// The returned plan's Files map hashes every generated file, so a tampered
// installed plan is detectable before execution (§R6). planDir is replaced
// whole: whatever it held before is gone once Compile returns.
func Compile(in Inputs, planDir string) (*spec.Plan, error) {
	m := in.Manifest
	if m == nil {
		return nil, fmt.Errorf("compile: nil manifest")
	}
	adapterName := in.AdapterName
	if adapterName == "" {
		adapterName = adapter.DefaultName
	}
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return nil, err
	}

	mode := in.Mode
	if mode == "" {
		mode = manifest.ModeAutonomous
	}
	if !mode.Valid() {
		return nil, fmt.Errorf("unknown run mode %q", mode)
	}
	if !m.Supports(mode) {
		return nil, fmt.Errorf("%s/%s@%s does not declare run mode %s (modes: %s) — not runnable this way",
			m.Namespace, m.Name, m.Version, mode, strings.Join(modeStrings(m.SupportedModes()), ", "))
	}

	caps, err := summarizeCapabilities(m, in.Policy)
	if err != nil {
		return nil, err
	}
	if (in.RegistrySearch || in.WebSearchedOnly) && !in.Web {
		return nil, &PolicyRefusal{Reason: toolsrv.ToolRegistrySearch + " and the searched-only web_read are of the web tools, and are never granted without them"}
	}
	if in.Web {
		if err := checkWebGrant(in, caps); err != nil {
			return nil, err
		}
		caps.WebTools, caps.RegistrySearch, caps.WebSearchedOnly = true, in.RegistrySearch, in.WebSearchedOnly
	}
	if in.Review {
		if err := checkReviewGrant(in, caps); err != nil {
			return nil, err
		}
		caps.ReviewTools = true
	}
	// The scratch folder is the runner's grant to every release's run and
	// session, never a line of the contract; the built-ins keep their own
	// fixed surfaces (plan scratch-and-pdf-reports.md D2, D5).
	caps.Scratch = in.Source != "builtin"
	instructionPath, body, err := deriveInstruction(m, in.WorkspaceDir)
	if err != nil {
		return nil, err
	}

	schemaJSON, output, err := loadOutputSchema(in.WorkspaceDir)
	if err != nil {
		return nil, err
	}

	tspec := toolSpec(caps, m.Capabilities.Secrets)
	extraServers, extraEntries, err := dataAccessServers(caps.DataAccess, in.Policy)
	if err != nil {
		return nil, err
	}

	// A package server's OAuth sign-in runs on the person's connection, as
	// a remote server's does (agentry-cli#665), except for a package known
	// not to run on an access token alone (plan D25): refused by name here
	// as well as at validation, so a release published before the package
	// was listed does not run without what it needs.
	for _, srv := range m.Capabilities.MCPServers {
		if srv.IsRemote() || srv.OAuth == nil {
			continue
		}
		if alt, bad := manifest.UnsupportedOAuthPackage(srv.SourceID()); bad {
			return nil, &PolicyRefusal{Reason: fmt.Sprintf(
				"declared MCP server %s cannot run on an access token alone, so it cannot take a connection: run a version of the QuickAgent that declares %s instead",
				serverRef(srv), alt)}
		}
	}

	// Declared MCP servers (RUNNER-PLAN-3): policy-gate their egress and
	// refuse name collisions before anything renders.
	for _, srv := range in.MCPServers {
		if err := checkRemote(srv); err != nil {
			return nil, err
		}
		if err := checkLiteralEnv(srv); err != nil {
			return nil, err
		}
		if _, _, err := oauthEnv(srv); err != nil {
			return nil, err
		}
		for _, h := range srv.Egress {
			if h == "*" {
				// A wildcard server would get no relay from the gateway
				// and, with no network of the run's own to inherit, land
				// on docker's default bridge — a route out that nothing of
				// ours constrains and a route to every other container
				// there. Policy can permit the agent's own "*" (the
				// llm-proxy still fronts it); for a declared server there
				// is no such front, so the answer is hosts or nothing
				// (agentry-cli#105, DR-20).
				return nil, &PolicyRefusal{Reason: fmt.Sprintf(
					`declared MCP server %s requests egress "*": the runner gives a declared server no unrestricted network, only a relay per declared host; declare the hosts it needs`, srv.Name)}
			}
		}
		if srv.Name == spec.ToolServerName {
			return nil, fmt.Errorf("declared MCP server resolves to the reserved name %q", srv.Name)
		}
		if _, dup := extraEntries[srv.Name]; dup {
			return nil, &PolicyRefusal{Reason: fmt.Sprintf(
				"declared MCP server %s collides with an operator dataAccess server of the same name", srv.Name)}
		}
	}

	// The System Prompt, then the package's instruction (agentry-cli#520).
	declared := mcpServerNames(extraServers, in.MCPServers)
	inputView := inputViewOf(m)
	instruction := renderInstruction(promptFacts{
		caps:      caps,
		secrets:   sortedCopy(m.Capabilities.Secrets),
		optional:  sortedCopy(m.Capabilities.OptionalSecrets),
		servers:   declared,
		schema:    len(schemaJSON) > 0,
		intake:    len(m.Intake) > 0,
		inputView: inputView,
		review:    in.Review,
	}, body)

	files := map[string][]byte{}
	// The tools server runs in the shared runner image unless the agent
	// declares exec, in which case it runs in that agent's exec image —
	// the runner image with the operator's catalogued packages installed,
	// minimized to the declared commands (agentry-cli#9, #100).
	toolsImage := in.RunnerImage
	var execImage *spec.ExecImageSpec
	if in.Unsafe != nil {
		execImage = unsafeExecImageFor(caps.ExecCommands, in.RunnerImage, in.Unsafe)
	} else if execImage, err = execImageFor(caps.ExecCommands, in.RunnerImage, in.Policy.Exec); err != nil {
		return nil, err
	}
	if execImage != nil {
		toolsImage = execImage.Tag
		// Unsafe Mode's image is not minimized, and a declared command
		// runs with its PATH (the operator's decision of 2026-09-30).
		tspec.Exec.ImagePath = execImage.Unsafe != nil
	}
	if err := renderGatewayFiles(files, tspec, caps, m.Capabilities.Secrets, toolsImage, in.RunnerImage, extraServers, extraEntries, in.MCPServers); err != nil {
		return nil, err
	}

	res, err := ad.Compile(adapter.Input{
		Mode:         mode,
		Instruction:  instruction,
		Summary:      m.Summary,
		Model:        in.Policy.Model,
		HarnessImage: in.Policy.Images.HarnessDockerAgent,
		GatewayURL:   spec.GatewayURL(),
		SchemaJSON:   schemaJSON,
		Harness:      in.Policy.Harness[adapterName],
	})
	if err != nil {
		var refusal *adapter.Refusal
		if errors.As(err, &refusal) {
			return nil, &PolicyRefusal{Reason: refusal.Reason}
		}
		return nil, fmt.Errorf("adapter %s: %w", adapterName, err)
	}
	if res.Session != nil {
		res.Session.Budgets = sessionBudgets(m, in.Policy)
	}
	for name, content := range res.Files {
		files[filepath.ToSlash(filepath.Join("harness", name))] = content
	}

	servers := append([]string{spec.ToolServerName}, declared...)
	sort.Strings(servers)

	plan := &spec.Plan{
		PlanVersion:  spec.PlanVersion,
		CompiledBy:   in.CompiledBy,
		Ref:          fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
		Digest:       in.Digest,
		Source:       in.Source,
		Modes:        modeStrings(m.SupportedModes()),
		Mode:         string(mode),
		Summary:      m.Summary,
		PolicySHA256: in.PolicySHA,
		Instruction: spec.InstructionSpec{
			SourcePath: instructionPath,
			SHA256:     sha256hex([]byte(instruction)),
			BodySHA256: sha256hex([]byte(body)),
		},
		Capabilities: caps,
		Tools:        tspec.GrantedTools(),
		MCPServers:   in.MCPServers,
		InputView:    inputView,
		Enforcement:  enforcement(caps, m.Capabilities.Secrets, m.Capabilities.OptionalSecrets, extraServers, in.Policy, in.MCPServers),
		ExecImage:    execImage,
		Gateway:      gatewaySpec(in.Policy.Images.Gateway, servers, caps, in.MCPServers),
		Harness: spec.HarnessSpec{
			Adapter:    adapterName,
			Model:      res.Model,
			Agent:      res.Agent,
			Host:       res.Host,
			ResultKind: res.ResultKind,
			Session:    res.Session,
		},
		Proxy: spec.ProxySpec{
			Image:     in.RunnerImage,
			Allow:     append([]string(nil), in.Policy.Model.ProxyAllow...),
			PlainHTTP: plainHTTPTargets(in.Policy),
		},
		Secrets:         sortedCopy(m.Capabilities.Secrets),
		OptionalSecrets: sortedCopy(m.Capabilities.OptionalSecrets),
		Budgets: spec.Budgets{
			TimeoutSeconds: in.Policy.Budgets.TimeoutSeconds,
			MaxTurns:       in.Policy.Budgets.MaxTurns,
			MaxCostUSD:     in.Policy.Budgets.MaxCostUSD,
		},
		Output: output,
		Files:  map[string]string{},
	}
	if execImage != nil && execImage.Unsafe != nil {
		plan.Enforcement["exec"] = unsafeExecEnforcement(execImage)
	}

	if rs := in.Policy.Docker.Resolvers; len(rs) > 0 && (in.Web || in.Policy.Docker.Runtime != "") {
		// The name servers are part of the plan too: a changed list
		// recompiles. A run with the web tools reads them, and under a
		// runtime so do the classes that resolve host names
		// (agentry-cli#337).
		plan.Docker = &spec.DockerSpec{Resolvers: slices.Clone(rs)}
	}
	if rt := in.Policy.Docker.Runtime; rt != "" {
		// The runtime is part of the plan and so of its hash: an operator
		// who changes it recompiles rather than reinterpreting a stored
		// plan (DR-23). Which container classes it holds is the run's to
		// say (the docker filter that gives the gateway's containers the
		// runtime runs on Linux only): the receipt's runtime line names
		// each class, and the ones on the daemon's default and why
		// (engine.runtimeLine, agentry-cli#221), so `runtime:` never
		// claims more ground than the engine holds.
		if plan.Docker == nil {
			plan.Docker = &spec.DockerSpec{}
		}
		plan.Docker.Runtime = rt
		plan.Docker.GatewayRuntime = in.Policy.Docker.GatewayRuntime
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		plan.Enforcement["runtime"] = rt + ": the run's containers are created with docker --runtime " + rt +
			" where it holds; the receipt's runtime line names each container class, and each that takes the daemon's default and why"
	}
	// The model key's source is a mechanism the receipt names (§R11): the
	// variable, or the variable else the keychain (agentry-cli#148).
	if keyEnv, account := modelKeySource(in.Policy, adapterName); keyEnv != "" {
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		if account == "" {
			plan.Enforcement["model.apiKey"] = "$" + keyEnv + " from the host environment, injected into the agent container (or the host harness process) only"
		} else {
			plan.Enforcement["model.apiKey"] = "$" + keyEnv + " from the host environment, else the OS keychain entry " + account +
				" (policy apiKeySource keyring); injected into the agent container (or the host harness process) only, never written to the plan or the receipt"
		}
		// A subscription token is a mechanism of its own (agentry-cli#171):
		// the plan was rendered for it, and the run refuses a value of
		// the other kind.
		if adapterName == adapter.ClaudeAdapterName && policy.KeyKind(in.Policy.Harness[adapterName], in.Policy.Model) == policy.KeyKindSubscription {
			plan.Enforcement["model.apiKey"] += "; an Anthropic subscription token (policy apiKeyKind subscription), passed to Claude Code as CLAUDE_CODE_OAUTH_TOKEN without --bare (DR-26); the run checks the value it resolves is one"
		}
	}
	// Write the generated files, hashing each into the plan.
	if res.Host != nil {
		// §R11 invariant 8: the harness's own isolation is a claim like any
		// other, and for a host process it is a weaker one.
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		plan.Enforcement["harness.isolation"] = "host process from this machine's own installation of " + res.Host.Command +
			" (no container): the proxy variables and the gateway catalog bound it, not the run network"
	}
	// Everything is written into a staging directory beside planDir and
	// published in one rename (dirpublish.Replace), so a plan directory is
	// only ever one compile's complete output: another process reading or
	// compiling the same plan at the same time never sees this compile's
	// plan.json beside another's record (agentry-cli#289).
	err = dirpublish.Replace(planDir, func(staging string) error {
		for rel, content := range files {
			abs := filepath.Join(staging, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return fmt.Errorf("create plan dir: %w", err)
			}
			if err := os.WriteFile(abs, content, 0o644); err != nil { //nolint:gosec // generated config, no secrets by construction
				return fmt.Errorf("write %s: %w", rel, err)
			}
			plan.Files[rel] = sha256hex(content)
		}
		planBytes, err := plan.MarshalCanonical()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staging, "plan.json"), planBytes, 0o644); err != nil { //nolint:gosec // ditto
			return fmt.Errorf("write plan.json: %w", err)
		}
		if in.IntegrityKey != nil {
			return writeIntegrityRecord(in.IntegrityKey, staging, planBytes)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// VerifyFiles re-hashes an installed plan's generated files, refusing a
// tampered plan dir before anything runs (§R6).
func VerifyFiles(p *spec.Plan, planDir string) error {
	for rel, want := range p.Files {
		b, err := os.ReadFile(filepath.Join(planDir, filepath.FromSlash(rel))) //nolint:gosec // paths from our own plan
		if err != nil {
			return fmt.Errorf("installed plan incomplete: %w", err)
		}
		if got := sha256hex(b); got != want {
			return fmt.Errorf("installed plan file %s does not match plan.json (got %s want %s) — delete the install dir and re-run", rel, got[:12], want[:12])
		}
	}
	return nil
}

// summarizeCapabilities normalizes the manifest unions and applies the
// policy gates that refuse a run outright (§R4 wildcard row).
func summarizeCapabilities(m *manifest.Manifest, pol *policy.Policy) (spec.CapSummary, error) {
	caps := spec.CapSummary{
		EgressMode:  "none",
		FSReadMode:  "none",
		FSWriteMode: "none",
	}
	eg := m.Capabilities.Network.Egress
	switch {
	case eg.IsZero() || eg.None:
		// undeclared = none: the registry's own reading (§5.4 empty
		// capabilities = pure text agent).
	case len(eg.Hosts) == 1 && eg.Hosts[0] == "*":
		if !pol.AllowWildcardEgress {
			return caps, &PolicyRefusal{Reason: `manifest declares network.egress: "*" and this machine's policy has allowWildcardEgress: false`}
		}
		caps.EgressMode = "all"
	default:
		hosts := sortedCopy(eg.Hosts)
		for _, h := range hosts {
			if h == "*" {
				// A list mixing "*" with hosts is a wildcard with extra
				// steps; hold it to the same gate.
				if !pol.AllowWildcardEgress {
					return caps, &PolicyRefusal{Reason: `manifest egress list contains "*" and policy has allowWildcardEgress: false`}
				}
				caps.EgressMode = "all"
			}
		}
		if caps.EgressMode != "all" {
			caps.EgressMode = "hosts"
			caps.EgressHosts = hosts
		}
	}

	caps.FSReadMode, caps.FSReadPaths = fsMode(m.Capabilities.Filesystem.Read)
	caps.FSWriteMode, caps.FSWritePaths = fsMode(m.Capabilities.Filesystem.Write)
	for _, p := range append(append([]string{}, caps.FSReadPaths...), caps.FSWritePaths...) {
		if err := checkWorkspacePath(p); err != nil {
			return caps, err
		}
	}
	if !m.Capabilities.Exec.IsZero() && !m.Capabilities.Exec.None {
		caps.ExecCommands = sortedCopy(m.Capabilities.Exec.Commands)
	}
	for _, d := range m.Capabilities.DataAccess {
		caps.DataAccess = append(caps.DataAccess, string(d))
	}
	sort.Strings(caps.DataAccess)
	return caps, nil
}

// checkWorkspacePath judges a filesystem.read/write path before it becomes
// a bind-mount specification. workspaceVolumes builds those by joining the
// workspace placeholder and the path with a slash, and docker reads the
// result as source:target[:mode] — so a path is a relative, clean, slash
// path with no ".." segment and no ":" in it, or the bind leaves the run
// workspace (or names a different one). pkg/manifest.Validate refuses the
// same shapes at publish and on the --local path, but the compiler is the
// last thing between the manifest and the mount, and a manifest that
// reached it unvalidated — a registry release, a cached install — is
// judged here regardless (security review 2026-09-20 §4.2).
func checkWorkspacePath(p string) error {
	switch {
	case p == "" || p == ".":
		return fmt.Errorf("compile: filesystem path %q names the workspace itself; declare read: workspace or write: workspace instead", p)
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\"):
		return fmt.Errorf("compile: filesystem path %q is absolute; paths are relative to the workspace", p)
	case strings.ContainsAny(p, ":\\"):
		return fmt.Errorf("compile: filesystem path %q contains a character a bind mount cannot carry", p)
	case path.Clean(p) != p:
		return fmt.Errorf("compile: filesystem path %q is not in its clean form (%q)", p, path.Clean(p))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("compile: filesystem path %q would leave the workspace", p)
		}
	}
	return nil
}

func fsMode(a manifest.FSAccess) (string, []string) {
	switch a.Mode {
	case manifest.FSModeWorkspace:
		return "workspace", nil
	case manifest.FSModePaths:
		return "paths", sortedCopy(a.Paths)
	default: // none or undeclared
		return "none", nil
	}
}

// mcpServerNames is every MCP server a run has besides the runner's own
// tools server: the operator's dataAccess servers and the QuickAgent's
// declared ones, sorted.
func mcpServerNames(extra []string, declared []spec.MCPServerSpec) []string {
	names := append([]string(nil), extra...)
	for _, srv := range declared {
		names = append(names, srv.Name)
	}
	sort.Strings(names)
	return names
}

// sessionBudgets applies plan §6.7: the operator's caps, with the idle
// timeout tightened by the manifest's interactive hint (never loosened).
func sessionBudgets(m *manifest.Manifest, pol *policy.Policy) spec.SessionBudgets {
	b := spec.SessionBudgets{
		IdleTimeoutSeconds: pol.Sessions.IdleTimeoutSeconds,
		MaxWallSeconds:     pol.Sessions.MaxWallSeconds,
		MaxUserTurns:       pol.Sessions.MaxUserTurns,
	}
	if m.Interactive != nil && m.Interactive.IdleTimeoutSeconds > 0 &&
		(b.IdleTimeoutSeconds == 0 || m.Interactive.IdleTimeoutSeconds < b.IdleTimeoutSeconds) {
		b.IdleTimeoutSeconds = m.Interactive.IdleTimeoutSeconds
	}
	return b
}

// modeStrings renders a mode set for the plan.
func modeStrings(modes []manifest.Mode) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}

// deriveInstruction implements the §R2 closed rule for where the harness
// instruction comes from: the manifest's own candidate list — a schema v2
// entrypoints.instruction (AGENT.md when omitted); for v1 the skill
// entrypoint, or AGENT.md then PROMPT.md for a prompt-agent — read in order.
// A manifest with no candidates (the withdrawn mcp-server kind) is not
// runnable (DR-15).
func deriveInstruction(m *manifest.Manifest, workspace string) (string, string, error) {
	candidates := m.InstructionCandidates()
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("%s/%s declares no instruction file, so it is not a runnable QuickAgent (RUNNER-PLAN §R2; the withdrawn mcp-server kind is a tool server)", m.Namespace, m.Name)
	}
	for _, rel := range candidates {
		body, err := readInstructionFile(workspace, rel)
		if err == nil {
			return rel, body, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("instruction file missing from the package: looked for %s (RUNNER-PLAN §R2)", strings.Join(candidates, ", "))
}

// ReadInstruction reads the instruction a plan was compiled from out of a
// workspace and returns its prose as the compile read it: frontmatter
// stripped, without the System Prompt. The text is held to the body's
// digest the plan records, so what it returns is the instruction the plan
// was built on, whichever client ran it and whether or not that client
// checked the workspace. The engine sends it as the task of a run given
// none (agentry-cli#449). A plan with no body digest was compiled before
// the System Prompt (agentry-cli#520) and is refused: it is recompiled,
// never reinterpreted.
func ReadInstruction(workspace string, in spec.InstructionSpec) (string, error) {
	body, err := readInstructionFile(workspace, in.SourcePath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the instruction file %s is missing from the package", in.SourcePath)
	}
	if err != nil {
		return "", err
	}
	if in.BodySHA256 == "" {
		return "", fmt.Errorf("the plan records no digest of %s's own text: it was compiled by an older CLI and must be compiled again", in.SourcePath)
	}
	if sha256hex([]byte(body)) != in.BodySHA256 {
		return "", fmt.Errorf("the instruction file %s changed after the plan was compiled", in.SourcePath)
	}
	return body, nil
}

// readInstructionFile reads one archive-relative instruction file, its
// frontmatter stripped. A path that leaves the package is refused.
func readInstructionFile(workspace, rel string) (string, error) {
	p := filepath.FromSlash(rel)
	if rel == "" || !filepath.IsLocal(p) {
		return "", fmt.Errorf("instruction path %q is not a file inside the package", rel)
	}
	b, err := os.ReadFile(filepath.Join(workspace, p)) //nolint:gosec // local path from the verified archive
	if err != nil {
		return "", fmt.Errorf("read instruction %s: %w", rel, err)
	}
	return stripFrontmatter(string(b)), nil
}

// stripFrontmatter drops a leading YAML frontmatter block: the harness
// wants the prose, and skill frontmatter is install-target metadata.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return strings.TrimSpace(s) + "\n"
	}
	rest := s[strings.Index(s, "\n")+1:]
	for _, marker := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, marker); i >= 0 {
			return strings.TrimSpace(rest[i+len(marker):]) + "\n"
		}
	}
	// Unterminated frontmatter: treat the whole file as body rather than
	// silently emptying the instruction.
	return strings.TrimSpace(s) + "\n"
}

// loadOutputSchema reads the optional §R2 output convention.
func loadOutputSchema(workspace string) ([]byte, spec.OutputSpec, error) {
	b, err := os.ReadFile(filepath.Join(workspace, OutputSchemaFile)) //nolint:gosec // fixed name under the verified archive
	if err != nil {
		if os.IsNotExist(err) {
			return nil, spec.OutputSpec{}, nil
		}
		return nil, spec.OutputSpec{}, fmt.Errorf("read %s: %w", OutputSchemaFile, err)
	}
	var probe any
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, spec.OutputSpec{}, fmt.Errorf("%s is not valid JSON: %w", OutputSchemaFile, err)
	}
	// The schema must itself be a valid JSON Schema: the engine validates
	// the final message against it (DR-8 closed, agentry-cli#11), so a
	// broken schema is a compile-time refusal, not a run-time surprise.
	if _, err := CompileOutputSchema(b); err != nil {
		return nil, spec.OutputSpec{}, fmt.Errorf("%s is not a valid JSON Schema: %w", OutputSchemaFile, err)
	}
	return b, spec.OutputSpec{SchemaFile: OutputSchemaFile, SchemaSHA256: sha256hex(b)}, nil
}

// CompileOutputSchema compiles output.schema.json bytes (draft 2020-12 by
// default, whatever $schema names otherwise) into a validator the engine
// runs the final message through.
func CompileOutputSchema(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(OutputSchemaFile, doc); err != nil {
		return nil, err
	}
	return c.Compile(OutputSchemaFile)
}

// checkWebGrant is the web tools' gate: a built-in package that declares
// nothing their unrestricted container could widen (plan builder-rework.md
// §7.3: "refused at compile for any release").
func checkWebGrant(in Inputs, caps spec.CapSummary) error {
	if in.Source != "builtin" {
		return &PolicyRefusal{Reason: fmt.Sprintf("the web tools (%s, %s, %s) are the built-ins' alone, the Drafter's and the Knowledge Creator's; a %s package is never granted them",
			toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolRegistrySearch, cmp.Or(in.Source, "release"))}
	}
	c := in.Manifest.Capabilities
	if caps.EgressMode != "none" || len(caps.ExecCommands) > 0 || caps.FSWriteMode != "none" || len(c.Secrets) > 0 || len(c.MCPServers) > 0 || len(in.MCPServers) > 0 {
		return &PolicyRefusal{Reason: "the web tools are granted only to a package that declares no egress, exec, filesystem write, secret or MCP server: their container has no network allowlist of its own"}
	}
	// Under a sandboxed runtime the tools server's own network stack does
	// not reach docker's embedded DNS on the run's web network (DR-23,
	// agentry-cli#386): with no resolvers of the host's it would resolve
	// nothing and the draft would read nothing, so it does not run.
	if rt := in.Policy.Docker.Runtime; rt != "" && len(in.Policy.Docker.Resolvers) == 0 {
		return &PolicyRefusal{Reason: fmt.Sprintf("the web tools under the %s runtime resolve host names only through the host's own name servers: name them in the runner policy's docker.resolvers (docker's embedded DNS is not reachable from %s)", rt, rt)}
	}
	return nil
}

// checkReviewGrant is the review grant's gate, as checkWebGrant is the web
// tools': a built-in package that declares nothing its container could
// widen, and never the web tools beside it, since their container's network
// is the whole public web and the review's is TypeSafe's API alone.
func checkReviewGrant(in Inputs, caps spec.CapSummary) error {
	if in.Source != "builtin" {
		return &PolicyRefusal{Reason: fmt.Sprintf("the review grant (%s) is a built-in's alone; a %s package is never granted it",
			toolsrv.ToolAIReview, cmp.Or(in.Source, "release"))}
	}
	if in.Web {
		return &PolicyRefusal{Reason: "the review grant is never given beside the web tools: its container reaches TypeSafe's API alone, theirs the public web"}
	}
	c := in.Manifest.Capabilities
	if caps.EgressMode != "none" || len(caps.ExecCommands) > 0 || caps.FSWriteMode != "none" || len(c.Secrets) > 0 || len(c.MCPServers) > 0 || len(in.MCPServers) > 0 {
		return &PolicyRefusal{Reason: "the review grant is given only to a package that declares no egress, exec, filesystem write, secret or MCP server: its container holds the person's key and reaches TypeSafe's API, and nothing else may run beside it"}
	}
	return nil
}

// toolSpec compiles the capability summary into the tools-server spec —
// the single choke point deciding which tools exist (§R3, §R11 inv. 4).
func toolSpec(caps spec.CapSummary, secrets []string) *toolsrv.Spec {
	return &toolsrv.Spec{
		SpecVersion:  toolsrv.SpecVersion,
		WorkspaceDir: spec.WorkspaceMount,
		OutDir:       spec.OutMount,
		// The inputs channel is always configured (RUNNER-PLAN-2 §P4):
		// whether a given run provides files is a run-time fact, and the
		// engine mounts an empty directory when none were.
		InputsDir: spec.InputsMount,
		// The decoded inputs beside them (plan file-handling.md §5): the
		// engine decodes a run's inputs before the agent starts, and
		// mounts an empty directory when there were none.
		InputsTextDir: spec.InputsTextMount,
		ScratchDir:    scratchDir(caps),
		// export_pdf goes with the scratch folder, to every release's run
		// and session (plan scratch-and-pdf-reports.md D5, D6).
		PDF: toolsrv.PDF{Enabled: caps.Scratch},
		FSRead: toolsrv.Access{
			Enabled: caps.FSReadMode != "none",
			Paths:   caps.FSReadPaths,
		},
		FSWrite: toolsrv.Access{
			Enabled: caps.FSWriteMode != "none",
			Paths:   caps.FSWritePaths,
		},
		Fetch: toolsrv.Fetch{
			Enabled:   caps.Fetches(),
			Hosts:     caps.EgressHosts,
			AllowAll:  caps.EgressMode == "all",
			SecretEnv: sortedCopy(secrets),
		},
		Exec: toolsrv.Exec{Commands: caps.ExecCommands},
		// The run's half of Web (where it scans, searches, which registry,
		// the ledger) is the engine's, stamped per run.
		Web: toolsrv.Web{Enabled: caps.WebTools, RegistrySearch: caps.RegistrySearch, SearchedOnly: caps.WebSearchedOnly},
		// The review grant: the run's ledger is the engine's, stamped per
		// run.
		Review: toolsrv.Review{Enabled: caps.ReviewTools},
	}
}

// scratchDir is the tools spec's scratch folder: the mount when the plan
// grants it, empty otherwise.
func scratchDir(caps spec.CapSummary) string {
	if caps.Scratch {
		return spec.ScratchMount
	}
	return ""
}

// GatewayConfigDir is where the rendered gateway files are mounted inside
// the gateway container. It must be the gateway's own catalogs directory:
// since v0.43 a local --catalog path "must resolve within Docker MCP
// catalogs directory" (pkg/catalog, errUntrustedLocalPath) and the
// gateway exits at startup otherwise (agentry-cli#41). HOME is pinned to
// /root in the container env so the directory is the same on every host.
const GatewayConfigDir = "/root/.docker/mcp/catalogs"

// GatewayGuardFlags are the guards every gateway a run starts is given,
// after its transport and files: --block-network, without which allowHosts
// is advisory (§R11 invariant 1), then the guards the gateway defaults on,
// passed explicitly so a run never inherits one it depends on from a
// version's default. --block-secrets is default true in the pinned gateway
// and passed anyway: the refusal of a secret-looking tool result is in the
// enforcement table the receipts answer to (agentry#297), and a version
// whose default flipped would drop the guard while the receipt still
// claimed it. The MCP server survey starts its gateway with them too
// (scripts/mcp-server-survey, agentry-cli#658).
func GatewayGuardFlags() []string {
	return []string{"--block-network", "--verify-signatures", "--log-calls", "--block-secrets"}
}

// gatewaySpec builds the gateway container launch (§R4). The flag set is
// compiled, and --block-network is unconditional: without it allowHosts is
// advisory, which is the sharpest foot-gun the research flagged (§R11
// invariant 1 pins this in a test). The guards that the gateway itself
// defaults on are passed explicitly too, so the run never inherits one it
// depends on from a version's default.
func gatewaySpec(image string, servers []string, caps spec.CapSummary, mcpServers []spec.MCPServerSpec) spec.GatewaySpec {
	withStore := spec.HasPackageServers(mcpServers)
	writable := []string{spec.PHOut}
	if caps.FSWriteMode != "none" {
		writable = append(writable, spec.PHWorkspace)
	}
	// A package server's token folder (agentry-cli#665, DR-77), each one
	// by its exact path: never the run's cfg or its connections folder,
	// which hold the gateway's own files and the other servers' folders.
	for _, srv := range mcpServers {
		if srv.OAuth != nil && len(srv.OAuth.Files) > 0 {
			writable = append(writable, ConnectionFolder(srv.Name))
		}
	}
	return spec.GatewaySpec{
		Image: image,
		Args: append([]string{
			"--transport", "streaming",
			"--port", fmt.Sprintf("%d", spec.GatewayPort),
			"--servers", strings.Join(servers, ","),
			"--catalog", GatewayConfigDir + "/catalog.yaml",
			"--registry", GatewayConfigDir + "/registry.yaml",
			"--config", GatewayConfigDir + "/config.yaml",
			"--tools-config", GatewayConfigDir + "/tools.yaml",
			"--secrets", GatewayConfigDir + "/secrets.env",
		}, GatewayGuardFlags()...),
		Env: []spec.EnvVar{
			{Name: spec.GatewayAuthTokenEnv, FromRunToken: true},
			// Pins the catalogs directory (see GatewayConfigDir); the
			// image sets no HOME and runs as root.
			{Name: "HOME", Value: "/root"},
			// Bind-mount containment (gateway v0.43.1+): sources must
			// resolve under these roots; writable binds need exact
			// entries. Separator ":" — verified empirically at M-R2. On a
			// Windows host the engine spells these roots /c/… so no drive
			// colon splits them (gatewayHostPath, agentry-cli#414).
			{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS",
				Value: strings.Join(bindRoots(withStore), ":")},
			{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS",
				Value: strings.Join(writable, ":")},
		},
		Mounts: []spec.Mount{
			{Source: spec.PHCfg + "/gateway", Target: GatewayConfigDir, ReadOnly: true},
			// The gateway drives the host daemon: that is its documented
			// privilege level, and the reason the agent container never
			// sees this socket (§R4).
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		},
		Servers: servers,
	}
}

// renderGatewayFiles writes the five-file gateway config set (§R4) into
// the files map.
func renderGatewayFiles(files map[string][]byte, tspec *toolsrv.Spec, caps spec.CapSummary,
	secrets []string, toolsImage, runnerImage string, extraServers []string, extraEntries map[string]yaml.Node,
	mcpServers []spec.MCPServerSpec,
) error {
	tspecJSON, err := json.MarshalIndent(tspec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal toolspec: %w", err)
	}
	files["gateway/toolspec.json"] = append(tspecJSON, '\n')

	entry := catalogEntry(tspec, caps, secrets, toolsImage)
	catalog := yamlMap{}
	catalog.set(spec.ToolServerName, entry)
	for _, name := range extraServers {
		node := extraEntries[name]
		catalog.set(name, &node)
	}
	for _, srv := range mcpServers {
		if srv.Remote() {
			catalog.set(srv.Name, remoteServerEntry(srv, runnerImage))
			continue
		}
		catalog.set(srv.Name, mcpServerEntry(srv))
	}
	catalogDoc := yamlMap{}
	catalogDoc.set("registry", catalog.node())
	b, err := marshalYAML("# generated by agentry run — gateway server catalog (RUNNER-PLAN §R4)\n", catalogDoc.node())
	if err != nil {
		return err
	}
	files["gateway/catalog.yaml"] = b

	enabled := yamlMap{}
	names := append([]string{spec.ToolServerName}, extraServers...)
	for _, srv := range mcpServers {
		names = append(names, srv.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := yamlMap{}
		e.set("ref", strNode(""))
		enabled.set(name, e.node())
	}
	registryDoc := yamlMap{}
	registryDoc.set("registry", enabled.node())
	if b, err = marshalYAML("# generated by agentry run — enabled servers\n", registryDoc.node()); err != nil {
		return err
	}
	files["gateway/registry.yaml"] = b

	files["gateway/config.yaml"] = []byte("# generated by agentry run — per-server config values (none)\n{}\n")

	// Per-server tool allowlist: belt to the tools server's own braces —
	// the server only registers granted tools, and the gateway only
	// routes these names.
	tools := yamlMap{}
	tools.set(spec.ToolServerName, strListNode(tspec.GrantedTools()))
	// A declared server with a manifest tool filter gets an allowlist
	// entry; one without a filter is granted its full set (no entry).
	for _, srv := range mcpServers {
		if len(srv.Tools) > 0 {
			tools.set(srv.Name, strListNode(sortedCopy(srv.Tools)))
		}
	}
	if b, err = marshalYAML("# generated by agentry run — per-server tool allowlist\n", tools.node()); err != nil {
		return err
	}
	files["gateway/tools.yaml"] = b
	return nil
}

// checkLiteralEnv refuses a declared env value the gateway would rewrite
// or drop. docker/mcp-gateway (pinned v0.43.3, pkg/gateway/clientpool.go)
// expands `$NAME`/`${NAME}` in a catalog env value against the server
// container's environment — the server's secrets included — evaluates a
// value holding `{{…}}` as a template, and skips a value that comes out
// empty, so such a value would not reach the server as the manifest wrote
// it, could copy a secret into a plain variable, or would be named in the
// receipt without ever being set. A declared value is literal and
// non-empty, or it is not declared.
//
// The `$` and `{{ }}` half is the registry's own rule,
// manifest.LiteralEnvValue (agentry#291), called rather than copied so
// the registry and the runner cannot drift (agentry-cli#131); the empty
// value and the `@@` run-placeholder marker are the runner's alone.
func checkLiteralEnv(srv spec.MCPServerSpec) error {
	for _, name := range slices.Sorted(maps.Keys(srv.Env)) {
		v := srv.Env[name]
		if v == "" {
			return fmt.Errorf("declared MCP server %s: env %s: the value is empty and the gateway would not set it; declare a value or leave the key out", srv.Ref(), name)
		}
		if !manifest.LiteralEnvValue(v) {
			return fmt.Errorf("declared MCP server %s: env %s: the value contains $ or {{ }}, which the gateway would interpret; declare a literal value", srv.Ref(), name)
		}
		if strings.Contains(v, "@@") {
			// The engine rewrites @@RUN_*@@ placeholders in plan files into
			// host paths; a manifest value must not be able to spell one.
			return fmt.Errorf("declared MCP server %s: env %s: the value contains @@, the run placeholder marker; declare a literal value", srv.Ref(), name)
		}
	}
	return nil
}

// execImageFor maps the declared commands onto the operator's exec
// catalog (agentry-cli#100, plan examples-declared-servers §5.2, D1). A
// command the catalog does not carry is a plan-time refusal — the
// operator, not the manifest, says which binaries this machine provides —
// never a run-time "absent". Nil when nothing is declared.
func execImageFor(commands []string, runnerImage string, cat policy.ExecPolicy) (*spec.ExecImageSpec, error) {
	if len(commands) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var pkgs, missing []string
	for _, c := range sortedCopy(commands) {
		pkg := cat.Package(c)
		if pkg == "" {
			missing = append(missing, c)
			continue
		}
		if !seen[pkg] {
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	if len(missing) > 0 {
		return nil, receipt.WithReason(&PolicyRefusal{Reason: fmt.Sprintf(
			"manifest declares exec command(s) %s that this machine's policy does not catalogue: add each to exec.packages (command: apk package, or busybox for an applet the runner image already has); every machine provides %s",
			strings.Join(missing, ", "), strings.Join(policy.BaseExecCommands(), ", "))},
			receipt.ReasonExecNotProvided, "", missing...)
	}
	sort.Strings(pkgs)
	return &spec.ExecImageSpec{
		Tag:       spec.ExecImageTag(runnerImage, commands, pkgs, cat.APKMirror),
		Commands:  sortedCopy(commands),
		Packages:  pkgs,
		APKMirror: cat.APKMirror,
	}, nil
}

// unsafeExecImageFor is execImageFor under Unsafe Mode: the declared
// commands placed through the unsafe base's extras list, then its index,
// never refused (D8); this machine's exec catalogue is not consulted (D1).
// Nil when nothing is declared.
func unsafeExecImageFor(commands []string, runnerImage string, data *unsafeexec.Data) *spec.ExecImageSpec {
	if len(commands) == 0 {
		return nil
	}
	pkgs, u := data.Place(commands)
	return &spec.ExecImageSpec{
		Tag:      spec.UnsafeExecImageTag(runnerImage, u, commands, pkgs),
		Commands: sortedCopy(commands),
		Packages: pkgs,
		Unsafe:   &u,
	}
}

// bindRoots lists the host roots the gateway may bind into the containers
// it launches: the run's own directories, plus the signature's server
// store when declared servers exist — each server is launched from a
// read-only bind of that store, and the gateway refuses a source outside
// its roots (agentry-cli#105). Left out otherwise so a plan without
// servers keeps its pre-PLAN-3 bytes.
func bindRoots(withServers bool) []string {
	roots := []string{spec.PHWorkspace, spec.PHOut, spec.PHCfg, spec.PHInputs, spec.PHInputsText, spec.PHKnowledge}
	if withServers {
		roots = append(roots, spec.PHServers)
	}
	return roots
}

// mcpServerEntry renders one declared MCP server (RUNNER-PLAN-3) as a
// gateway catalog entry: launched from the signature's read-only server
// store inside the pinned runtime image, with its own egress allowlist,
// only its own secrets, and its plain env as the gateway's env list
// (name/value pairs; catalog.Server.Env in docker/mcp-gateway).
func mcpServerEntry(srv spec.MCPServerSpec) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode(fmt.Sprintf("declared MCP server %s@%s (%s), installed per-signature", srv.Package, srv.Version, srv.Source)))
	e.set("title", strNode(srv.Package))
	e.set("type", strNode("server"))
	e.set("image", strNode(srv.Image))
	e.set("command", strListNode(srv.Command))
	e.set("longLived", boolNode(true))
	volumes := []string{spec.PHServers + "/" + srv.StoreSubdir + ":" + spec.MCPStorePrefix + "/" + srv.StoreSubdir + ":ro"}
	if srv.OAuth != nil && len(srv.OAuth.Files) > 0 {
		// The token folder of a package server's OAuth sign-in
		// (agentry-cli#665): a folder, never a single-file bind, so the
		// files the runner renews mid-run are seen. Writable, unlike a
		// bridge's: workspace-mcp, the measured Gmail server, refuses to
		// start unless it can write a probe file there (the operator's
		// decision of 2026-10-06, DR-77). It holds the access token alone,
		// is the run's own and is deleted at teardown; the runner still
		// renames each renewed file into place.
		volumes = append(volumes, ConnectionFolder(srv.Name)+":"+spec.ConnectionMount)
	}
	e.set("volumes", strListNode(volumes))
	switch {
	case len(srv.Egress) > 0:
		// "host:443/tcp": the gateway fronts each host with a TCP relay
		// (its L4 proxy) reached by that name — TLS runs end to end and
		// any HTTP client works. Its L7 form is an HTTP CONNECT proxy
		// handed over as a scheme-less proxy variable, which Node's
		// fetch cannot use and other clients ignore (agentry-cli#105,
		// DR-20).
		hosts := make([]string, 0, len(srv.Egress))
		for _, h := range sortedCopy(srv.Egress) {
			hosts = append(hosts, h+":443/tcp")
		}
		e.set("allowHosts", strListNode(hosts))
	default:
		e.set("disableNetwork", boolNode(true))
	}
	// A package server's OAuth sign-in (agentry-cli#665): an env entry that
	// carries the access token is a secret of the run, which the engine
	// renders into the gateway's secrets file under this server's key, and
	// one naming only the token folder is plain env. Checked by oauthEnv
	// before any entry renders.
	plainOAuth, tokenEnv, _ := oauthEnv(srv)
	if secretNames := append(sortedCopy(srv.Secrets), tokenEnv...); len(secretNames) > 0 {
		var entries []*yaml.Node
		for _, name := range secretNames {
			s := yamlMap{}
			s.set("name", strNode(ServerSecretKey(srv.Name, name)))
			s.set("env", strNode(name))
			entries = append(entries, s.node())
		}
		e.set("secrets", seqNode(entries))
	}
	if env := mergeEnv(srv.Env, plainOAuth); len(env) > 0 {
		var entries []*yaml.Node
		for _, name := range slices.Sorted(maps.Keys(env)) {
			kv := yamlMap{}
			kv.set("name", strNode(name))
			kv.set("value", strNode(env[name]))
			entries = append(entries, kv.node())
		}
		e.set("env", seqNode(entries))
	}
	return e.node()
}

// oauthEnv splits a package server's oauth env (agentry-cli#665): the
// entries that name only the token folder, rendered here with {dir} as the
// folder's mount, and the names of those that carry the access token, which
// the engine renders per run into the secrets file. A value the gateway
// would interpret, one that spells the run placeholder marker, or one with
// a placeholder outside the grammar is refused, as checkLiteralEnv refuses
// the plain env.
func oauthEnv(srv spec.MCPServerSpec) (plain map[string]string, tokenEnv []string, err error) {
	if srv.OAuth == nil {
		return nil, nil, nil
	}
	for _, name := range slices.Sorted(maps.Keys(srv.OAuth.Env)) {
		v := srv.OAuth.Env[name]
		if !manifest.LiteralEnvValue(v) || strings.Contains(v, "@@") {
			return nil, nil, fmt.Errorf("declared MCP server %s: oauth env %s: the value contains $, {{ }} or @@, which the gateway or the runner would interpret", srv.Ref(), name)
		}
		if _, exists := srv.Env[name]; exists || slices.Contains(srv.Secrets, name) {
			return nil, nil, fmt.Errorf("declared MCP server %s: oauth env %s is also the server's env or one of its secrets", srv.Ref(), name)
		}
		if srv.OAuth.TokenEnv(name) {
			// Checked here, rendered by the engine with the run's token.
			if _, err := manifest.ExpandOAuthEnvValue(v, map[string]string{"access_token": "x", "dir": spec.ConnectionMount}); err != nil {
				return nil, nil, fmt.Errorf("declared MCP server %s: oauth env %s: %w", srv.Ref(), name, err)
			}
			tokenEnv = append(tokenEnv, name)
			continue
		}
		r, err := manifest.ExpandOAuthEnvValue(v, map[string]string{"dir": spec.ConnectionMount})
		if err != nil {
			return nil, nil, fmt.Errorf("declared MCP server %s: oauth env %s: %w", srv.Ref(), name, err)
		}
		if plain == nil {
			plain = map[string]string{}
		}
		plain[name] = r
	}
	return plain, tokenEnv, nil
}

// mergeEnv is the plain env and the rendered oauth env as one map; nil
// when both are empty.
func mergeEnv(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

// remoteServerEntry renders one declared remote server (plan
// mcp-oauth-connections.md §5, D3; agentry-cli#663) as an ordinary gateway
// catalog entry: the runner image's bridge, forwarding stdio to the
// server's address, with the address's host as its one allowed host, so it
// runs on the relay network behind that host's relay, under the run's
// docker filter and the policy's runtime, as a package server does. The
// gateway dials nothing itself and is unchanged. An auth: secret server's
// bearer, and an auth: oauth server's access token, are not the gateway's
// secrets: the engine writes either into the server's connection folder,
// mounted read-only, which the bridge reads at every request.
func remoteServerEntry(srv spec.MCPServerSpec, runnerImage string) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode(fmt.Sprintf("declared remote MCP server %s (auth %s), reached through the runner image's bridge", srv.Package, srv.Auth)))
	e.set("title", strNode(srv.Package))
	e.set("type", strNode("server"))
	e.set("image", strNode(runnerImage))
	command := []string{"bridge", "--url", srv.Package}
	if srv.SendsBearer() {
		command = append(command, "--token-file", spec.ConnectionMount+"/"+spec.BearerFile)
		if srv.SignsIn() {
			command = append(command, "--oauth")
		}
		e.set("volumes", strListNode([]string{ConnectionFolder(srv.Name) + ":" + spec.ConnectionMount + ":ro"}))
	}
	e.set("command", strListNode(command))
	e.set("longLived", boolNode(true))
	e.set("allowHosts", strListNode([]string{srv.RemoteHost() + ":443/tcp"}))
	return e.node()
}

// ConnectionFolder is a remote server's connection folder, in plan
// placeholders: under the run's cfg directory, never under cfg/gateway,
// which is the gateway's own mount.
func ConnectionFolder(serverName string) string {
	return spec.PHCfg + "/" + spec.ConnectionsDir + "/" + serverName
}

// checkRemote refuses a remote server spec the compiler cannot render as
// the manifest's rules promise: an https address on a host, auth none with
// no secret, or auth secret with exactly one.
func checkRemote(srv spec.MCPServerSpec) error {
	if !srv.Remote() {
		return nil
	}
	if !strings.HasPrefix(srv.Package, "https://") || srv.RemoteHost() == "" || strings.ContainsAny(srv.Package, "${} \t\n\"'") {
		return fmt.Errorf("declared remote MCP server %q: not an https address a run can reach", srv.Package)
	}
	if u, err := url.Parse(srv.Package); err != nil || (u.Port() != "" && u.Port() != "443") {
		// The bridge's one allowed host is on 443 (the manifest's rule too).
		return fmt.Errorf("declared remote MCP server %s: a run reaches a remote server on port 443 only", srv.Package)
	}
	switch srv.Auth {
	case string(manifest.AuthNone):
		if len(srv.Secrets) != 0 {
			return fmt.Errorf("declared remote MCP server %s: auth none, yet it names a secret", srv.Package)
		}
	case string(manifest.AuthSecret):
		if len(srv.Secrets) != 1 {
			return fmt.Errorf("declared remote MCP server %s: auth secret needs exactly one declared secret, its bearer", srv.Package)
		}
	case string(manifest.AuthOAuth):
		if len(srv.Secrets) != 0 {
			return fmt.Errorf("declared remote MCP server %s: auth oauth, yet it names a secret", srv.Package)
		}
	default:
		return fmt.Errorf("declared remote MCP server %s: auth %q is not one this runner can send", srv.Package, srv.Auth)
	}
	return nil
}

// serverRef names a manifest's declared server: its pin, or a remote
// server's address.
func serverRef(srv manifest.MCPServer) string {
	if srv.IsRemote() {
		return srv.URL
	}
	return srv.SourceID() + "@" + srv.Version
}

// ServerSecretKey is the gateway secrets-file key for a declared server's
// env name — server-scoped, so a value reaches its own server and nothing
// else. Shared with the engine's secrets.env writer.
func ServerSecretKey(serverName, envName string) string {
	return serverName + "." + strings.ToLower(envName)
}

// catalogEntry builds the agentry-tools server entry — the manifest
// contract translated into the gateway's server-entry vocabulary.
func catalogEntry(tspec *toolsrv.Spec, caps spec.CapSummary, secrets []string, runnerImage string) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode("Agentry QuickAgent tools server (generated per run)"))
	e.set("title", strNode("QuickAgent Tools"))
	e.set("type", strNode("server"))
	e.set("image", strNode(runnerImage))
	e.set("command", strListNode(spec.ToolsCmd()))
	e.set("longLived", boolNode(true))

	vols := []string{spec.PHCfg + "/gateway/toolspec.json:" + spec.ToolSpecMount + ":ro"}
	vols = append(vols, workspaceVolumes(caps)...)
	// The run-inputs channel (P2 §P4): read-only by mount AND by tool
	// surface; the output/receipts volume stays separate below.
	vols = append(vols, spec.PHInputs+":"+spec.InputsMount+":ro")
	// The decoded inputs (plan file-handling.md §5), read-only the same
	// way: the decode step's index and texts, written by the host.
	vols = append(vols, spec.PHInputsText+":"+spec.InputsTextMount+":ro")
	// The knowledge channel (plan knowledge.md §4.5): one read-only mount
	// whether or not packs are attached, so an attachment never moves the
	// plan; the engine grants the tools only when it attaches one (DR-38).
	vols = append(vols, spec.PHKnowledge+":"+spec.KnowledgeMount+":ro")
	vols = append(vols, spec.PHOut+":"+spec.OutMount+":rw")
	e.set("volumes", strListNode(vols))

	if tspec.Fetch.Enabled && !tspec.Fetch.AllowAll {
		// Manifest hosts are DNS names; the gateway wants host:port.
		// 443 only: declared-host plaintext HTTP failing closed is the
		// right default for a security tool plane. This stays the
		// gateway's L7 form (an HTTP CONNECT proxy): the tools server is
		// our Go binary, whose http client reads the proxy variable the
		// gateway sets and re-checks every hop itself. A declared
		// server's hosts are relays instead (mcpServerEntry).
		hosts := make([]string, 0, len(caps.EgressHosts))
		for _, h := range caps.EgressHosts {
			hosts = append(hosts, h+":443")
		}
		e.set("allowHosts", strListNode(hosts))
	} else if tspec.Review.Enabled {
		// The review grant reaches TypeSafe's API and nothing else, by the
		// same gateway proxy a declared host is reached by: the judge's
		// client reads the proxy variable the gateway sets.
		e.set("allowHosts", strListNode([]string{toolsrv.ReviewHost + ":443"}))
	} else if !tspec.Fetch.Enabled && !tspec.Web.Enabled {
		e.set("disableNetwork", boolNode(true))
	}
	// AllowAll: neither key — unrestricted, policy-gated, disclosed. The
	// web tools likewise: web_read reaches any public address, and holds
	// its own address rule (toolsrv/web.go), and the scans and searches go
	// to the registry or to the person's own vendors.

	// The web tools' keys (the job's credential hosted, the person's Jev
	// and search keys local) are the tools container's alone, as a
	// declared secret is; checkWebGrant leaves no declared secret beside
	// them.
	if tspec.Web.Enabled {
		secrets = append(append([]string(nil), secrets...), toolsrv.WebSecretEnv()...)
	}
	// The review's key, the person's own, likewise (checkReviewGrant).
	if tspec.Review.Enabled {
		secrets = append(append([]string(nil), secrets...), toolsrv.ReviewSecretEnv()...)
	}
	if len(secrets) > 0 {
		var entries []*yaml.Node
		for _, name := range sortedCopy(secrets) {
			s := yamlMap{}
			s.set("name", strNode(SecretKey(name)))
			s.set("env", strNode(name))
			entries = append(entries, s.node())
		}
		e.set("secrets", seqNode(entries))
	}
	return e.node()
}

// workspaceVolumes maps the filesystem contract to bind mounts (§R4 mount
// table): whole-workspace modes get one bind, paths mode gets per-path
// binds; a write grant makes its bind rw (the tools server still refuses
// reads the read grant doesn't cover).
func workspaceVolumes(caps spec.CapSummary) []string {
	ws, mnt := spec.PHWorkspace, spec.WorkspaceMount
	wholeRead, wholeWrite := caps.FSReadMode == "workspace", caps.FSWriteMode == "workspace"
	switch {
	case wholeWrite:
		return []string{ws + ":" + mnt + ":rw"}
	case wholeRead && caps.FSWriteMode == "none":
		return []string{ws + ":" + mnt + ":ro"}
	case wholeRead && caps.FSWriteMode == "paths":
		vols := []string{ws + ":" + mnt + ":ro"}
		for _, p := range caps.FSWritePaths {
			vols = append(vols, ws+"/"+p+":"+mnt+"/"+p+":rw")
		}
		return vols
	}
	// No whole-workspace grant: per-path binds only.
	rw := map[string]bool{}
	for _, p := range caps.FSWritePaths {
		rw[p] = true
	}
	set := map[string]bool{}
	for _, p := range append(append([]string{}, caps.FSReadPaths...), caps.FSWritePaths...) {
		set[p] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var vols []string
	for _, p := range paths {
		mode := ":ro"
		if rw[p] {
			mode = ":rw"
		}
		vols = append(vols, ws+"/"+p+":"+mnt+"/"+p+mode)
	}
	return vols
}

// SecretKey is the gateway secrets-file key for a declared env name. One
// definition shared by the catalog entry and the engine's secrets.env
// writer, so they cannot drift.
func SecretKey(envName string) string {
	return spec.ToolServerName + "." + strings.ToLower(envName)
}

// dataAccessServers loads operator-cataloged server entries for the
// declared categories (§R3). Each policy entry is a YAML file containing a
// single-key mapping {name: <server entry>} — the gateway's own
// file://server.yaml shape.
func dataAccessServers(categories []string, pol *policy.Policy) ([]string, map[string]yaml.Node, error) {
	if len(categories) == 0 || len(pol.DataAccessServers) == 0 {
		return nil, nil, nil
	}
	entries := map[string]yaml.Node{}
	var names []string
	for _, cat := range categories {
		for _, path := range pol.DataAccessServers[cat] {
			b, err := os.ReadFile(path) //nolint:gosec // operator-authored policy path
			if err != nil {
				return nil, nil, &PolicyRefusal{Reason: fmt.Sprintf("dataAccess %s server file: %v", cat, err)}
			}
			var doc map[string]yaml.Node
			if err := yaml.Unmarshal(b, &doc); err != nil || len(doc) != 1 {
				return nil, nil, &PolicyRefusal{Reason: fmt.Sprintf("dataAccess server file %s must be a single {name: entry} mapping", path)}
			}
			for name, node := range doc {
				if name == spec.ToolServerName {
					return nil, nil, &PolicyRefusal{Reason: fmt.Sprintf("dataAccess server file %s reuses the reserved name %q", path, name)}
				}
				if _, dup := entries[name]; !dup {
					names = append(names, name)
				}
				entries[name] = node
			}
		}
	}
	sort.Strings(names)
	return names, entries, nil
}

// enforcement writes the §R8 capabilities_enforced disclosure — each
// mechanism named precisely, per the honest-posture rule.
func enforcement(caps spec.CapSummary, secrets, optional []string, extraServers []string, pol *policy.Policy, mcpServers []spec.MCPServerSpec) map[string]string {
	e := map[string]string{}
	switch caps.EgressMode {
	case "none":
		e["network"] = "no egress: tools container runs with networking disabled (disableNetwork); no fetch tool exposed"
	case "hosts":
		e["network"] = "egress pinned to declared hosts via gateway per-server proxies + DNS interception (--block-network) and re-checked per request/redirect hop by the fetch tool: " + strings.Join(caps.EgressHosts, ", ")
	case "all":
		e["network"] = "UNRESTRICTED tool-plane egress (manifest \"*\", permitted by operator policy allowWildcardEgress)"
	}
	if caps.WebTools {
		tools, read := toolsrv.ToolWebSearch+" (only with a search to use) and "+toolsrv.ToolWebRead, "every page and search result"
		if caps.RegistrySearch {
			tools = toolsrv.ToolWebSearch + " (only with a search to use), " + toolsrv.ToolWebRead + " and " + toolsrv.ToolRegistrySearch
			read = "every page, search result and registry answer"
		}
		e["network"] = "no declared egress and no fetch tool; the built-in's web tools alone reach out, from a tools container with no network allowlist of its own, each holding its own bounds (see web)"
		if caps.WebSearchedOnly {
			tools += ", " + toolsrv.ToolWebRead + " opening only an address a " + toolsrv.ToolWebSearch + " of this run returned and AI Review passed"
		}
		e["web"] = "the built-in's web tools " + tools +
			", on the tools container only: " + toolsrv.ToolWebRead + " is GET only, http and https on ports 80 and 443, public addresses only, checked at every connection and redirect hop, no cookie or credential, at most 32 KiB of text a page; " +
			read + " is read by AI Review (Jev) before the model, and one flagged or not scanned is withheld with its address and the reason; " +
			"the keys (the job's credential, or the person's own Jev and search keys) are injected into the tools container only"
	}
	if caps.ReviewTools {
		e["network"] = "no declared egress and no fetch tool; the tools container reaches " + toolsrv.ReviewHost + ":443 alone, TypeSafe's API, for the review grant, " +
			"through the gateway's per-server proxy and DNS interception (--block-network)"
		e["review"] = "the built-in's review grant, on the tools container only: " + toolsrv.ToolAIReview + " asks AI Review's release rubric (the six questions stage S5 asks) over the package in the run's inputs and answers with scores and file names, never the package's text; " +
			toolsrv.ToolReadInput + " answers a file only after AI Review's model-input questions passed it, and one flagged or not scanned is withheld with its path and the reason; " +
			"the person's own TypeSafe key is injected into the tools container only"
	}
	e["filesystem"] = "read " + fsDesc(caps.FSReadMode, caps.FSReadPaths) +
		"; write " + fsDesc(caps.FSWriteMode, caps.FSWritePaths) +
		" — enforced by the tools-container mount table (ro/rw, per-path binds) plus server-side grant checks; " +
		fmt.Sprintf("on Linux the run's docker filter mounts the tools container's %s as a %d MiB tmpfs and the tools server refuses to serve without it; under Docker Desktop (macOS, Windows), which runs no filter, it is the container's own layer, unbounded", spec.ToolsTmpMount, spec.ToolsTmpfsBytes>>20)
	if caps.Scratch {
		e["pdf"] = fmt.Sprintf("the runner's grant, not the contract's: %s renders a Markdown file from reports/ or the scratch folder into a PDF in reports/ with Typst %s in the tools container (built without a package downloader), through the runner's confine role: an empty environment, a fresh render root under %s whose every file the tools server makes new, no write of its own (Landlock; the PDF comes back on its stdout), TCP refused where Landlock is ABI 4 or later, %d GiB of address space and %d seconds; as root it runs as a user of its own that owns nothing, and where there is no Landlock that user is all that holds its writes. Only local PNG and JPEG images from reports/, the scratch folder and the inputs are drawn. The theme is the run's, never the model's, and the receipt's pdfs block names it and each PDF's SHA-256; write_report refuses a PDF",
			toolsrv.ToolExportPDF, toolsrv.TypstVersion, spec.ToolsTmpMount, toolsrv.PDFRenderMemory>>30, int(toolsrv.PDFRenderTimeout.Seconds()))
		e["scratch"] = fmt.Sprintf("the runner's grant, not the contract's: %s, %s and %s over the scratch folder %s on the tools container only, empty at the start, never copied to the output folder, and removed with the container; on Linux a %d MiB tmpfs the run's docker filter mounts, under Docker Desktop a folder in the container's own layer, unbounded",
			toolsrv.ToolScratchList, toolsrv.ToolScratchRead, toolsrv.ToolScratchWrite, spec.ScratchMount, spec.ToolsTmpfsBytes>>20)
	}
	if len(caps.ExecCommands) == 0 {
		e["exec"] = "no exec: run_command tool not exposed; harness built-in shell toolsets never enabled"
	} else {
		e["exec"] = execEnforcement(caps.ExecCommands, pol.Exec)
	}
	// A declared server's secrets are resolved and refused exactly as the
	// release's own (engine.secretValues) but reach only that server's
	// container, so the line names each with its server (agentry-cli#376):
	// "none declared" beside a server that was handed a key would state less
	// than the run enforced.
	var serverSecrets, remoteSecrets, alsoOwn []string
	for _, srv := range mcpServers {
		if srv.SendsBearer() {
			for _, name := range srv.Secrets {
				if slices.Contains(secrets, name) {
					alsoOwn = append(alsoOwn, name)
				}
			}
		}
		switch {
		case len(srv.Secrets) == 0:
		case srv.Remote():
			// A remote server's bearer (agentry-cli#663) is not one of the
			// gateway's secrets: the engine writes it into the server's
			// connection folder for its bridge alone.
			remoteSecrets = append(remoteSecrets, srv.Package+": "+strings.Join(sortedCopy(srv.Secrets), ", "))
		default:
			serverSecrets = append(serverSecrets, srv.Package+"@"+srv.Version+": "+strings.Join(sortedCopy(srv.Secrets), ", "))
		}
	}
	serverPart := "each declared server's own, injected only into that server's container via gateway secret scoping: " + strings.Join(serverSecrets, "; ")
	remotePart := "each remote server's bearer, written by the runner into a run-owned folder mounted read-only into that server's bridge alone, never into the gateway's secrets file, and deleted at teardown: " + strings.Join(remoteSecrets, "; ")
	switch {
	case len(secrets) == 0 && len(serverSecrets) == 0 && len(remoteSecrets) == 0:
		e["secrets"] = "none declared"
	case len(secrets) == 0 && len(serverSecrets) == 0:
		e["secrets"] = "declared servers' values resolved at run start (missing ⇒ refuse, exit 11), absent from plan and receipts; " + remotePart
	case len(secrets) == 0:
		e["secrets"] = "declared servers' values resolved at run start (missing ⇒ refuse, exit 11), absent from plan and receipts; " + serverPart
		if len(remoteSecrets) > 0 {
			e["secrets"] += "; " + remotePart
		}
	default:
		e["secrets"] = "values resolved at run start (missing ⇒ refuse, exit 11), injected only into the tools container via gateway secret scoping, absent from plan and receipts: " + strings.Join(sortedCopy(secrets), ", ")
		if len(serverSecrets) > 0 {
			e["secrets"] += "; " + serverPart
		}
		if len(remoteSecrets) > 0 {
			e["secrets"] += "; " + remotePart
		}
	}
	// An optional secret (capabilities.optionalSecrets, DR-70) is the one
	// exception to "missing ⇒ refuse": a run starts without it, the runner
	// writing it empty into the secrets file, which the tools server reads
	// as not set. The names a run went without are the receipt's
	// secrets.absent.
	if len(optional) > 0 {
		e["secrets"] += "; except the optional ones, which a run starts without, written empty into the secrets file when unresolved: " + strings.Join(sortedCopy(optional), ", ")
	}
	// "Injected only into" is where a value is put, not every place it is:
	// the gateway reads the whole --secrets file to inject them, so its own
	// container holds every value, the release's and each server's
	// (agentry-cli#404).
	if len(secrets) > 0 || len(serverSecrets) > 0 {
		e["secrets"] += "; the MCP gateway, which injects them, reads the run's whole secrets file, so its container holds every value too"
		if len(remoteSecrets) > 0 && len(alsoOwn) == 0 {
			e["secrets"] += ", a remote server's bearer excepted"
		}
	}
	// A bearer whose name the release also declares as its own secret is
	// that secret's value too: it is in the gateway's file for the tools
	// container under the release's name, and the line must not say less.
	if len(alsoOwn) > 0 {
		e["secrets"] += "; " + strings.Join(sortedCopy(alsoOwn), ", ") + " is also the release's own secret, so its value is in the gateway's secrets file for the tools container as well, and the gateway holds it"
	}
	proxyLine := fmt.Sprintf("CONNECT allowlist: %s", strings.Join(pol.Model.ProxyAllow, ", "))
	if plain := plainHTTPTargets(pol); len(plain) > 0 {
		proxyLine += "; plain http forwarded, unencrypted, to the operator's http model endpoint(s): " + strings.Join(plain, ", ")
	}
	e["agentContainer"] = "internal-only run network; reachable endpoints: " + spec.GatewayAlias +
		fmt.Sprintf(":%d (Bearer auth) and %s:%d (%s); no workspace, output, or docker-socket mounts; model API key present by necessity",
			spec.GatewayPort, spec.ProxyAlias, spec.ProxyPort, proxyLine)
	if len(caps.DataAccess) > 0 {
		if len(extraServers) > 0 {
			e["dataAccess"] = "declared " + strings.Join(caps.DataAccess, ", ") + "; operator-cataloged servers attached: " + strings.Join(extraServers, ", ")
		} else {
			e["dataAccess"] = "declared " + strings.Join(caps.DataAccess, ", ") + "; no servers cataloged by policy (declaration recorded for audit)"
		}
	}
	var remote []string
	for _, srv := range mcpServers {
		if !srv.Remote() {
			continue
		}
		d := srv.Package + " (auth " + srv.Auth
		switch {
		case srv.SignsIn():
			d += ", the person's connection for this release: an access token alone, refreshed by the runner before the run and again before it expires during a local run, and written into a folder mounted read-only into its bridge; the refresh token and any client secret never enter the run"
		case srv.SendsBearer():
			d += ", " + strings.Join(srv.Secrets, ", ") + " sent as its bearer"
		}
		if len(srv.Tools) > 0 {
			d += ", tools filtered to " + strings.Join(sortedCopy(srv.Tools), "/")
		}
		d += ", reaching " + srv.RemoteHost() + ":443 alone)"
		remote = append(remote, d)
	}
	if len(remote) > 0 {
		e["mcpServers.remote"] = "declared remote servers, run at their vendors and reached through the runner image's bridge, a stdio server the gateway launches like a package server: on the gateway's relay network behind one relay to the address's host, under the run's docker filter and runtime; the gateway dials none of them and keeps no network of its own; what the vendor's server does is not checked here: " + strings.Join(remote, "; ")
	}
	if spec.HasPackageServers(mcpServers) {
		var parts []string
		for _, srv := range mcpServers {
			if srv.Remote() {
				continue
			}
			d := fmt.Sprintf("%s@%s (%s, %s", srv.Package, srv.Version, srv.Source, egressDesc(srv.Egress))
			if len(srv.Tools) > 0 {
				d += ", tools filtered to " + strings.Join(sortedCopy(srv.Tools), "/")
			}
			if len(srv.Env) > 0 {
				// Keys only: a receipt never carries a configured value.
				d += ", env keys " + strings.Join(slices.Sorted(maps.Keys(srv.Env)), "/")
			}
			if srv.OAuth != nil {
				d += ", " + oauthDesc(srv)
			}
			d += ")"
			parts = append(parts, d)
		}
		e["mcpServers"] = "declared servers installed per-signature from the operator's registries, launched from the release's read-only store on no network of the run's own — a server with declared hosts sits on the gateway's relay network with a TCP relay linked under each host's name — a network the runner refuses to share: the run does not start while another gateway's relays are on it (checked as the run starts; a gateway started during the run is not refused), and its own relays are removed at teardown — one with none has no network — with server-scoped secrets and the manifest's plain env: " + strings.Join(parts, "; ")
	}
	return e
}

// oauthDesc is the receipt's account of a package server's OAuth sign-in
// (agentry-cli#665): where its access token goes, by file and env name,
// and that nothing else of the connection does. Never a template.
func oauthDesc(srv spec.MCPServerSpec) string {
	o := srv.OAuth
	d := "signed in at " + o.Issuer + " by the person's connection for this release: an access token alone, refreshed by the runner before the run"
	var where []string
	if len(o.Files) > 0 {
		names := make([]string, 0, len(o.Files))
		for _, f := range o.Files {
			names = append(names, f.Path)
		}
		where = append(where, "rendered into "+strings.Join(names, "/")+" in the server's own folder at "+spec.ConnectionMount+" (writable by the server, deleted at teardown) and rewritten before it expires during a local run")
	}
	var tokenEnv []string
	for name := range o.Env {
		if o.TokenEnv(name) {
			tokenEnv = append(tokenEnv, name)
		}
	}
	if len(tokenEnv) > 0 {
		where = append(where, "set in env "+strings.Join(slices.Sorted(slices.Values(tokenEnv)), "/")+" through the gateway's secrets file, fixed for the run")
	}
	if len(where) > 0 {
		d += ", " + strings.Join(where, ", and ")
	}
	return d + "; the refresh token and any client secret never enter the run"
}

// execEnforcement is the exec line of the receipt: the allowlist, the
// catalogued package behind each command, and the git accommodation. The
// resolved package versions are appended by the engine once the image
// build has produced them; the compiled line never claims a version.
func execEnforcement(commands []string, cat policy.ExecPolicy) string {
	var from []string
	git := false
	for _, c := range sortedCopy(commands) {
		from = append(from, c+" ← apk "+cat.Package(c))
		if c == "git" {
			git = true
		}
	}
	line := "run_command restricted to argv0 allowlist + shim PATH {" + strings.Join(commands, ", ") +
		"}; tools container is the runner image with the operator's catalogued packages installed (" + strings.Join(from, ", ") +
		") and minimized to exactly those binaries (per-QuickAgent exec image, agentry-cli#9, #100)"
	if git {
		// The workspace is a bind mount owned by the host user and the
		// tools container runs as root; git refuses such a repository as
		// "dubious ownership". The run's mounts are the only places a
		// repository can be, so those paths are trusted, nothing wider.
		line += "; git: safe.directory covers the run's workspace and inputs mounts in the image's /etc/gitconfig, so a bind mount owned by the host user is not refused as dubiously owned"
	}
	return line
}

// unsafeExecEnforcement is the exec line of an Unsafe Mode plan: it names
// Unsafe Mode, the base by digest, where each declared command comes from,
// every command the image cannot place, and what is not enforced that the
// catalogue's line enforces (no minimize, the image's PATH).
func unsafeExecEnforcement(img *spec.ExecImageSpec) string {
	u := img.Unsafe
	commands := img.Commands
	from := map[string]string{}
	for _, e := range u.Extras {
		from[e.Command] = e.String()
	}
	unplaced := map[string]bool{}
	var missing []string
	for _, c := range u.Unplaced {
		unplaced[c.Command] = true
		m := c.Command
		if len(c.Candidates) > 0 {
			m += " (several packages ship it: " + strings.Join(c.Candidates, ", ") + ")"
		}
		missing = append(missing, m)
	}
	var placed []string
	for _, c := range sortedCopy(commands) {
		switch {
		case unplaced[c]:
		case from[c] != "":
			placed = append(placed, c+" ← "+from[c])
		default:
			placed = append(placed, c+" ← apt")
		}
	}
	line := "UNSAFE MODE: run_command restricted to the argv0 allowlist {" + strings.Join(commands, ", ") +
		"}; the tools container is the Unsafe Mode image " + u.Base + " with the declared commands installed one at a time, from Kali Linux's repository or, for a pip extra, PyPI (" +
		strings.Join(placed, ", ") + " — apt packages: " + cmp.Or(strings.Join(img.Packages, ", "), "none") +
		"); this machine's exec catalogue was not consulted; the image is not minimized, and a declared command runs with the image's own PATH after its shims, so it can reach every program the image holds (per-QuickAgent Unsafe Mode exec image, plan unsafe-mode.md, agentry-cli#490)"
	if len(missing) > 0 {
		line += "; not available in Unsafe Mode's image: " + strings.Join(missing, "; ")
	}
	if slices.Contains(commands, "git") {
		line += "; git: safe.directory covers the run's workspace and inputs mounts in the image's /etc/gitconfig, so a bind mount owned by the host user is not refused as dubiously owned"
	}
	return line
}

func egressDesc(hosts []string) string {
	for _, h := range hosts {
		if h == "*" {
			return "UNRESTRICTED egress, permitted by policy"
		}
	}
	if len(hosts) == 0 {
		return "no egress"
	}
	return "egress " + strings.Join(sortedCopy(hosts), ",")
}

func fsDesc(mode string, paths []string) string {
	switch mode {
	case "none":
		return "none"
	case "workspace":
		return "workspace"
	default:
		return "paths {" + strings.Join(paths, ", ") + "}"
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// modelKeySource is the variable a run reads the model key from and, when
// the policy says keyring, the keychain account it falls back to — by the
// policy's one rule (policy.KeyEnv, policy.KeySource).
func modelKeySource(pol *policy.Policy, adapterName string) (keyEnv, account string) {
	hp := pol.Harness[adapterName]
	keyEnv = policy.KeyEnv(hp, pol.Model)
	if policy.KeySource(hp, pol.Model) == modelkey.SourceKeyring {
		account = modelkey.Account(pol.Model.Provider)
	}
	return keyEnv, account
}

// plainHTTPTargets is the host:port of every plain-http model base URL in
// the policy (the model's and each host harness's) that the CONNECT
// allowlist already names: the targets the proxy may forward plain http
// to (DR-45, agentry-cli#377). Intersecting with the allowlist means a
// plain target never reaches a host a tunnel could not; https endpoints
// contribute nothing, so an all-https policy compiles as before.
func plainHTTPTargets(pol *policy.Policy) []string {
	if pol == nil {
		return nil
	}
	urls := []string{pol.Model.BaseURL}
	for _, hp := range pol.Harness {
		urls = append(urls, hp.BaseURL)
	}
	allowed := map[string]bool{}
	for _, a := range pol.Model.ProxyAllow {
		allowed[strings.ToLower(strings.TrimSpace(a))] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "http" || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "80"
		}
		hp := strings.ToLower(net.JoinHostPort(u.Hostname(), port))
		if allowed[hp] && !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	sort.Strings(out)
	return out
}
