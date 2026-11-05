package compile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// compileDesign compiles yaml from source with the design grant, and the
// review grant beside it when review, in mode.
func compileDesign(t *testing.T, yaml, source string, mode manifest.Mode, review bool) error {
	t.Helper()
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Design.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Compile(Inputs{Manifest: m, Mode: mode, Digest: "sha256:x", Source: source, Design: true, Review: review, WorkspaceDir: ws,
		Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, filepath.Join(t.TempDir(), "plan"))
	return err
}

// TestOnlyTheReportDesignerIsGrantedPreviews: render_preview, check_design
// and view_reference compile for a built-in run alone. A release, a local
// package, a persistent agent, a built-in beside the review grant and one
// whose contract would widen what runs beside them are refused by policy;
// the built-in's plan grants the three tools and says so.
func TestOnlyTheReportDesignerIsGrantedPreviews(t *testing.T) {
	refused := func(name string, err error, want string) {
		t.Helper()
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, want)
		}
	}
	for _, source := range []string{"registry", "local", ""} {
		refused("source "+source, compileDesign(t, webTextAgent, source, manifest.ModeAutonomous, false), "the Report Designer's alone")
	}
	persistent := persistentInputs(t, nil)
	persistent.Design = true
	_, err := Compile(persistent, filepath.Join(t.TempDir(), "plan"))
	refused("a persistent agent", err, "the Report Designer's alone")
	refused("beside the review grant", compileDesign(t, webTextAgent, "builtin", manifest.ModeAutonomous, true), "")
	for name, widen := range map[string]string{
		"egress":  "  network:\n    egress: [api.example.com]\n",
		"exec":    "  exec:\n    commands: [git]\n",
		"write":   "  filesystem:\n    write: workspace\n",
		"secrets": "  secrets: [API_KEY]\n",
		"servers": "  mcpServers:\n    - npm: \"@acme/mcp\"\n      version: 1.0.0\n",
	} {
		y := strings.Replace(webTextAgent, "capabilities:\n  network:\n    egress: none\n  exec: none\n", "capabilities:\n"+widen, 1)
		refused(name, compileDesign(t, y, "builtin", manifest.ModeAutonomous, false), "declares no egress")
	}

	m, err := manifest.Parse([]byte(webTextAgent))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Design.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, design := range []bool{true, false} {
		plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: "builtin", Design: design, WorkspaceDir: ws,
			Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, filepath.Join(t.TempDir(), "plan"))
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{toolsrv.ToolRenderPreview, toolsrv.ToolCheckDesign, toolsrv.ToolViewReference} {
			if slices.Contains(plan.Tools, tool) != design {
				t.Errorf("design %v: %s granted %v", design, tool, !design)
			}
		}
		if _, ok := plan.Enforcement["design"]; ok != design || plan.Capabilities.DesignTools != design {
			t.Errorf("design %v: enforcement line %v, capability %v", design, ok, plan.Capabilities.DesignTools)
		}
	}
}
