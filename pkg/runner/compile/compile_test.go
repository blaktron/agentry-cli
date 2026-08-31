// Compiler tests: the §R4 capability matrix, plan determinism (golden
// bytes, compile-twice hash equality), the policy gates, and the §R2
// instruction rules. The golden files are the framework's compatibility
// contract with itself — regenerate with `go test ./pkg/runner/compile
// -update` and review the diff like an API change, because it is one.
package compile

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
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

const pureTextYAML = `schemaVersion: "1"
name: pure-text
namespace: demo
kind: prompt-agent
version: 1.0.0
summary: "A pure text agent"
publish:
  include: [agent.yaml, AGENT.md]
`

const fullCapsYAML = `schemaVersion: "1"
name: full-caps
namespace: demo
kind: skill
version: 2.1.0
summary: "Exercises every capability shape"
entrypoints:
  skill: SKILL.md
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

// workspaceFor writes the prompt files a manifest kind needs.
func workspaceFor(t *testing.T, m *manifest.Manifest, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	switch m.Kind {
	case manifest.KindSkill:
		files["SKILL.md"] = "---\nname: x\n---\n\nDo the skill task well.\n"
	case manifest.KindPromptAgent:
		files["AGENT.md"] = "You are a pure text agent.\n"
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

func TestCompileGolden(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"puretext", pureTextYAML},
		{"fullcaps", fullCapsYAML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := parseManifest(t, tc.yaml)
			ws := workspaceFor(t, m, nil)
			_, planDir := compileFixture(t, m, ws)

			for _, rel := range []string{"plan.json", "gateway/catalog.yaml", "gateway/tools.yaml", "gateway/toolspec.json", "harness/agent.yaml"} {
				got, err := os.ReadFile(filepath.Join(planDir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatalf("read %s: %v", rel, err)
				}
				goldenPath := filepath.Join("testdata", "golden", tc.name+"."+strings.ReplaceAll(rel, "/", "-"))
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
		})
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
			wantTools:  []string{"list_inputs", "read_input", "write_report"},
			catalogHas: []string{"disableNetwork: true"},
			catalogNot: []string{"allowHosts", ":/workspace"},
		},
		{
			name: "read only workspace",
			caps: "capabilities:\n  filesystem:\n    read: workspace\n",
			wantTools: []string{
				"fs_read", "fs_list", "list_inputs", "read_input", "write_report",
			},
			catalogHas: []string{"@@RUN_WORKSPACE@@:/workspace:ro", "disableNetwork: true"},
			catalogNot: []string{"fs_write", ":/workspace:rw"},
		},
		{
			name: "egress hosts",
			caps: "capabilities:\n  network:\n    egress: [api.example.com]\n",
			wantTools: []string{
				"fetch", "list_inputs", "read_input", "write_report",
			},
			catalogHas: []string{"allowHosts", "api.example.com:443"},
			catalogNot: []string{"disableNetwork"},
		},
		{
			name: "exec commands",
			caps: "capabilities:\n  exec: { commands: [jq] }\n",
			wantTools: []string{
				"run_command", "list_inputs", "read_input", "write_report",
			},
			catalogHas: []string{"disableNetwork: true"},
		},
		{
			name: "write paths only",
			caps: "capabilities:\n  filesystem:\n    write: { paths: [out.md] }\n",
			wantTools: []string{
				"fs_write", "list_inputs", "read_input", "write_report",
			},
			catalogHas: []string{"@@RUN_WORKSPACE@@/out.md:/workspace/out.md:rw"},
			catalogNot: []string{"@@RUN_WORKSPACE@@:/workspace:ro"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "schemaVersion: \"1\"\nname: matrix\nnamespace: demo\nkind: prompt-agent\nversion: 1.0.0\nsummary: \"m\"\n" +
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
	yaml := "schemaVersion: \"1\"\nname: wild\nnamespace: demo\nkind: prompt-agent\nversion: 1.0.0\nsummary: \"w\"\n" +
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
	t.Run("skill strips frontmatter and appends contract", func(t *testing.T) {
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
		if !strings.Contains(s, "Runner contract (SecureAgentry)") {
			t.Error("runner contract postscript missing")
		}
	})
	t.Run("prompt-agent requires AGENT.md or PROMPT.md", func(t *testing.T) {
		m := parseManifest(t, pureTextYAML)
		ws := t.TempDir() // no prompt files at all
		_, err := Compile(Inputs{
			Manifest: m, Digest: "sha256:x", Source: "local", WorkspaceDir: ws,
			Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test",
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "AGENT.md") {
			t.Fatalf("want AGENT.md/PROMPT.md error, got %v", err)
		}
	})
	t.Run("mcp-server is not runnable", func(t *testing.T) {
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
		if err == nil || !strings.Contains(err.Error(), "not a runnable BriefAgent") {
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
