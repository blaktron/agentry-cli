package adapter

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestCompactionNotesOnlyForTheDefaultHarness: compaction_notes is a key only
// the default harness build reads, and any other docker-agent refuses a config
// that carries it ("unknown field"), so a plan names the notes file for the
// default image alone, and never for a persistent agent.
func TestCompactionNotesOnlyForTheDefaultHarness(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	want := "compaction_notes: '" + spec.PHNotesFile + "'"
	for _, tc := range []struct {
		name, image string
		persistent  bool
		has         bool
	}{
		{"the default build", policy.DefaultHarnessImage, false, true},
		{"the vendor's image", "docker/docker-agent:1.157.2", false, false},
		{"an earlier build a policy pins", "ghcr.io/blaktron/agentry-docker-agent-src:1.154.0-2026.10.30.8@sha256:7a31b6babe98ceb0556dabfb45ed83998d6a9549b99c2c0a0536fdf52af9f217", false, false},
		{"a persistent agent on the default build", policy.DefaultHarnessImage, true, false},
	} {
		in := testInput()
		in.HarnessImage = tc.image
		in.Persistent = tc.persistent
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		y := string(res.Files["agent.yaml"])
		if got := strings.Contains(y, want); got != tc.has {
			t.Errorf("%s: compaction_notes written = %v, want %v:\n%s", tc.name, got, tc.has, y)
		}
		if !tc.has && strings.Contains(y, "compaction_notes") {
			t.Errorf("%s: the config names compaction_notes:\n%s", tc.name, y)
		}
	}
}
