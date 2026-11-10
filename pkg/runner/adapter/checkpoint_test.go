package adapter

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestTheCheckpointHookOnlyForTheDefaultHarness: the default build's config
// carries one pre_compact command hook, the same one for a run and a
// persistent agent; a policy that pins another image gets none, as it gets no
// compaction_notes.
func TestTheCheckpointHookOnlyForTheDefaultHarness(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, image string
		persistent  bool
		want        bool
	}{
		{"the default build", policy.DefaultHarnessImage, false, true},
		{"a persistent agent on the default build", policy.DefaultHarnessImage, true, true},
		{"the vendor's image", "docker/docker-agent:1.157.2", false, false},
		{"an earlier build a policy pins", "ghcr.io/blaktron/agentry-docker-agent-src:1.154.0-2026.10.30.8@sha256:7a31b6babe98ceb0556dabfb45ed83998d6a9549b99c2c0a0536fdf52af9f217", false, false},
	} {
		in := testInput()
		in.HarnessImage = tc.image
		in.Persistent = tc.persistent
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var doc struct {
			Agents map[string]struct {
				Hooks struct {
					PreCompact []hookYAML `yaml:"pre_compact"`
				} `yaml:"hooks"`
				Toolsets []any `yaml:"toolsets"`
			} `yaml:"agents"`
		}
		if err := yaml.Unmarshal(res.Files["agent.yaml"], &doc); err != nil {
			t.Fatal(err)
		}
		root := doc.Agents["root"]
		if len(root.Toolsets) != 1 {
			t.Errorf("%s: %d toolsets, want the gateway's alone", tc.name, len(root.Toolsets))
		}
		if !tc.want {
			if len(root.Hooks.PreCompact) != 0 {
				t.Errorf("%s: the config carries a pre_compact hook: %+v", tc.name, root.Hooks.PreCompact)
			}
			continue
		}
		if len(root.Hooks.PreCompact) != 1 || root.Hooks.PreCompact[0] != checkpointHook() {
			t.Fatalf("%s: pre_compact %+v, want the checkpoint hook", tc.name, root.Hooks.PreCompact)
		}
	}
}

// TestTheCheckpointHookPrintsItsText: the command, run by sh as the harness
// runs a command hook, prints the template and nothing else, and the
// template names every heading the summary must keep.
func TestTheCheckpointHookPrintsItsText(t *testing.T) {
	if strings.ContainsAny(checkpointText, "'\n") {
		t.Fatal("the template holds a single quote or a line break, which the command's quoting cannot carry")
	}
	for _, heading := range []string{"the task", "the checklist", "status", "blocked", "the next step", "files, paths and identifiers", "constraints", "approaches that failed"} {
		if !strings.Contains(checkpointText, heading) {
			t.Errorf("the template lacks %q", heading)
		}
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	out, err := exec.Command(sh, "-c", checkpointHook().Command).Output() //nolint:gosec // the hook's own fixed command
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != checkpointText+"\n" {
		t.Fatalf("the hook printed %q", out)
	}
}
