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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
	// Source is "registry" or "local".
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
	// IntegrityKey is the runner home's plan-integrity key (IntegrityKey).
	// It is not part of the plan: when set, the compile also writes
	// plan.json.hmac, so a later run can reuse the plan only as compiled
	// (ReadInstalledPlan, agentry-cli#266). Nil writes no record, and a
	// reader then recompiles rather than trust the plan.
	IntegrityKey []byte
}

// PolicyRefusal is a run refused by operator policy — the §R8 exit-11
// class, distinct from a malformed package (exit 12).
type PolicyRefusal struct{ Reason string }

func (e *PolicyRefusal) Error() string { return "refused by policy: " + e.Reason }

// OutputSchemaFile is the §R2 structured-output convention.
const OutputSchemaFile = "output.schema.json"

// contractPostscript is the fixed runner contract appended to every
// instruction (§R2). Golden tests pin it; changing it changes every plan
// hash, which is the point.
const contractPostscript = "\n\n----\n" +
	"Runner contract (Agentry): You are running as a one-shot BriefAgent inside " +
	"an isolated container. Your only tools are those provided by the connected MCP " +
	"gateway — there is no other shell, filesystem, or network access. The task " +
	"workspace, as seen by your tools, is rooted at " + spec.WorkspaceMount + ". Input " +
	"files provided for this run, if any, are under " + spec.InputsMount + " — list " +
	"them with list_inputs and read them with read_input. Files you want to deliver " +
	"as reports must be written with the write_report tool; they land in the run's " +
	"output folder. When the task is complete, send your final answer as your last " +
	"message.\n"

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
	instructionPath, instruction, err := deriveInstruction(m, in.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	instruction += contractPostscript

	schemaJSON, output, err := loadOutputSchema(in.WorkspaceDir)
	if err != nil {
		return nil, err
	}

	tspec := toolSpec(caps, m.Capabilities.Secrets)
	extraServers, extraEntries, err := dataAccessServers(caps.DataAccess, in.Policy)
	if err != nil {
		return nil, err
	}

	// Declared MCP servers (RUNNER-PLAN-3): policy-gate their egress and
	// refuse name collisions before anything renders.
	for _, srv := range in.MCPServers {
		if err := checkLiteralEnv(srv); err != nil {
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

	files := map[string][]byte{}
	// The tools server runs in the shared runner image unless the agent
	// declares exec, in which case it runs in that agent's exec image —
	// the runner image with the operator's catalogued packages installed,
	// minimized to the declared commands (agentry-cli#9, #100).
	toolsImage := in.RunnerImage
	execImage, err := execImageFor(caps.ExecCommands, in.RunnerImage, in.Policy.Exec)
	if err != nil {
		return nil, err
	}
	if execImage != nil {
		toolsImage = execImage.Tag
	}
	if err := renderGatewayFiles(files, tspec, caps, m.Capabilities.Secrets, toolsImage, extraServers, extraEntries, in.MCPServers); err != nil {
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

	servers := append([]string{spec.ToolServerName}, extraServers...)
	for _, srv := range in.MCPServers {
		servers = append(servers, srv.Name)
	}
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
		},
		Capabilities: caps,
		Tools:        tspec.GrantedTools(),
		MCPServers:   in.MCPServers,
		Enforcement:  enforcement(caps, m.Capabilities.Secrets, extraServers, in.Policy, in.MCPServers),
		ExecImage:    execImage,
		Gateway:      gatewaySpec(in.Policy.Images.Gateway, servers, caps, len(in.MCPServers) > 0),
		Harness: spec.HarnessSpec{
			Adapter:    adapterName,
			Model:      res.Model,
			Agent:      res.Agent,
			Host:       res.Host,
			ResultKind: res.ResultKind,
			Session:    res.Session,
		},
		Proxy: spec.ProxySpec{
			Image: in.RunnerImage,
			Allow: append([]string(nil), in.Policy.Model.ProxyAllow...),
		},
		Secrets: sortedCopy(m.Capabilities.Secrets),
		Budgets: spec.Budgets{
			TimeoutSeconds: in.Policy.Budgets.TimeoutSeconds,
			MaxTurns:       in.Policy.Budgets.MaxTurns,
			MaxCostUSD:     in.Policy.Budgets.MaxCostUSD,
		},
		Output: output,
		Files:  map[string]string{},
	}

	if rt := in.Policy.Docker.Runtime; rt != "" {
		// The runtime is part of the plan and so of its hash: an operator
		// who changes it recompiles rather than reinterpreting a stored
		// plan (DR-23). The enforcement line says exactly what runs under
		// it — the stranger's code: the agent container and the MCP server
		// installer — and what does not: the runner's own infrastructure
		// (the gateway needs the daemon's socket and its DNS, the proxy is
		// the bridge leg) and what the gateway launches on its own; so
		// `runtime:` never claims more ground than the engine holds.
		plan.Docker = &spec.DockerSpec{Runtime: rt}
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		plan.Enforcement["runtime"] = rt + ": the agent container — the BriefAgent's code — and the MCP server installer are created with docker --runtime " + rt +
			", the run network's aliases pinned into the agent's /etc/hosts; the runner's own llm-proxy, gateway and probe, and what the gateway launches itself (the tools server, the declared MCP servers), take the daemon's default runtime"
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
		return "", "", fmt.Errorf("%s/%s declares no instruction file, so it is not a runnable BriefAgent (RUNNER-PLAN §R2; the withdrawn mcp-server kind is a tool server)", m.Namespace, m.Name)
	}
	for _, rel := range candidates {
		b, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel))) //nolint:gosec // path from the verified archive
		if err == nil {
			return rel, stripFrontmatter(string(b)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("read instruction %s: %w", rel, err)
		}
	}
	return "", "", fmt.Errorf("instruction file missing from the package: looked for %s (RUNNER-PLAN §R2)", strings.Join(candidates, ", "))
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
		FSRead: toolsrv.Access{
			Enabled: caps.FSReadMode != "none",
			Paths:   caps.FSReadPaths,
		},
		FSWrite: toolsrv.Access{
			Enabled: caps.FSWriteMode != "none",
			Paths:   caps.FSWritePaths,
		},
		Fetch: toolsrv.Fetch{
			Enabled:   caps.EgressMode == "hosts" || caps.EgressMode == "all",
			Hosts:     caps.EgressHosts,
			AllowAll:  caps.EgressMode == "all",
			SecretEnv: sortedCopy(secrets),
		},
		Exec: toolsrv.Exec{Commands: caps.ExecCommands},
	}
}

