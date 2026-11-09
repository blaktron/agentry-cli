package adapter

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestCompactionNotesOnlyForTheDefaultHarness: compaction_notes is a key only
// the default harness build reads, and any other docker-agent refuses a config
// that carries it ("unknown field"), so a plan names the checklist file for
// the default image alone: in the scratch folder for a run, in the data
// folder for a persistent agent.
func TestCompactionNotesOnlyForTheDefaultHarness(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, image string
		persistent  bool
		want        string
	}{
		{"the default build", policy.DefaultHarnessImage, false, spec.ChecklistForHarness},
		{"the vendor's image", "docker/docker-agent:1.157.2", false, ""},
		{"an earlier build a policy pins", "ghcr.io/blaktron/agentry-docker-agent-src:1.154.0-2026.10.30.8@sha256:7a31b6babe98ceb0556dabfb45ed83998d6a9549b99c2c0a0536fdf52af9f217", false, ""},
		{"a persistent agent on the default build", policy.DefaultHarnessImage, true, spec.AgentChecklistForHarness},
	} {
		in := testInput()
		in.HarnessImage = tc.image
		in.Persistent = tc.persistent
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		y := string(res.Files["agent.yaml"])
		if tc.want != "" && !strings.Contains(y, "compaction_notes: "+tc.want+"\n") {
			t.Errorf("%s: compaction_notes is not %q:\n%s", tc.name, tc.want, y)
		}
		if tc.want == "" && strings.Contains(y, "compaction_notes") {
			t.Errorf("%s: the config names compaction_notes:\n%s", tc.name, y)
		}
	}
}
