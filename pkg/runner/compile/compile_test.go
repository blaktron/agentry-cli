// Compiler tests: the §R4 capability matrix, plan determinism (golden
// bytes, compile-twice hash equality), the policy gates, and the §R2
// instruction rules. The golden files are the framework's compatibility
// contract with itself — regenerate with `go test ./pkg/runner/compile
// -update` and review the diff like an API change, because it is one.
package compile

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

var update = flag.Bool("update", false, "rewrite golden files")

// testPolicy is a fixed policy so plans are byte-reproducible.
func testPolicy() *policy.Policy {
	p := policy.Default()
	p.Images.Runner = "agentry-runner:test"
	// The fixtures declare jq and git; the catalog carries them
	// (agentry-cli#100: an uncatalogued command is a plan-time refusal).
	p.Exec.Packages = map[string]string{"jq": "jq", "git": "git"}
	return p
}

// parseManifest builds a manifest through the real YAML decoder so the
// capability unions are exercised, not hand-assembled.
func parseManifest(t *testing.T, yaml string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse test manifest: %v", err)
	}
	return m
}

const pureTextYAML = `schemaVersion: "2"
name: pure-text
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "A pure text agent"
publish:
  include: [agent.yaml, AGENT.md]
`

const fullCapsYAML = `schemaVersion: "2"
name: full-caps
namespace: demo
modes: [autonomous, interactive]
version: 2.1.0
summary: "Exercises every capability shape"
entrypoints:
  instruction: SKILL.md
capabilities:
  network:
    egress: [api.example.com, api.other.net]
  filesystem:
    read: workspace
    write: { paths: [reports, notes/summary.md] }
  exec: { commands: [jq, git] }
  secrets: [EXAMPLE_TOKEN]
  dataAccess: [tickets]
publish:
  include: [agent.yaml, SKILL.md]
`

