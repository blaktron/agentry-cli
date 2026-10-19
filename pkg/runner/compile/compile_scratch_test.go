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

// TestEveryRunGetsTheScratchFolder: a release, a local package and a
// built-in are each granted the scratch folder, grep and glob by the runner,
// whatever the contract. The grant is no line of the contract: the plan's
// contract fields are the manifest's, the plan says the folder is the
// runner's grant, and the System Prompt tells the model what it is.
func TestEveryRunGetsTheScratchFolder(t *testing.T) {
	scratchTools := []string{toolsrv.ToolScratchList, toolsrv.ToolScratchRead, toolsrv.ToolScratchWrite, toolsrv.ToolGrep, toolsrv.ToolGlob}
	for _, source := range []string{"registry", "local", "builtin"} {
		plan, dir, err := compileWebWith(t, webTextAgent, source, false, nil)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if !plan.Capabilities.Scratch {
			t.Fatalf("%s: plan grants scratch %v", source, plan.Capabilities.Scratch)
		}
		if plan.Capabilities.FSReadMode != "none" || plan.Capabilities.FSWriteMode != "none" {
			t.Fatalf("%s: the scratch folder moved the contract: read %q write %q", source, plan.Capabilities.FSReadMode, plan.Capabilities.FSWriteMode)
		}
		for _, name := range scratchTools {
			if !slices.Contains(plan.Tools, name) {
				t.Fatalf("%s: %s not granted in %v", source, name, plan.Tools)
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
		if ts.ScratchDir != "/scratch" {
			t.Fatalf("%s: the tools spec's scratch folder is %q", source, ts.ScratchDir)
		}
		if _, ok := plan.Enforcement["scratch"]; !ok {
			t.Fatalf("%s: enforcement does not name the scratch folder", source)
		}
		if !strings.Contains(plan.Enforcement["filesystem"], "mounts the tools container's /tmp as a 256 MiB tmpfs") {
			t.Fatalf("%s: enforcement does not name the bounded /tmp: %q", source, plan.Enforcement["filesystem"])
		}
		agent, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(agent), "**Scratch folder.**") {
			t.Fatalf("%s: the System Prompt has no scratch sentence", source)
		}
	}
}

// TestTheReviewGrantNeverSearchesItsInputs: a built-in under the review
// grant has the scratch folder, grep and glob, but its inputs reach the model
// only through read_input after AI Review, so the tools spec searches no
// inputs, search_inputs is not granted, and the System Prompt names no
// inputs area for grep.
func TestTheReviewGrantNeverSearchesItsInputs(t *testing.T) {
	plan, dir, err := compileReview(t, webTextAgent, "builtin", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Capabilities.Scratch || plan.Capabilities.PDF {
		t.Fatalf("review grant: scratch %v, pdf %v", plan.Capabilities.Scratch, plan.Capabilities.PDF)
	}
	for _, name := range []string{toolsrv.ToolScratchWrite, toolsrv.ToolGrep, toolsrv.ToolGlob, toolsrv.ToolAIReview} {
		if !slices.Contains(plan.Tools, name) {
			t.Fatalf("%s not granted: %v", name, plan.Tools)
		}
	}
	for _, name := range []string{toolsrv.ToolSearchInputs, toolsrv.ToolExportPDF} {
		if slices.Contains(plan.Tools, name) {
			t.Fatalf("%s granted under the review grant: %v", name, plan.Tools)
		}
	}
	agent, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(agent)
	if !strings.Contains(prompt, "They never search your inputs: read those with read_input.") ||
		strings.Contains(prompt, "named by its path: /inputs") {
		t.Fatal("the System Prompt offers grep over the inputs under the review grant")
	}
}
