package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// renderedInstruction reads the text a harness is given out of a compiled
// plan directory, whichever adapter wrote it.
func renderedInstruction(t *testing.T, planDir, adapterName string) string {
	t.Helper()
	if adapterName == "claude" || adapterName == "codex" {
		b, err := os.ReadFile(filepath.Join(planDir, "harness", "instruction.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	b, err := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Agents map[string]struct {
			Instruction string `yaml:"instruction"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Agents["root"].Instruction
}

// The capability shapes the System Prompt is composed over: no grant at
// all, named hosts with a secret, every shape at once, the wildcard, and
// an output schema.
var promptCases = []struct {
	name   string
	yaml   string
	mode   manifest.Mode
	pol    func(*policy.Policy)
	schema bool
}{
	{name: "puretext", yaml: pureTextYAML, mode: manifest.ModeAutonomous},
	{name: "fullcaps", yaml: fullCapsYAML, mode: manifest.ModeAutonomous},
	{name: "fullcaps-interactive", yaml: fullCapsYAML, mode: manifest.ModeInteractive},
	{name: "wildcard", yaml: strings.Replace(fullCapsYAML, "egress: [api.example.com, api.other.net]", `egress: "*"`, 1),
		mode: manifest.ModeAutonomous, pol: func(p *policy.Policy) { p.AllowWildcardEgress = true }},
	{name: "puretext-schema", yaml: pureTextYAML, mode: manifest.ModeAutonomous, schema: true},
}

func compilePromptCase(t *testing.T, i int, adapterName string) (string, string) {
	t.Helper()
	tc := promptCases[i]
	m := parseManifest(t, tc.yaml)
	extra := map[string]string(nil)
	if tc.schema {
		extra = map[string]string{OutputSchemaFile: `{"type":"object","properties":{"ok":{"type":"boolean"}}}`}
	}
	ws := workspaceFor(t, m, extra)
	pol := testPolicy()
	if tc.pol != nil {
		tc.pol(pol)
	}
	planDir := t.TempDir()
	plan, err := Compile(Inputs{
		Manifest: m, Mode: tc.mode, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
		Policy: pol, PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test", AdapterName: adapterName,
	}, planDir)
	if err != nil {
		t.Fatalf("compile %s on %s: %v", tc.name, adapterName, err)
	}
	return renderedInstruction(t, planDir, plan.Harness.Adapter), strings.Join(plan.Tools, " ")
}

// TestTheSystemPromptHeadComesFirst: the head is the first text every
// harness is given, the same bytes for every plan, before anything the
// plan composes and before the package's instruction (agentry-cli#520).
func TestTheSystemPromptHeadComesFirst(t *testing.T) {
	for _, adapterName := range []string{"docker-agent", "claude", "codex"} {
		for i, tc := range promptCases {
			if tc.mode == manifest.ModeInteractive && adapterName != "docker-agent" {
				continue // a host adapter's session is its own suite's
			}
			got, _ := compilePromptCase(t, i, adapterName)
			if !strings.HasPrefix(got, systemPromptHead) {
				t.Errorf("%s/%s: the System Prompt's head is not the first text:\n%.300s", adapterName, tc.name, got)
			}
			head := strings.Index(got, systemPromptHead)
			instr := strings.Index(got, instructionHeading)
			if instr < 0 || instr < head {
				t.Errorf("%s/%s: the package's instruction does not follow the System Prompt", adapterName, tc.name)
			}
			if strings.Count(got, "# Agentry System Prompt") != 1 {
				t.Errorf("%s/%s: the System Prompt appears more than once", adapterName, tc.name)
			}
		}
	}
}

// TestTheSystemPromptStatesNoGrantTheRunLacks: the body is composed from
// the compiled plan, so a tool it names is one the plan grants (the
// knowledge tools aside: they are named "if attached", because a pack is
// a run-time fact), and its network sentence is the plan's (D11).
func TestTheSystemPromptStatesNoGrantTheRunLacks(t *testing.T) {
	all := []string{toolsrv.ToolFSRead, toolsrv.ToolFSList, toolsrv.ToolFSWrite, toolsrv.ToolFetch,
		toolsrv.ToolRunCommand, toolsrv.ToolListInputs, toolsrv.ToolReadInput, toolsrv.ToolWriteReport,
		toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolRegistrySearch}
	for i, tc := range promptCases {
		text, granted := compilePromptCase(t, i, "docker-agent")
		prompt := text[:strings.Index(text, instructionHeading)]
		for _, tool := range all {
			if strings.Contains(prompt, tool) && !strings.Contains(" "+granted+" ", " "+tool+" ") {
				t.Errorf("%s: the System Prompt names %s, which the plan does not grant (%s)", tc.name, tool, granted)
			}
		}
		switch tc.name {
		case "puretext", "puretext-schema":
			for _, want := range []string{"**Network.** None.", "**Commands.** None", "no workspace file access", "**Credentials.** None"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("%s: want %q in the System Prompt", tc.name, want)
				}
			}
		case "fullcaps", "fullcaps-interactive":
			for _, want := range []string{"api.example.com, api.other.net", "git, jq", "EXAMPLE_TOKEN", "notes/summary.md, reports"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("%s: want %q in the System Prompt", tc.name, want)
				}
			}
			if strings.Contains(prompt, "**Network.** None") {
				t.Errorf("%s: a plan with egress is told it has no network", tc.name)
			}
		case "wildcard":
			if !strings.Contains(prompt, "any public host") {
				t.Errorf("%s: the wildcard's network sentence is missing", tc.name)
			}
		}
		if tc.schema != strings.Contains(prompt, "returns structured output") {
			t.Errorf("%s: the structured-output line does not follow the plan's schema (%v)", tc.name, tc.schema)
		}

	}
}

// TestTheWebToolsArePromptedOnlyWithTheGrant: a run with the built-ins'
// web grant is told how AI Review reads what the tools bring back; no
// other run hears of them.
func TestTheWebToolsArePromptedOnlyWithTheGrant(t *testing.T) {
	with := systemPrompt(promptFacts{caps: capsNone(true)})
	without := systemPrompt(promptFacts{caps: capsNone(false)})
	for _, tool := range []string{toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolRegistrySearch} {
		if !strings.Contains(with, tool) {
			t.Errorf("a web-granted run is not told about %s", tool)
		}
		if strings.Contains(without, tool) {
			t.Errorf("a run without the web grant is told about %s", tool)
		}
	}
	if !strings.Contains(with, "AI Review reads every search result") {
		t.Error("a web-granted run is not told that AI Review reads the results first")
	}
}

// TestTheSystemPromptStaysWithinItsSize: up to ~12 KB (D12), for the
// largest composition.
func TestTheSystemPromptStaysWithinItsSize(t *testing.T) {
	f := promptFacts{
		caps:    capsNone(true),
		secrets: []string{"A_TOKEN", "B_TOKEN", "C_TOKEN"},
		servers: []string{"github", "tickets", "slack"},
		schema:  true,
	}
	f.caps.EgressMode, f.caps.EgressHosts = "hosts", []string{"api.example.com", "api.other.net"}
	f.caps.FSReadMode, f.caps.FSWriteMode = "workspace", "workspace"
	f.caps.ExecCommands = []string{"git", "jq", "python3"}
	if n := len(systemPrompt(f)); n > 12<<10 {
		t.Errorf("the System Prompt is %d bytes, past its 12 KiB", n)
	}
}

func capsNone(web bool) spec.CapSummary {
	return spec.CapSummary{EgressMode: "none", FSReadMode: "none", FSWriteMode: "none", WebTools: web}
}
