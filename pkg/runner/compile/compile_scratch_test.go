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

// TestEveryReleaseGetsTheScratchFolder: a release or a local package is
// granted the scratch folder by the runner, whatever its contract, and a
// built-in is not. The grant is no line of the contract: the plan's
// contract fields are the manifest's, the plan says the folder is the
// runner's grant, and the System Prompt tells the model what it is.
func TestEveryReleaseGetsTheScratchFolder(t *testing.T) {
	scratchTools := []string{toolsrv.ToolScratchList, toolsrv.ToolScratchRead, toolsrv.ToolScratchWrite}
	for _, source := range []string{"registry", "local", "builtin"} {
		plan, dir, err := compileWebWith(t, webTextAgent, source, false, nil)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		granted := source != "builtin"
		if plan.Capabilities.Scratch != granted {
			t.Fatalf("%s: plan grants scratch %v", source, plan.Capabilities.Scratch)
		}
		if plan.Capabilities.FSReadMode != "none" || plan.Capabilities.FSWriteMode != "none" {
			t.Fatalf("%s: the scratch folder moved the contract: read %q write %q", source, plan.Capabilities.FSReadMode, plan.Capabilities.FSWriteMode)
		}
		for _, name := range scratchTools {
			if slices.Contains(plan.Tools, name) != granted {
				t.Fatalf("%s: %s granted %v in %v", source, name, !granted, plan.Tools)
			}
		}
		for _, name := range []string{toolsrv.ToolFSRead, toolsrv.ToolFSList, toolsrv.ToolFSWrite} {
			if slices.Contains(plan.Tools, name) {
				t.Fatalf("%s: the scratch folder granted the workspace's %s", source, name)
			}
		}
		raw, err := os.ReadFile(filepath.Join(dir, "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ts toolsrv.Spec
		if err := json.Unmarshal(raw, &ts); err != nil {
			t.Fatal(err)
		}
		if (ts.ScratchDir == "/scratch") != granted {
			t.Fatalf("%s: the tools spec's scratch folder is %q", source, ts.ScratchDir)
		}
		if _, ok := plan.Enforcement["scratch"]; ok != granted {
			t.Fatalf("%s: enforcement names the scratch folder: %v", source, ok)
		}
		if !strings.Contains(plan.Enforcement["filesystem"], "mounts the tools container's /tmp as a 256 MiB tmpfs") {
			t.Fatalf("%s: enforcement does not name the bounded /tmp: %q", source, plan.Enforcement["filesystem"])
		}
		agent, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(agent), "**Scratch folder.**") != granted {
			t.Fatalf("%s: the System Prompt's scratch sentence present %v", source, !granted)
		}
	}
}
