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

// TestLengthRecoveryOnlyForTheDefaultHarness: length_recovery is a key only
// the default harness build reads, and any other docker-agent refuses a config
// that carries it, so a plan turns it on for the default image alone.
func TestLengthRecoveryOnlyForTheDefaultHarness(t *testing.T) {
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
		{"the vendor's image", "docker/docker-agent:1.160.0", false, false},
		{"an earlier build a policy pins", "ghcr.io/blaktron/agentry-docker-agent-src:1.159.0-2026.11.10.11@sha256:892427b8d19ef28f0e023a7cb5452c00418e2906ca6fa74bd43ab0e8ae5e71b8", false, false},
	} {
		in := testInput()
		in.HarnessImage = tc.image
		in.Persistent = tc.persistent
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		y := string(res.Files["agent.yaml"])
		if got := strings.Contains(y, "length_recovery: true\n"); got != tc.want {
			t.Errorf("%s: length_recovery written = %v, want %v:\n%s", tc.name, got, tc.want, y)
		}
		if !tc.want && strings.Contains(y, "length_recovery") {
			t.Errorf("%s: the config names length_recovery:\n%s", tc.name, y)
		}
	}
}
