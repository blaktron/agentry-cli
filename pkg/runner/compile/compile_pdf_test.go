package compile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestBuiltinsGetNoExportPDF: export_pdf is the runner's grant to every
// release's run and session beside the scratch folder, never a line of the
// contract, and a built-in gets neither. The plan names it in its
// enforcement, and the System Prompt says when to export; the compiled plan
// carries no theme, author or ledger, which are the run's.
func TestBuiltinsGetNoExportPDF(t *testing.T) {
	for _, source := range []string{"registry", "local", "builtin"} {
		plan, dir, err := compileWebWith(t, webTextAgent, source, false, nil)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		granted := source != "builtin"
		if slices.Contains(plan.Tools, toolsrv.ToolExportPDF) != granted {
			t.Fatalf("%s: export_pdf granted %v in %v", source, !granted, plan.Tools)
		}
		if plan.Capabilities.FSWriteMode != "none" || plan.Capabilities.EgressMode != "none" {
			t.Fatalf("%s: export_pdf moved the contract: %+v", source, plan.Capabilities)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ts toolsrv.Spec
		if err := json.Unmarshal(raw, &ts); err != nil {
			t.Fatal(err)
		}
		if ts.PDF.Enabled != granted || ts.PDF.Theme != nil || ts.PDF.Author != "" || ts.PDF.Ledger != "" {
			t.Fatalf("%s: the compiled tools spec's pdf = %+v", source, ts.PDF)
		}
		if line, ok := plan.Enforcement["pdf"]; ok != granted || (granted && !strings.Contains(line, "the runner's grant, not the contract's")) {
			t.Fatalf("%s: enforcement names export_pdf %v: %q", source, ok, line)
		}
		agent, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if rule := "when the report is meant for someone other than the person"; strings.Contains(string(agent), rule) != granted {
			t.Fatalf("%s: the System Prompt's export rule present %v", source, !granted)
		}
	}
}
