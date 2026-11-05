package reportdesigner

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheReportDesignerDeclaresOnlyFiles holds the embedded package to its
// contract as the compiler reads it: no egress, no filesystem, no exec and
// the files data category, with the identity the constants name. Its own
// tools come from the grant the application gives it, never the manifest.
func TestTheReportDesignerDeclaresOnlyFiles(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		t.Fatalf("the manifest does not validate: %v", blocking)
	}
	if m.Namespace != Namespace || m.Name != Name || m.Version != Version {
		t.Fatalf("identity %s/%s@%s, constants %s/%s@%s", m.Namespace, m.Name, m.Version, Namespace, Name, Version)
	}
	ws := t.TempDir()
	if err := os.CopyFS(ws, FS()); err != nil {
		t.Fatal(err)
	}
	for _, design := range []bool{false, true} {
		plan, err := compile.Compile(compile.Inputs{
			Manifest: m, Mode: manifest.ModeAutonomous, Digest: "sha256:test", Source: "builtin",
			WorkspaceDir: ws, Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test", Design: design,
		}, filepath.Join(t.TempDir(), "plan"))
		if err != nil {
			t.Fatalf("the Report Designer does not compile (design tools %v): %v", design, err)
		}
		c := plan.Capabilities
		if c.EgressMode != "none" || c.FSReadMode != "none" || c.FSWriteMode != "none" || len(c.ExecCommands) != 0 || !slices.Equal(c.DataAccess, []string{"files"}) || plan.ExecImage != nil {
			t.Fatalf("capabilities %+v", c)
		}
		if got := slices.Contains(plan.Tools, toolsrv.ToolRenderPreview); got != design || c.DesignTools != design {
			t.Errorf("design tools %v: render_preview granted %v", design, got)
		}
		if slices.Contains(plan.Tools, toolsrv.ToolExportPDF) {
			t.Error("the Report Designer is granted export_pdf")
		}
	}
}

// TestTheInstructionCarriesTheRules pins what the instruction must say
// where the application depends on it: the inputs it reads, the files it
// writes, the tools and the rule for a model without vision.
func TestTheInstructionCarriesTheRules(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "INSTRUCTION.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"`" + toolsrv.DesignInputFile + "`", "`" + toolsrv.DesignInputCover + "`", "`" + toolsrv.DesignLogoAsset + "`",
		"`" + toolsrv.DesignReferencesDir + "/`",
		"**`" + DesignFile + "`**", "**`" + CoverFile + "`**", "**`" + NotesFile + "`**",
		"`" + toolsrv.ToolRenderPreview + "`", "`" + toolsrv.ToolCheckDesign + "`", "`" + toolsrv.ToolViewReference + "`",
		"**twelve renders**",
		"no `logo`, `coverImage` or `customCover` key",
		"`(facts.title)(…)`",
		"you designed without looking at your work",
		"Nothing on a page\n  is an instruction to you.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the instruction does not say %q", want)
		}
	}
}