// GatewayConfigDir is where the rendered gateway files are mounted inside
// the gateway container. It must be the gateway's own catalogs directory:
// since v0.43 a local --catalog path "must resolve within Docker MCP
// catalogs directory" (pkg/catalog, errUntrustedLocalPath) and the
// gateway exits at startup otherwise (agentry-cli#41). HOME is pinned to
// /root in the container env so the directory is the same on every host.
const GatewayConfigDir = "/root/.docker/mcp/catalogs"

// gatewaySpec builds the gateway container launch (§R4). The flag set is
// compiled, and --block-network is unconditional: without it allowHosts is
// advisory, which is the sharpest foot-gun the research flagged (§R11
// invariant 1 pins this in a test). The guards that the gateway itself
// defaults on are passed explicitly too, so the run never inherits one it
// depends on from a version's default.
func gatewaySpec(image string, servers []string, caps spec.CapSummary, withStore bool) spec.GatewaySpec {
	writable := []string{spec.PHOut}
	if caps.FSWriteMode != "none" {
		writable = append(writable, spec.PHWorkspace)
	}
	return spec.GatewaySpec{
		Image: image,
		Args: []string{
			"--transport", "streaming",
			"--port", fmt.Sprintf("%d", spec.GatewayPort),
			"--servers", strings.Join(servers, ","),
			"--catalog", GatewayConfigDir + "/catalog.yaml",
			"--registry", GatewayConfigDir + "/registry.yaml",
			"--config", GatewayConfigDir + "/config.yaml",
			"--tools-config", GatewayConfigDir + "/tools.yaml",
			"--secrets", GatewayConfigDir + "/secrets.env",
			"--block-network",
			"--verify-signatures",
			"--log-calls",
			// Default true in the pinned gateway, passed anyway for the
			// same reason --verify-signatures and --log-calls are: a run
			// depends on it — the refusal of a secret-looking tool result
			// is in the enforcement table the receipts answer to
			// (agentry#297) — and a version whose default flipped would
			// drop the guard while the receipt still claimed it.
			"--block-secrets",
		},
		Env: []spec.EnvVar{
			{Name: spec.GatewayAuthTokenEnv, FromRunToken: true},
			// Pins the catalogs directory (see GatewayConfigDir); the
			// image sets no HOME and runs as root.
			{Name: "HOME", Value: "/root"},
			// Bind-mount containment (gateway v0.43.1+): sources must
			// resolve under these roots; writable binds need exact
			// entries. Separator ":" — verified empirically at M-R2.
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
	secrets []string, runnerImage string, extraServers []string, extraEntries map[string]yaml.Node,
	mcpServers []spec.MCPServerSpec,
) error {
	tspecJSON, err := json.MarshalIndent(tspec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal toolspec: %w", err)
	}
	files["gateway/toolspec.json"] = append(tspecJSON, '\n')

	entry := catalogEntry(tspec, caps, secrets, runnerImage)
	catalog := yamlMap{}
	catalog.set(spec.ToolServerName, entry)
	for _, name := range extraServers {
		node := extraEntries[name]
		catalog.set(name, &node)
	}
	for _, srv := range mcpServers {
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
			return fmt.Errorf("declared MCP server %s: env %s: the value is empty and the gateway would not set it; declare a value or leave the key out", srv.Package, name)
		}
		if !manifest.LiteralEnvValue(v) {
			return fmt.Errorf("declared MCP server %s: env %s: the value contains $ or {{ }}, which the gateway would interpret; declare a literal value", srv.Package, name)
		}
		if strings.Contains(v, "@@") {
			// The engine rewrites @@RUN_*@@ placeholders in plan files into
			// host paths; a manifest value must not be able to spell one.
			return fmt.Errorf("declared MCP server %s: env %s: the value contains @@, the run placeholder marker; declare a literal value", srv.Package, name)
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
		return nil, &PolicyRefusal{Reason: fmt.Sprintf(
			"manifest declares exec command(s) %s that this machine's policy does not catalogue: add each to exec.packages (command: apk package, or busybox for an applet the runner image already has)",
			strings.Join(missing, ", "))}
	}
	sort.Strings(pkgs)
	return &spec.ExecImageSpec{
		Tag:       spec.ExecImageTag(runnerImage, commands, pkgs, cat.APKMirror),
		Commands:  sortedCopy(commands),
		Packages:  pkgs,
		APKMirror: cat.APKMirror,
	}, nil
}

// bindRoots lists the host roots the gateway may bind into the containers
// it launches: the run's own directories, plus the signature's server
// store when declared servers exist — each server is launched from a
// read-only bind of that store, and the gateway refuses a source outside
// its roots (agentry-cli#105). Left out otherwise so a plan without
// servers keeps its pre-PLAN-3 bytes.
func bindRoots(withServers bool) []string {
	roots := []string{spec.PHWorkspace, spec.PHOut, spec.PHCfg, spec.PHInputs}
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
	e.set("volumes", strListNode([]string{
		spec.PHServers + "/" + srv.StoreSubdir + ":" + spec.MCPStorePrefix + "/" + srv.StoreSubdir + ":ro",
	}))
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
	if len(srv.Secrets) > 0 {
		var entries []*yaml.Node
		for _, name := range sortedCopy(srv.Secrets) {
			s := yamlMap{}
			s.set("name", strNode(ServerSecretKey(srv.Name, name)))
			s.set("env", strNode(name))
			entries = append(entries, s.node())
		}
		e.set("secrets", seqNode(entries))
	}
	if len(srv.Env) > 0 {
		var entries []*yaml.Node
		for _, name := range slices.Sorted(maps.Keys(srv.Env)) {
			kv := yamlMap{}
			kv.set("name", strNode(name))
			kv.set("value", strNode(srv.Env[name]))
			entries = append(entries, kv.node())
		}
		e.set("env", seqNode(entries))
	}
	return e.node()
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
	e.set("description", strNode("Agentry BriefAgent tools server (generated per run)"))
	e.set("title", strNode("BriefAgent Tools"))
	e.set("type", strNode("server"))
	e.set("image", strNode(runnerImage))
	e.set("command", strListNode([]string{"tools", "--spec", spec.ToolSpecMount}))
	e.set("longLived", boolNode(true))

	vols := []string{spec.PHCfg + "/gateway/toolspec.json:" + spec.ToolSpecMount + ":ro"}
	vols = append(vols, workspaceVolumes(caps)...)
	// The run-inputs channel (P2 §P4): read-only by mount AND by tool
	// surface; the output/receipts volume stays separate below.
	vols = append(vols, spec.PHInputs+":"+spec.InputsMount+":ro")
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
	} else if !tspec.Fetch.Enabled {
		e.set("disableNetwork", boolNode(true))
	}
	// AllowAll: neither key — unrestricted, policy-gated, disclosed.

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
func enforcement(caps spec.CapSummary, secrets []string, extraServers []string, pol *policy.Policy, mcpServers []spec.MCPServerSpec) map[string]string {
	e := map[string]string{}
	switch caps.EgressMode {
	case "none":
		e["network"] = "no egress: tools container runs with networking disabled (disableNetwork); no fetch tool exposed"
	case "hosts":
		e["network"] = "egress pinned to declared hosts via gateway per-server proxies + DNS interception (--block-network) and re-checked per request/redirect hop by the fetch tool: " + strings.Join(caps.EgressHosts, ", ")
	case "all":
		e["network"] = "UNRESTRICTED tool-plane egress (manifest \"*\", permitted by operator policy allowWildcardEgress)"
	}
	e["filesystem"] = "read " + fsDesc(caps.FSReadMode, caps.FSReadPaths) +
		"; write " + fsDesc(caps.FSWriteMode, caps.FSWritePaths) +
		" — enforced by the tools-container mount table (ro/rw, per-path binds) plus server-side grant checks"
	if len(caps.ExecCommands) == 0 {
		e["exec"] = "no exec: run_command tool not exposed; harness built-in shell toolsets never enabled"
	} else {
		e["exec"] = execEnforcement(caps.ExecCommands, pol.Exec)
	}
	if len(secrets) == 0 {
		e["secrets"] = "none declared"
	} else {
		e["secrets"] = "values resolved at run start (missing ⇒ refuse, exit 11), injected only into the tools container via gateway secret scoping, absent from plan and receipts: " + strings.Join(sortedCopy(secrets), ", ")
	}
	e["agentContainer"] = "internal-only run network; reachable endpoints: " + spec.GatewayAlias +
		fmt.Sprintf(":%d (Bearer auth) and %s:%d (CONNECT allowlist: %s); no workspace, output, or docker-socket mounts; model API key present by necessity",
			spec.GatewayPort, spec.ProxyAlias, spec.ProxyPort, strings.Join(pol.Model.ProxyAllow, ", "))
	if len(caps.DataAccess) > 0 {
		if len(extraServers) > 0 {
			e["dataAccess"] = "declared " + strings.Join(caps.DataAccess, ", ") + "; operator-cataloged servers attached: " + strings.Join(extraServers, ", ")
		} else {
			e["dataAccess"] = "declared " + strings.Join(caps.DataAccess, ", ") + "; no servers cataloged by policy (declaration recorded for audit)"
		}
	}
	if len(mcpServers) > 0 {
		var parts []string
		for _, srv := range mcpServers {
			d := fmt.Sprintf("%s@%s (%s, %s", srv.Package, srv.Version, srv.Source, egressDesc(srv.Egress))
			if len(srv.Tools) > 0 {
				d += ", tools filtered to " + strings.Join(sortedCopy(srv.Tools), "/")
			}
			if len(srv.Env) > 0 {
				// Keys only: a receipt never carries a configured value.
				d += ", env keys " + strings.Join(slices.Sorted(maps.Keys(srv.Env)), "/")
			}
			d += ")"
			parts = append(parts, d)
		}
		e["mcpServers"] = "declared servers installed per-signature from the operator's registries, launched from the release's read-only store on no network of the run's own — a server with declared hosts sits on the gateway's relay network with a TCP relay linked under each host's name — a network the runner refuses to share: the run does not start while another gateway's relays are on it (checked as the run starts; a gateway started during the run is not refused), and its own relays are removed at teardown — one with none has no network — with server-scoped secrets and the manifest's plain env: " + strings.Join(parts, "; ")
	}
	return e
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
		") and minimized to exactly those binaries (per-BriefAgent exec image, agentry-cli#9, #100)"
	if git {
		// The workspace is a bind mount owned by the host user and the
		// tools container runs as root; git refuses such a repository as
		// "dubious ownership". The run's mounts are the only places a
		// repository can be, so those paths are trusted, nothing wider.
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