// workspaceFor writes the instruction file a manifest names (its first
// candidate: entrypoints.instruction, AGENT.md by default).
func workspaceFor(t *testing.T, m *manifest.Manifest, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	if c := m.InstructionCandidates(); len(c) > 0 {
		if c[0] == "SKILL.md" {
			files["SKILL.md"] = "---\nname: x\n---\n\nDo the skill task well.\n"
		} else {
			files[c[0]] = "You are a pure text agent.\n"
		}
	}
	for k, v := range extra {
		files[k] = v
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func compileFixture(t *testing.T, m *manifest.Manifest, ws string) (*spec.Plan, string) {
	t.Helper()
	planDir := t.TempDir()
	plan, err := Compile(Inputs{
		Manifest:     m,
		Digest:       "sha256:0123456789abcdef",
		Source:       "local",
		WorkspaceDir: ws,
		Policy:       testPolicy(),
		PolicySHA:    "fixedpolicysha",
		RunnerImage:  "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return plan, planDir
}

// compileFixtureMode is compileFixture for a chosen run mode.
func compileFixtureMode(t *testing.T, m *manifest.Manifest, ws string, mode manifest.Mode) (*spec.Plan, string) {
	t.Helper()
	return compileFixtureModeWith(t, m, ws, mode, nil)
}

// compileFixtureModeWith is compileFixtureMode with the policy edited
// first, the way an operator's file shapes a plan.
func compileFixtureModeWith(t *testing.T, m *manifest.Manifest, ws string, mode manifest.Mode, edit func(*policy.Policy)) (*spec.Plan, string) {
	t.Helper()
	planDir := t.TempDir()
	pol := testPolicy()
	if edit != nil {
		edit(pol)
	}
	plan, err := Compile(Inputs{
		Manifest: m, Mode: mode, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
		Policy: pol, PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatalf("compile %s: %v", mode, err)
	}
	return plan, planDir
}

func TestCompileGolden(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		mode manifest.Mode
		pol  func(*policy.Policy)
	}{
		{"puretext", pureTextYAML, manifest.ModeAutonomous, nil},
		{"fullcaps", fullCapsYAML, manifest.ModeAutonomous, nil},
		{"fullcaps-interactive", fullCapsYAML, manifest.ModeInteractive, nil},
		// docker.runtime is in the plan and its hash, and the enforcement
		// map says what it covers (DR-23).
		{"fullcaps-runsc", fullCapsYAML, manifest.ModeAutonomous, func(p *policy.Policy) { p.Docker.Runtime = "runsc" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := parseManifest(t, tc.yaml)
			ws := workspaceFor(t, m, nil)
			_, planDir := compileFixtureModeWith(t, m, ws, tc.mode, tc.pol)

			checkGolden(t, tc.name, planDir, []string{"plan.json", "gateway/catalog.yaml", "gateway/tools.yaml", "gateway/toolspec.json", "harness/agent.yaml"})
		})
	}
}

// checkGolden compares the named plan files with testdata/golden/<name>.<file>
// and rewrites them under -update.
func checkGolden(t *testing.T, name, planDir string, rels []string) {
	t.Helper()
	for _, rel := range rels {
		got, err := os.ReadFile(filepath.Join(planDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		goldenPath := filepath.Join("testdata", "golden", name+"."+strings.ReplaceAll(rel, "/", "-"))
		if *update {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("missing golden %s (run with -update): %v", goldenPath, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from golden %s:\n--- got ---\n%s\n--- want ---\n%s", rel, goldenPath, got, want)
		}
	}
}

func TestCompileDeterministic(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	p1, _ := compileFixture(t, m, ws)
	p2, _ := compileFixture(t, m, ws)
	h1, err := p1.Hash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := p2.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("same inputs produced different plan hashes: %s vs %s", h1, h2)
	}
}

// TestCompileCapabilityMatrix pins the §R4 mapping: which tools exist,
// which mounts and network settings the catalog entry gets.
func TestCompileCapabilityMatrix(t *testing.T) {
	tests := []struct {
		name       string
		caps       string // capabilities block (or empty)
		wantTools  []string
		catalogHas []string
		catalogNot []string
	}{
		{
			name:       "pure text",
			caps:       "",
			wantTools:  []string{"list_inputs", "read_input", "search_inputs", "write_report"},
			catalogHas: []string{"disableNetwork: true"},
			catalogNot: []string{"allowHosts", ":/workspace"},
		},
		{
			name: "read only workspace",
			caps: "capabilities:\n  filesystem:\n    read: workspace\n",
			wantTools: []string{
				"fs_read", "fs_list", "list_inputs", "read_input", "search_inputs", "write_report",
			},
			catalogHas: []string{"@@RUN_WORKSPACE@@:/workspace:ro", "disableNetwork: true"},
			catalogNot: []string{"fs_write", ":/workspace:rw"},
		},
		{
			name: "egress hosts",
			caps: "capabilities:\n  network:\n    egress: [api.example.com]\n",
			wantTools: []string{
				"fetch", "list_inputs", "read_input", "search_inputs", "write_report",
			},
			catalogHas: []string{"allowHosts", "api.example.com:443"},
			catalogNot: []string{"disableNetwork"},
		},
		{
			name: "exec commands",
			caps: "capabilities:\n  exec: { commands: [jq] }\n",
			wantTools: []string{
				"run_command", "list_inputs", "read_input", "search_inputs", "write_report",
			},
			catalogHas: []string{"disableNetwork: true"},
		},
		{
			name: "write paths only",
			caps: "capabilities:\n  filesystem:\n    write: { paths: [out.md] }\n",
			wantTools: []string{
				"fs_write", "list_inputs", "read_input", "search_inputs", "write_report",
			},
			catalogHas: []string{"@@RUN_WORKSPACE@@/out.md:/workspace/out.md:rw"},
			catalogNot: []string{"@@RUN_WORKSPACE@@:/workspace:ro"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "schemaVersion: \"2\"\nname: matrix\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"m\"\n" +
				tc.caps + "publish:\n  include: [agent.yaml, AGENT.md]\n"
			m := parseManifest(t, yaml)
			ws := workspaceFor(t, m, nil)
			plan, planDir := compileFixture(t, m, ws)

			if got := strings.Join(plan.Tools, ","); got != strings.Join(tc.wantTools, ",") {
				t.Errorf("tools = %v, want %v", plan.Tools, tc.wantTools)
			}
			catalog, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.catalogHas {
				if !strings.Contains(string(catalog), want) {
					t.Errorf("catalog.yaml missing %q:\n%s", want, catalog)
				}
			}
			for _, not := range tc.catalogNot {
				if strings.Contains(string(catalog), not) {
					t.Errorf("catalog.yaml unexpectedly contains %q:\n%s", not, catalog)
				}
			}
		})
	}
}

// TestGatewayAlwaysBlocksNetwork pins §R11 invariant 1: without
// --block-network the gateway's allowHosts is advisory, so its absence is
// a security regression, not a style choice.
func TestGatewayAlwaysBlocksNetwork(t *testing.T) {
	for _, yaml := range []string{pureTextYAML, fullCapsYAML} {
		m := parseManifest(t, yaml)
		ws := workspaceFor(t, m, nil)
		plan, _ := compileFixture(t, m, ws)
		found := false
		for _, a := range plan.Gateway.Args {
			if a == "--block-network" {
				found = true
			}
		}
		if !found {
			t.Fatalf("gateway args lack --block-network: %v", plan.Gateway.Args)
		}
	}
}

func TestWildcardEgressPolicyGate(t *testing.T) {
	yaml := "schemaVersion: \"2\"\nname: wild\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"w\"\n" +
		"capabilities:\n  network:\n    egress: \"*\"\n" +
		"publish:\n  include: [agent.yaml, AGENT.md]\n"
	m := parseManifest(t, yaml)
	ws := workspaceFor(t, m, nil)

	_, err := Compile(Inputs{
		Manifest: m, Digest: "sha256:x", Source: "local", WorkspaceDir: ws,
		Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
	}, t.TempDir())
	var refusal *PolicyRefusal
	if err == nil || !errors.As(err, &refusal) {
		t.Fatalf("wildcard egress with default policy: got %v, want PolicyRefusal", err)
	}

	pol := testPolicy()
	pol.AllowWildcardEgress = true
	plan, err := Compile(Inputs{
		Manifest: m, Digest: "sha256:x", Source: "local", WorkspaceDir: ws,
		Policy: pol, PolicySHA: "s", RunnerImage: "agentry-runner:test",
	}, t.TempDir())
	if err != nil {
		t.Fatalf("wildcard egress with allowing policy: %v", err)
	}
	if plan.Capabilities.EgressMode != "all" {
		t.Fatalf("egress mode = %q, want all", plan.Capabilities.EgressMode)
	}
	if !strings.Contains(plan.Enforcement["network"], "UNRESTRICTED") {
		t.Fatalf("wildcard enforcement must disclose itself loudly, got %q", plan.Enforcement["network"])
	}
}

func TestInstructionRules(t *testing.T) {
	t.Run("skill strips frontmatter and follows the System Prompt", func(t *testing.T) {
		m := parseManifest(t, fullCapsYAML)
		ws := workspaceFor(t, m, nil)
		_, planDir := compileFixture(t, m, ws)
		agentYAML, err := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		s := string(agentYAML)
		if strings.Contains(s, "name: x") {
			t.Error("frontmatter leaked into the instruction")
		}
		if !strings.Contains(s, "Do the skill task well.") {
			t.Error("skill body missing from instruction")
		}
		if !strings.Contains(s, "# Agentry System Prompt") || strings.Index(s, "# Agentry System Prompt") > strings.Index(s, "Do the skill task well.") {
			t.Error("the System Prompt is missing, or does not come before the instruction")
		}
	})
	t.Run("v2 instruction defaults to AGENT.md and must exist", func(t *testing.T) {
		m := parseManifest(t, pureTextYAML)
		ws := t.TempDir() // no instruction file at all
		_, err := Compile(Inputs{
			Manifest: m, Digest: "sha256:x", Source: "local", WorkspaceDir: ws,
			Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "looked for AGENT.md") {
			t.Fatalf("want missing-AGENT.md error, got %v", err)
		}
	})
	t.Run("v1 prompt-agent falls back from AGENT.md to PROMPT.md", func(t *testing.T) {
		m := parseManifest(t, "schemaVersion: \"1\"\nname: old\nnamespace: demo\nkind: prompt-agent\nversion: 1.0.0\nsummary: \"v1\"\npublish:\n  include: [agent.yaml, PROMPT.md]\n")
		ws := workspaceFor(t, m, map[string]string{"PROMPT.md": "Prompt body.\n"})
		if err := os.Remove(filepath.Join(ws, "AGENT.md")); err != nil {
			t.Fatal(err)
		}
		plan, _ := compileFixture(t, m, ws)
		if plan.Instruction.SourcePath != "PROMPT.md" || !reflect.DeepEqual(plan.Modes, []string{"autonomous"}) {
			t.Fatalf("v1 plan = %s / modes %v", plan.Instruction.SourcePath, plan.Modes)
		}
	})
	t.Run("ReadInstruction gives the plan's instruction as the compile read it", func(t *testing.T) {
		m := parseManifest(t, fullCapsYAML)
		ws := workspaceFor(t, m, nil)
		plan, _ := compileFixture(t, m, ws)
		got, err := ReadInstruction(ws, plan.Instruction)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "name: x") || !strings.Contains(got, "Do the skill task well.") || strings.Contains(got, "System Prompt") {
			t.Errorf("ReadInstruction = %q, want the body without frontmatter or the System Prompt", got)
		}
		older := plan.Instruction
		older.BodySHA256 = ""
		if _, err := ReadInstruction(ws, older); err == nil || !strings.Contains(err.Error(), "compiled by an older CLI") {
			t.Errorf("a plan compiled before the System Prompt was read: %v", err)
		}
		for _, rel := range []string{"", "../outside.md", "/etc/passwd"} {
			outside := plan.Instruction
			outside.SourcePath = rel
			if _, err := ReadInstruction(ws, outside); err == nil {
				t.Errorf("ReadInstruction(%q) read a path outside the package", rel)
			}
		}
		if err := os.WriteFile(filepath.Join(ws, plan.Instruction.SourcePath), []byte("Do another task.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadInstruction(ws, plan.Instruction); err == nil || !strings.Contains(err.Error(), "changed after the plan was compiled") {
			t.Errorf("an instruction that is not the plan's was read: %v", err)
		}
	})
	t.Run("v1 mcp-server is not runnable", func(t *testing.T) {
		m := parseManifest(t, `schemaVersion: "1"
name: toolsrv
namespace: demo
kind: mcp-server
version: 1.0.0
summary: "a tool server"
entrypoints:
  mcp: { command: node, args: [server.js], transport: stdio }
publish:
  include: [agent.yaml, server.js]
`)
		_, err := Compile(Inputs{
			Manifest: m, Digest: "sha256:x", Source: "local", WorkspaceDir: t.TempDir(),
			Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
		}, t.TempDir())
		// The withdrawn kind declares no run mode, so the mode gate refuses
		// it before the instruction rule would.
		if err == nil || !strings.Contains(err.Error(), "not runnable") {
			t.Fatalf("want not-runnable error, got %v", err)
		}
	})
}

// TestOutputSchemaConvention pins §R2: output.schema.json flips the result
// kind and lands in the instruction.
func TestOutputSchemaConvention(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, map[string]string{
		OutputSchemaFile: `{"type":"object","required":["answer"]}`,
	})
	plan, planDir := compileFixture(t, m, ws)
	if plan.Harness.ResultKind != "json" {
		t.Fatalf("result kind = %q, want json", plan.Harness.ResultKind)
	}
	if plan.Output.SchemaFile != OutputSchemaFile || plan.Output.SchemaSHA256 == "" {
		t.Fatalf("output spec not recorded: %+v", plan.Output)
	}
	agentYAML, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml"))
	if !strings.Contains(string(agentYAML), `"required"`) {
		t.Error("schema not embedded in the instruction")
	}
}

// TestVerifyFilesDetectsTamper pins the §R6 integrity check.
func TestVerifyFilesDetectsTamper(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	plan, planDir := compileFixture(t, m, ws)
	if err := VerifyFiles(plan, planDir); err != nil {
		t.Fatalf("pristine plan failed verify: %v", err)
	}
	target := filepath.Join(planDir, "gateway", "catalog.yaml")
	if err := os.WriteFile(target, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFiles(plan, planDir); err == nil {
		t.Fatal("tampered plan passed verify")
	}
}

// TestNoSecretValuesInPlan pins §R11 invariant 3 at the compile layer: a
// secret VALUE present in the compiling process's environment must never
// reach plan bytes (only the NAME may).
func TestNoSecretValuesInPlan(t *testing.T) {
	t.Setenv("EXAMPLE_TOKEN", "super-secret-value-xyzzy")
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	_, planDir := compileFixture(t, m, ws)
	err := filepath.Walk(planDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, []byte("super-secret-value-xyzzy")) {
			t.Errorf("secret value leaked into %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- dataAccess → operator-cataloged servers (§R3) ----

const dataAccessYAML = `schemaVersion: "2"
name: data-access
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "Declares semantic data access"
capabilities:
  dataAccess: [email, tickets]
publish:
  include: [agent.yaml, AGENT.md]
`

// writeServerEntry writes one gateway server-entry YAML file — the
// {name: entry} shape policy.dataAccessServers points at.
func writeServerEntry(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".yaml")
	content := name + ":\n  description: operator-cataloged " + name + "\n  image: example/" + name + ":1.0.0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// compileWithPolicy is compileFixture with a caller-shaped policy.
func compileWithPolicy(t *testing.T, m *manifest.Manifest, ws string, pol *policy.Policy) (*spec.Plan, string, error) {
	t.Helper()
	planDir := t.TempDir()
	plan, err := Compile(Inputs{
		Manifest:     m,
		Digest:       "sha256:0123456789abcdef",
		Source:       "local",
		WorkspaceDir: ws,
		Policy:       pol,
		PolicySHA:    "fixedpolicysha",
		RunnerImage:  "agentry-runner:test",
	}, planDir)
	return plan, planDir, err
}

func TestDataAccessServersAttached(t *testing.T) {
	m := parseManifest(t, dataAccessYAML)
	ws := workspaceFor(t, m, nil)
	pol := testPolicy()
	pol.DataAccessServers = map[string][]string{
		"tickets": {writeServerEntry(t, "jira-mcp")},
		"email":   {writeServerEntry(t, "mail-mcp")},
	}
	plan, planDir, err := compileWithPolicy(t, m, ws, pol)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// The plan's enabled-server list: companion server + cataloged ones,
	// sorted.
	wantServers := []string{"agentry-tools", "jira-mcp", "mail-mcp"}
	if got := strings.Join(plan.Gateway.Servers, ","); got != strings.Join(wantServers, ",") {
		t.Fatalf("servers = %q, want %q", got, wantServers)
	}
	// --servers argv must carry the same list.
	joined := strings.Join(plan.Gateway.Args, " ")
	if !strings.Contains(joined, "--servers agentry-tools,jira-mcp,mail-mcp") {
		t.Errorf("gateway argv lacks the merged server list: %q", joined)
	}

	// catalog.yaml carries the operator entries verbatim next to the
	// companion server; registry.yaml enables all three.
	catalog, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"jira-mcp:", "image: example/jira-mcp:1.0.0", "mail-mcp:", "agentry-tools:"} {
		if !strings.Contains(string(catalog), want) {
			t.Errorf("catalog.yaml missing %q:\n%s", want, catalog)
		}
	}
	registry, err := os.ReadFile(filepath.Join(planDir, "gateway", "registry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"jira-mcp:", "mail-mcp:", "agentry-tools:"} {
		if !strings.Contains(string(registry), want) {
			t.Errorf("registry.yaml missing %q:\n%s", want, registry)
		}
	}

	// The §R8 disclosure names the attached servers (plan-level check).
	if got := plan.Enforcement["dataAccess"]; !strings.Contains(got, "declared email, tickets") ||
		!strings.Contains(got, "operator-cataloged servers attached: jira-mcp, mail-mcp") {
		t.Errorf("enforcement disclosure = %q", got)
	}
}

func TestDataAccessServersDedupe(t *testing.T) {
	m := parseManifest(t, dataAccessYAML)
	ws := workspaceFor(t, m, nil)
	pol := testPolicy()
	shared := writeServerEntry(t, "shared-mcp")
	// Both declared categories catalog the SAME server file: it must be
	// attached once, not twice.
	pol.DataAccessServers = map[string][]string{
		"tickets": {shared},
		"email":   {shared},
	}
	plan, _, err := compileWithPolicy(t, m, ws, pol)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	count := 0
	for _, s := range plan.Gateway.Servers {
		if s == "shared-mcp" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared server attached %d times in %v", count, plan.Gateway.Servers)
	}
}

func TestDataAccessRefusals(t *testing.T) {
	m := parseManifest(t, dataAccessYAML)
	ws := workspaceFor(t, m, nil)

	t.Run("reserved companion name", func(t *testing.T) {
		pol := testPolicy()
		path := filepath.Join(t.TempDir(), "evil.yaml")
		if err := os.WriteFile(path, []byte("agentry-tools:\n  image: evil/impostor:1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pol.DataAccessServers = map[string][]string{"tickets": {path}}
		_, _, err := compileWithPolicy(t, m, ws, pol)
		var pr *PolicyRefusal
		if !errors.As(err, &pr) || !strings.Contains(err.Error(), "reserved name") {
			t.Fatalf("err = %v, want a reserved-name PolicyRefusal", err)
		}
	})
	t.Run("missing server file", func(t *testing.T) {
		pol := testPolicy()
		pol.DataAccessServers = map[string][]string{"tickets": {filepath.Join(t.TempDir(), "absent.yaml")}}
		_, _, err := compileWithPolicy(t, m, ws, pol)
		var pr *PolicyRefusal
		if !errors.As(err, &pr) || !strings.Contains(err.Error(), "server file") {
			t.Fatalf("err = %v, want a PolicyRefusal naming the file", err)
		}
	})
	t.Run("multi-entry file shape", func(t *testing.T) {
		pol := testPolicy()
		path := filepath.Join(t.TempDir(), "two.yaml")
		if err := os.WriteFile(path, []byte("a:\n  image: x/a:1\nb:\n  image: x/b:1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pol.DataAccessServers = map[string][]string{"tickets": {path}}
		_, _, err := compileWithPolicy(t, m, ws, pol)
		var pr *PolicyRefusal
		if !errors.As(err, &pr) || !strings.Contains(err.Error(), "single {name: entry} mapping") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unparseable yaml", func(t *testing.T) {
		pol := testPolicy()
		path := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(path, []byte(":\tnot yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
		pol.DataAccessServers = map[string][]string{"tickets": {path}}
		_, _, err := compileWithPolicy(t, m, ws, pol)
		var pr *PolicyRefusal
		if !errors.As(err, &pr) {
			t.Fatalf("err = %v, want a PolicyRefusal", err)
		}
	})
}

// TestDataAccessUncatalogedIsAuditOnly pins the honest-posture fallback: a
// declared category with no operator catalog attaches nothing and says so.
func TestDataAccessUncatalogedIsAuditOnly(t *testing.T) {
	m := parseManifest(t, dataAccessYAML)
	ws := workspaceFor(t, m, nil)
	pol := testPolicy()
	pol.DataAccessServers = map[string][]string{"payments": {writeServerEntry(t, "pay-mcp")}} // different category
	plan, _, err := compileWithPolicy(t, m, ws, pol)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Gateway.Servers) != 1 || plan.Gateway.Servers[0] != "agentry-tools" {
		t.Fatalf("servers = %v, want only the companion server", plan.Gateway.Servers)
	}
	if got := plan.Enforcement["dataAccess"]; !strings.Contains(got, "no servers cataloged by policy") {
		t.Errorf("enforcement disclosure = %q", got)
	}
}

func TestPolicyRefusalError(t *testing.T) {
	err := &PolicyRefusal{Reason: "wildcard egress"}
	if got := err.Error(); got != "refused by policy: wildcard egress" {
		t.Errorf("Error() = %q", got)
	}
}

// A package whose output.schema.json is JSON but not a JSON Schema is
// refused at compile time (DR-8 closed, agentry-cli#11): the engine will
// validate the final message against it, so it must compile.
func TestOutputSchemaMustBeAValidSchema(t *testing.T) {
	if _, err := CompileOutputSchema([]byte(`{"type":"object","required":["a"]}`)); err != nil {
		t.Fatalf("valid schema refused: %v", err)
	}
	if _, err := CompileOutputSchema([]byte(`{"type":"nonsense"}`)); err == nil {
		t.Fatal("invalid schema type accepted")
	}
	if _, err := CompileOutputSchema([]byte(`{"properties":"not-an-object"}`)); err == nil {
		t.Fatal("malformed properties accepted")
	}
}

// TestGatewayConfigInsideCatalogsDir pins agentry-cli#41: the gateway
// (v0.43+) refuses a --catalog outside its own catalogs directory, so the
// rendered files must be mounted there and every file flag must point
// inside that mount.
func TestGatewayConfigInsideCatalogsDir(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	plan, _ := compileFixture(t, m, ws)
	var mounted bool
	for _, mt := range plan.Gateway.Mounts {
		if mt.Target == GatewayConfigDir && mt.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("gateway config is not mounted read-only at %s: %+v", GatewayConfigDir, plan.Gateway.Mounts)
	}
	args := plan.Gateway.Args
	for i, a := range args {
		switch a {
		case "--catalog", "--registry", "--config", "--tools-config", "--secrets":
			if i+1 >= len(args) || !strings.HasPrefix(args[i+1], GatewayConfigDir+"/") {
				t.Errorf("%s points outside the catalogs mount: %v", a, args)
			}
		}
	}
	var home bool
	for _, e := range plan.Gateway.Env {
		if e.Name == "HOME" && e.Value == "/root" {
			home = true
		}
	}
	if !home {
		t.Errorf("gateway env does not pin HOME=/root: %+v", plan.Gateway.Env)
	}
}

// TestModePlansIdenticalExceptSession pins plan §6.10 line 1: for one
// release and policy, the interactive plan's gateway file set, egress
// allowlist, mounts, secrets and instruction are byte-identical to the
// autonomous plan's; only the harness argv/stdin and the Session block
// differ.
func TestModePlansIdenticalExceptSession(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	auto, autoDir := compileFixtureMode(t, m, ws, manifest.ModeAutonomous)
	inter, interDir := compileFixtureMode(t, m, ws, manifest.ModeInteractive)
	if auto.Mode != "autonomous" || inter.Mode != "interactive" || !reflect.DeepEqual(auto.Modes, inter.Modes) {
		t.Fatalf("modes: %s/%v vs %s/%v", auto.Mode, auto.Modes, inter.Mode, inter.Modes)
	}
	if auto.Harness.Session != nil || inter.Harness.Session == nil {
		t.Fatalf("session blocks: auto=%v inter=%v", auto.Harness.Session, inter.Harness.Session)
	}
	if reflect.DeepEqual(auto.Harness.Agent.Argv, inter.Harness.Agent.Argv) {
		t.Fatal("argv must differ between modes")
	}
	for _, rel := range []string{"gateway/catalog.yaml", "gateway/registry.yaml", "gateway/config.yaml", "gateway/tools.yaml", "gateway/toolspec.json", "harness/agent.yaml"} {
		a, _ := os.ReadFile(filepath.Join(autoDir, filepath.FromSlash(rel)))
		b, _ := os.ReadFile(filepath.Join(interDir, filepath.FromSlash(rel)))
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs between modes", rel)
		}
	}
	// Normalize the permitted differences and compare the whole plan.
	na, nb := *auto, *inter
	na.Mode, nb.Mode = "", ""
	na.Harness.Agent.Argv, nb.Harness.Agent.Argv = nil, nil
	na.Harness.Agent.StdinTask, nb.Harness.Agent.StdinTask = false, false
	na.Harness.Session, nb.Harness.Session = nil, nil
	na.Files, nb.Files = nil, nil // hashes of identical files, checked above
	if !reflect.DeepEqual(na, nb) {
		ja, _ := json.MarshalIndent(na, "", " ")
		jb, _ := json.MarshalIndent(nb, "", " ")
		t.Errorf("plans differ beyond argv/session:\n%s\n---\n%s", ja, jb)
	}
}

// TestCompileRefusesUndeclaredMode: a mode the manifest does not declare is
// a compile refusal (exit 12: not runnable this way).
func TestCompileRefusesUndeclaredMode(t *testing.T) {
	m := parseManifest(t, pureTextYAML) // autonomous only
	ws := workspaceFor(t, m, nil)
	_, err := Compile(Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:x", Source: "local", WorkspaceDir: ws,
		Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not declare run mode interactive") {
		t.Fatalf("want refusal, got %v", err)
	}
	if _, err := Compile(Inputs{Manifest: m, Mode: "daemon", WorkspaceDir: ws, Policy: testPolicy()}, t.TempDir()); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

// TestSessionBudgetsFollowPolicyAndManifestHint: the plan's session caps
// are the operator's, and a manifest may only shorten the idle timeout.
func TestSessionBudgetsFollowPolicyAndManifestHint(t *testing.T) {
	pol := testPolicy()
	pol.Sessions = policy.Sessions{IdleTimeoutSeconds: 600, MaxWallSeconds: 3600, MaxUserTurns: 50}
	base := "schemaVersion: \"2\"\nname: hinted\nnamespace: demo\nmodes: [autonomous, interactive]\nversion: 1.0.0\nsummary: \"h\"\n"
	for _, tc := range []struct {
		name     string
		hint     string
		wantIdle int
	}{
		{"no hint", "", 600},
		{"shorter hint wins", "interactive:\n  idleTimeoutSeconds: 120\n", 120},
		{"longer hint is capped", "interactive:\n  idleTimeoutSeconds: 9000\n", 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseManifest(t, base+tc.hint+"publish:\n  include: [agent.yaml, AGENT.md]\n")
			got := sessionBudgets(m, pol)
			if got.IdleTimeoutSeconds != tc.wantIdle || got.MaxWallSeconds != 3600 || got.MaxUserTurns != 50 {
				t.Errorf("budgets = %+v", got)
			}
		})
	}
}

// TestCompileExecCatalog (agentry-cli#100): a declared command the
// operator's exec catalog does not carry is a plan-time policy refusal
// naming the command and the key; a catalogued one compiles into an exec
// image whose plan entry names the packages, whose tag moves with the
// catalog entry or the mirror, and whose enforcement line names the
// packages and, for git, the safe.directory setting.
func TestCompileExecCatalog(t *testing.T) {
	m := parseManifest(t, "schemaVersion: \"2\"\nname: gitter\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"g\"\n"+
		"capabilities:\n  exec: { commands: [git, sort] }\npublish:\n  include: [agent.yaml, AGENT.md]\n")
	ws := workspaceFor(t, m, nil)
	compileWith := func(pol *policy.Policy) (*spec.Plan, error) {
		return Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
			Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, t.TempDir())
	}

	bare := testPolicy()
	bare.Exec.Packages = nil
	_, err := compileWith(bare)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "git") || !strings.Contains(err.Error(), "exec.packages") {
		t.Fatalf("uncatalogued command not refused as policy naming the command and the key: %v", err)
	}

	pol := testPolicy()
	pol.Exec.Packages = map[string]string{"git": "git", "sort": "busybox", "jq": "jq"}
	plan, err := compileWith(pol)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExecImage == nil || plan.ExecImage.Tag == "" {
		t.Fatalf("plan lacks the exec image: %+v", plan.ExecImage)
	}
	if got := strings.Join(plan.ExecImage.Packages, ","); got != "busybox,git" {
		t.Fatalf("exec image packages = %q, want the declared commands' packages only, sorted", got)
	}
	if got := strings.Join(plan.ExecImage.Commands, ","); got != "git,sort" {
		t.Fatalf("exec image commands = %q", got)
	}
	enf := plan.Enforcement["exec"]
	for _, want := range []string{"git ← apk git", "sort ← apk busybox", "safe.directory"} {
		if !strings.Contains(enf, want) {
			t.Fatalf("enforcement lacks %q: %q", want, enf)
		}
	}
	if strings.Contains(enf, "jq") {
		t.Fatalf("enforcement names a package for an undeclared command: %q", enf)
	}

	pinned := testPolicy()
	pinned.Exec.Packages = map[string]string{"git": "git=2.49.1-r0", "sort": "busybox"}
	p2, err := compileWith(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if p2.ExecImage.Tag == plan.ExecImage.Tag {
		t.Fatal("a changed catalog entry must change the exec image tag")
	}
	mirrored := testPolicy()
	mirrored.Exec.Packages = pol.Exec.Packages
	mirrored.Exec.APKMirror = "https://mirror.example/alpine"
	p3, err := compileWith(mirrored)
	if err != nil {
		t.Fatal(err)
	}
	if p3.ExecImage.Tag == plan.ExecImage.Tag || p3.ExecImage.APKMirror != mirrored.Exec.APKMirror {
		t.Fatalf("a changed mirror must change the tag and be recorded: %+v", p3.ExecImage)
	}
	if plan.Enforcement["exec"] == "" || strings.Contains(plan.Enforcement["exec"], "2.49") {
		t.Fatal("the compiled line must not claim a resolved version the build has not produced")
	}
}

// Two plans that differ only in docker.runtime differ in their hash: an
// operator who changes the runtime recompiles rather than reinterpreting a
// stored plan; a plan compiled without the knob carries no docker block.
func TestRuntimeIsPartOfThePlanHash(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	plain, _ := compileFixtureMode(t, m, ws, manifest.ModeAutonomous)
	runsc, _ := compileFixtureModeWith(t, m, ws, manifest.ModeAutonomous, func(p *policy.Policy) { p.Docker.Runtime = "runsc" })
	h1, _ := plain.Hash()
	h2, _ := runsc.Hash()
	if h1 == h2 {
		t.Fatal("a plan with docker.runtime hashes like one without")
	}
	if plain.Docker != nil || plain.Enforcement["runtime"] != "" {
		t.Fatal("a plan compiled without docker.runtime carries a docker block or a runtime line")
	}
	if runsc.Runtime() != "runsc" || !strings.HasPrefix(runsc.Enforcement["runtime"], "runsc:") {
		t.Fatalf("runtime plan: docker=%+v enforcement=%q", runsc.Docker, runsc.Enforcement["runtime"])
	}
}

// Under a runtime, the host's name servers and the gateway's runtime are
// part of any plan, not only a draft's (agentry-cli#337): the classes that
// resolve host names read them. Without a runtime a plain run carries
// neither, so its bytes are what they were.
func TestTheSandboxSettingsArePartOfThePlan(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	both, _ := compileFixtureModeWith(t, m, ws, manifest.ModeAutonomous, func(p *policy.Policy) {
		p.Docker.Runtime, p.Docker.GatewayRuntime, p.Docker.Resolvers = "runsc", "runsc-uds", []string{"192.0.2.53"}
	})
	if both.GatewayRuntime() != "runsc-uds" || !slices.Equal(both.SandboxResolvers(), []string{"192.0.2.53"}) || both.WebResolvers() != nil {
		t.Fatalf("docker block %+v: want the gateway runtime and the resolvers for the sandbox, none for web tools it lacks", both.Docker)
	}
	bare, _ := compileFixtureModeWith(t, m, ws, manifest.ModeAutonomous, func(p *policy.Policy) { p.Docker.Resolvers = []string{"192.0.2.53"} })
	if bare.Docker != nil {
		t.Fatalf("resolvers without a runtime or the web tools reached the plan: %+v", bare.Docker)
	}
	h1, _ := both.Hash()
	runsc, _ := compileFixtureModeWith(t, m, ws, manifest.ModeAutonomous, func(p *policy.Policy) { p.Docker.Runtime = "runsc" })
	if h2, _ := runsc.Hash(); h1 == h2 {
		t.Fatal("the gateway runtime and resolvers leave the plan's hash unchanged")
	}
}

// TestModelKeySource (agentry-cli#148): the receipt's model.apiKey line
// names the variable, and the keychain account only when the policy says
// keyring — the harness's source over the model's.
func TestModelKeySource(t *testing.T) {
	pol := policy.Default()
	if env, acct := modelKeySource(pol, "docker-agent"); env != "ANTHROPIC_API_KEY" || acct != "" {
		t.Fatalf("default: %q %q", env, acct)
	}
	pol.Model.APIKeySource = "keyring"
	if env, acct := modelKeySource(pol, "docker-agent"); env != "ANTHROPIC_API_KEY" || acct != "provider:anthropic" {
		t.Fatalf("keyring: %q %q", env, acct)
	}
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {APIKeyEnv: "CLAUDE_KEY", APIKeySource: "env"}}
	if env, acct := modelKeySource(pol, "claude"); env != "CLAUDE_KEY" || acct != "" {
		t.Fatalf("harness env overrides: %q %q", env, acct)
	}
}

// TestCompileRefusesAPathOutsideTheWorkspace: a filesystem path in the
// manifest becomes a bind-mount specification by concatenation, so the
// compiler judges every path itself — whatever the manifest layer promised
// — and refuses one that would leave the run workspace or break the volume
// string (security review 2026-09-20 §4.2, C3).
func TestCompileRefusesAPathOutsideTheWorkspace(t *testing.T) {
	base := "schemaVersion: \"2\"\nname: paths\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"p\"\nentrypoints:\n  instruction: SKILL.md\npublish:\n  include: [agent.yaml, SKILL.md]\n"
	refused := map[string]string{
		"parent traversal":            "capabilities:\n  filesystem:\n    write: { paths: [\"../../../../home/victim/.ssh\"] }\n",
		"nested traversal":            "capabilities:\n  filesystem:\n    read: { paths: [\"docs/../../etc\"] }\n",
		"absolute":                    "capabilities:\n  filesystem:\n    write: { paths: [\"/etc\"] }\n",
		"colon":                       "capabilities:\n  filesystem:\n    read: { paths: [\"a:b\"] }\n",
		"dot":                         "capabilities:\n  filesystem:\n    write: { paths: [\".\"] }\n",
		"empty":                       "capabilities:\n  filesystem:\n    write: { paths: [\"\"] }\n",
		"unclean":                     "capabilities:\n  filesystem:\n    read: { paths: [\"a//b\"] }\n",
		"whole read, write traversal": "capabilities:\n  filesystem:\n    read: workspace\n    write: { paths: [\"../out\"] }\n",
	}
	for name, caps := range refused {
		t.Run(name, func(t *testing.T) {
			m := parseManifest(t, base+caps)
			ws := workspaceFor(t, m, nil)
			_, err := Compile(Inputs{
				Manifest: m, Digest: "sha256:x", Source: "registry", WorkspaceDir: ws,
				Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
			}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "filesystem path") {
				t.Fatalf("want a filesystem path refusal, got %v", err)
			}
		})
	}
	// The shapes a manifest may use compile to binds under the workspace.
	m := parseManifest(t, base+"capabilities:\n  filesystem:\n    read: { paths: [docs, \"data/in.csv\"] }\n    write: { paths: [reports, \"notes/summary.md\"] }\n")
	compileFixture(t, m, workspaceFor(t, m, nil))
	caps, err := summarizeCapabilities(m, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range workspaceVolumes(caps) {
		if !strings.HasPrefix(v, spec.PHWorkspace+"/") || strings.Contains(v, "..") {
			t.Fatalf("bind outside the workspace: %q", v)
		}
	}
}

// TestCompileIgnoresTheGuide: the manifest's guide (DEV-141,
// agentry-cli#257) is text for people before a run. It is not part of the
// plan, so a release that differs only in its guide compiles to the same
// plan hash, and the plan's bytes never carry the guide's words.
func TestCompileIgnoresTheGuide(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	plain, _ := compileFixture(t, m, ws)

	guided := *m
	guided.Guide = &manifest.Guide{
		Overview: "GUIDE-OVERVIEW-MARKER",
		Prepare:  []string{"GUIDE-PREPARE-MARKER"},
		Files:    "GUIDE-FILES-MARKER",
		Task:     "GUIDE-TASK-MARKER",
		Connect:  map[string]string{},
	}
	for _, name := range m.Capabilities.Secrets {
		guided.Guide.Connect[name] = "GUIDE-CONNECT-MARKER\nsecond line"
	}
	if len(guided.Guide.Connect) == 0 {
		t.Fatal("the fixture declares no secret to give connect help for")
	}
	withGuide, planDir := compileFixture(t, &guided, ws)

	h1, err := plain.Hash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := withGuide.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("a guide changed the plan hash: %s vs %s", h1, h2)
	}
	err = filepath.Walk(planDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), "GUIDE-") {
			t.Errorf("%s carries the guide's words", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Plain-http targets are the http model base URLs the CONNECT allowlist
// already names, nothing else (DR-45, agentry-cli#377).
func TestPlainHTTPTargetsAreHTTPEndpointsOnTheAllowlist(t *testing.T) {
	pol := policy.Default()
	pol.Model.BaseURL = "http://gpu-box.lan:8000/v1"
	pol.Model.ProxyAllow = []string{"gpu-box.lan:8000", "api.anthropic.com:443", "other.lan:80"}
	pol.Harness = map[string]policy.HarnessPolicy{
		"codex":  {BaseURL: "http://other.lan/v1"},       // on the allowlist as :80
		"claude": {BaseURL: "https://api.anthropic.com"}, // https: never plain
		"x":      {BaseURL: "http://not-allowed.lan:9/"}, // not on the allowlist
	}
	got := strings.Join(plainHTTPTargets(pol), ",")
	if got != "gpu-box.lan:8000,other.lan:80" {
		t.Fatalf("plain targets = %q", got)
	}
	pol.Model.BaseURL = "https://llm.internal:8443/v1"
	pol.Harness = nil
	if got := plainHTTPTargets(pol); len(got) != 0 {
		t.Fatalf("an all-https policy has plain targets: %v", got)
	}
}
