package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const codexImage = "ghcr.io/blaktron/agentry-codex:0.162.0@sha256:2222222222222222222222222222222222222222222222222222222222222222"

// Image preparation pulls the harness image the plan's adapter runs and
// no other (plan model-sign-ins.md D7): docker-agent's for its plans, the
// other adapter's alone for its, none for a host harness.
func TestPreparationPullsOnlyThePlansHarnessImage(t *testing.T) {
	pol := policy.Default()
	for _, c := range []struct {
		name       string
		harness    spec.HarnessSpec
		pull, skip []string
	}{
		{"docker-agent", spec.HarnessSpec{Adapter: "docker-agent", Agent: spec.ContainerSpec{Image: pol.Images.HarnessDockerAgent}},
			[]string{pol.Images.HarnessDockerAgent}, nil},
		{"codex image", spec.HarnessSpec{Adapter: "codex", Agent: spec.ContainerSpec{Image: codexImage}, ImagePin: &spec.ImagePin{By: policy.PinDigest}},
			[]string{codexImage}, []string{pol.Images.HarnessDockerAgent}},
		{"host", spec.HarnessSpec{Adapter: "claude", Host: &spec.HostProcessSpec{Command: "claude"}},
			nil, []string{pol.Images.HarnessDockerAgent}},
	} {
		f := &dockerx.Fake{ExistingImages: map[string]bool{"agentry-runner:test": true}}
		plan := &spec.Plan{Harness: c.harness}
		if err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ops := strings.Join(f.Ops(), " ")
		if !strings.Contains(ops, "Pull:"+pol.Images.Gateway) {
			t.Errorf("%s: the gateway was not prepared: %v", c.name, f.Ops())
		}
		for _, ref := range c.pull {
			if !strings.Contains(ops, "Pull:"+ref) {
				t.Errorf("%s: %s not pulled: %v", c.name, ref, f.Ops())
			}
		}
		for _, ref := range c.skip {
			if strings.Contains(ops, "Pull:"+ref) {
				t.Errorf("%s: %s pulled: %v", c.name, ref, f.Ops())
			}
		}
	}
}

// An image pinned by the artifact it is built from is never pulled, nor
// taken by its tag when present: this build can neither build nor check
// one, and says so by name.
func TestAnArtifactPinnedImageIsNeverPulled(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{ExistingImages: map[string]bool{"agentry-runner:test": true}}
	plan := &spec.Plan{Harness: spec.HarnessSpec{Adapter: "claude-image", Agent: spec.ContainerSpec{Image: "agentry-harness-claude:2.1.300"},
		ImagePin: &spec.ImagePin{By: policy.PinArtifact, Value: "sha256:33"}}}
	for _, present := range []bool{false, true} {
		f.ExistingImages["agentry-harness-claude:2.1.300"] = present
		err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan)
		if err == nil || !strings.Contains(err.Error(), "can neither build it nor check one already there") {
			t.Fatalf("present %v: %v", present, err)
		}
	}
	if strings.Contains(strings.Join(f.Ops(), " "), "Pull:agentry-harness-claude") {
		t.Fatalf("pulled: %v", f.Ops())
	}
}
