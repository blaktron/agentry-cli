// Package compile is the capability compiler (RUNNER-PLAN §R4): it turns
// manifest + operator policy into the run plan — generated gateway file
// set, tools-server spec, harness definition, and container topology.
// This is the layer that makes the registry's declared contract mean
// something at run time, so its two properties are non-negotiable:
// deterministic output (golden tests pin plan bytes) and one testable
// control per contract line (the Enforcement map states each mechanism).
package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// Inputs is everything a compile consumes. No run-scoped values belong
// here (§R4 determinism); the engine substitutes those at execute time.
type Inputs struct {
	// Manifest is the verified package manifest.
	Manifest *manifest.Manifest
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
	"Runner contract (SecureAgentry): You are running as a one-shot BriefAgent inside " +
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
// installed plan is detectable before execution (§R6).
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
		for _, h := range srv.Egress {
			if h == "*" && !in.Policy.AllowWildcardEgress {
				return nil, &PolicyRefusal{Reason: fmt.Sprintf(
					`declared MCP server %s requests egress "*" and this machine's policy has allowWildcardEgress: false`, srv.Name)}
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
	if err := renderGatewayFiles(files, tspec, caps, m.Capabilities.Secrets, in.RunnerImage, extraServers, extraEntries, in.MCPServers); err != nil {
		return nil, err
	}

	res, err := ad.Compile(adapter.Input{
		Instruction:  instruction,
		Summary:      m.Summary,
		Model:        in.Policy.Model,
		HarnessImage: in.Policy.Images.HarnessDockerAgent,
		GatewayURL:   spec.GatewayURL(),
		SchemaJSON:   schemaJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("adapter %s: %w", adapterName, err)
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
		Ref:          fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
		Digest:       in.Digest,
		Source:       in.Source,
		Kind:         string(m.Kind),
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
		Gateway:      gatewaySpec(in.Policy.Images.Gateway, servers, caps),
		Harness: spec.HarnessSpec{
			Adapter:    adapterName,
			Model:      res.Model,
			Agent:      res.Agent,
			ResultKind: res.ResultKind,
		},
		Proxy: spec.ProxySpec{
			Image: in.RunnerImage,
			Allow: append([]string(nil), in.Policy.Model.ProxyAllow...),
		},
		Secrets: sortedCopy(m.Capabilities.Secrets),
		Budgets: spec.Budgets{
			TimeoutSeconds: in.Policy.Budgets.TimeoutSeconds,
			MaxTurns:       in.Policy.Budgets.MaxTurns,
		},
		Output: output,
		Files:  map[string]string{},
	}

	// Write the generated files, hashing each into the plan.
	for rel, content := range files {
		abs := filepath.Join(planDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("create plan dir: %w", err)
		}
		if err := os.WriteFile(abs, content, 0o644); err != nil { //nolint:gosec // generated config, no secrets by construction
			return nil, fmt.Errorf("write %s: %w", rel, err)
		}
		plan.Files[rel] = sha256hex(content)
	}
	planBytes, err := plan.MarshalCanonical()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(planDir, "plan.json"), planBytes, 0o644); err != nil { //nolint:gosec // ditto
		return nil, fmt.Errorf("write plan.json: %w", err)
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
	if !m.Capabilities.Exec.IsZero() && !m.Capabilities.Exec.None {
		caps.ExecCommands = sortedCopy(m.Capabilities.Exec.Commands)
	}
	for _, d := range m.Capabilities.DataAccess {
		caps.DataAccess = append(caps.DataAccess, string(d))
	}
	sort.Strings(caps.DataAccess)
	return caps, nil
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

// deriveInstruction implements the §R2 closed rule for where the harness
// instruction comes from.
func deriveInstruction(m *manifest.Manifest, workspace string) (string, string, error) {
	switch m.Kind {
	case manifest.KindSkill:
		rel := m.Entrypoints.Skill
		if rel == "" {
			rel = "SKILL.md"
		}
		b, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel))) //nolint:gosec // path from the verified archive
		if err != nil {
			return "", "", fmt.Errorf("read skill entrypoint %s: %w", rel, err)
		}
		return rel, stripFrontmatter(string(b)), nil
	case manifest.KindPromptAgent:
		for _, rel := range []string{"AGENT.md", "PROMPT.md"} {
			b, err := os.ReadFile(filepath.Join(workspace, rel)) //nolint:gosec // fixed names under the verified archive
			if err == nil {
				return rel, stripFrontmatter(string(b)), nil
			}
		}
		return "", "", fmt.Errorf("prompt-agent package has neither AGENT.md nor PROMPT.md at its root (RUNNER-PLAN §R2)")
	case manifest.KindMCPServer:
		return "", "", fmt.Errorf("kind mcp-server is a tool server, not a runnable BriefAgent (RUNNER-PLAN §R2; attach it to other BriefAgents' runs instead)")
	default:
		return "", "", fmt.Errorf("kind %q is not runnable", m.Kind)
	}
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
	return b, spec.OutputSpec{SchemaFile: OutputSchemaFile, SchemaSHA256: sha256hex(b)}, nil
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

// gatewaySpec builds the gateway container launch (§R4). The flag set is
// compiled, and --block-network is unconditional: without it allowHosts is
// advisory, which is the sharpest foot-gun the research flagged (§R11
// invariant 1 pins this in a test).
func gatewaySpec(image string, servers []string, caps spec.CapSummary) spec.GatewaySpec {
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
			"--catalog", "/mcp/catalog.yaml",
			"--registry", "/mcp/registry.yaml",
			"--config", "/mcp/config.yaml",
			"--tools-config", "/mcp/tools.yaml",
			"--secrets", "/mcp/secrets.env",
			"--block-network",
			"--verify-signatures",
			"--log-calls",
		},
		Env: []spec.EnvVar{
			{Name: spec.GatewayAuthTokenEnv, FromRunToken: true},
			// Bind-mount containment (gateway v0.43.1+): sources must
			// resolve under these roots; writable binds need exact
			// entries. Separator ":" — verified empirically at M-R2.
			{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS",
				Value: strings.Join([]string{spec.PHWorkspace, spec.PHOut, spec.PHCfg, spec.PHInputs}, ":")},
			{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS",
				Value: strings.Join(writable, ":")},
		},
		Mounts: []spec.Mount{
			{Source: spec.PHCfg + "/gateway", Target: "/mcp", ReadOnly: true},
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

// mcpServerEntry renders one declared MCP server (RUNNER-PLAN-3) as a
// gateway catalog entry: launched from the signature's read-only server
// store inside the pinned runtime image, with its own egress allowlist
// and only its own secrets.
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
	allowAll := false
	for _, h := range srv.Egress {
		if h == "*" {
			allowAll = true
		}
	}
	switch {
	case allowAll:
		// Unrestricted: neither key — policy-gated at compile, disclosed
		// in enforcement.
	case len(srv.Egress) > 0:
		hosts := make([]string, 0, len(srv.Egress))
		for _, h := range sortedCopy(srv.Egress) {
			hosts = append(hosts, h+":443")
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
	e.set("description", strNode("SecureAgentry BriefAgent tools server (generated per run)"))
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
		// right default for a security tool plane.
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
		e["exec"] = "run_command restricted to argv0 allowlist + shim PATH {" + strings.Join(caps.ExecCommands, ", ") +
			"}; tools image is not binary-minimal in v1 (RUNNER-DEVIATIONS DR-1)"
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
			d += ")"
			parts = append(parts, d)
		}
		e["mcpServers"] = "declared servers installed per-signature from the operator's registries, launched from the release's read-only store, each with its own egress allowlist and server-scoped secrets: " + strings.Join(parts, "; ")
	}
	return e
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
