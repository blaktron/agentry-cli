package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
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

// An image pinned by the artifact it is built from is never pulled. One
// this build cannot make (an adapter it has no build for) is refused by
// name whether or not an image of its tag is here; Claude Code's and
// Copilot CLI's are used when the image here carries its artifact, and a
// pin naming a release this build does not carry is refused before
// anything is fetched.
func TestAnArtifactPinnedImageIsNeverPulled(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{ExistingImages: map[string]bool{"agentry-runner:test": true}}
	plan := &spec.Plan{Harness: spec.HarnessSpec{Adapter: "gemini-image", Agent: spec.ContainerSpec{Image: "agentry-harness-gemini:1.0.0"},
		ImagePin: &spec.ImagePin{By: policy.PinArtifact, Value: "sha256:33"}}}
	for _, present := range []bool{false, true} {
		f.ExistingImages["agentry-harness-gemini:1.0.0"] = present
		err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan)
		if err == nil || !strings.Contains(err.Error(), "can neither build it nor check one already there") {
			t.Fatalf("present %v: %v", present, err)
		}
	}

	claude, _ := pol.Images.HarnessImageFor("claude-image")
	f.ExistingImages[claude.Ref] = true
	f.Labels = map[string]map[string]string{claude.Ref: {harnessbuild.LabelArtifact: claude.Artifact}}
	plan = &spec.Plan{Harness: spec.HarnessSpec{Adapter: "claude-image", Agent: spec.ContainerSpec{Image: claude.Ref},
		ImagePin: &spec.ImagePin{By: policy.PinArtifact, Value: claude.Artifact}}}
	if err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan); err != nil {
		t.Fatalf("a built Claude Code image carrying its artifact: %v", err)
	}
	plan.Harness.ImagePin.Value = "sha512-" + strings.Repeat("A", 86) + "=="
	if err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan); err == nil || !strings.Contains(err.Error(), "knows no Claude Code release") {
		t.Fatalf("an unknown release: %v", err)
	}
	copilot, _ := pol.Images.HarnessImageFor("copilot-image")
	f.ExistingImages[copilot.Ref] = true
	f.Labels[copilot.Ref] = map[string]string{harnessbuild.LabelArtifact: copilot.Artifact}
	plan = &spec.Plan{Harness: spec.HarnessSpec{Adapter: "copilot-image", Agent: spec.ContainerSpec{Image: copilot.Ref},
		ImagePin: &spec.ImagePin{By: policy.PinArtifact, Value: copilot.Artifact}}}
	if err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan); err != nil {
		t.Fatalf("a built Copilot CLI image carrying its artifact: %v", err)
	}
	plan.Harness.ImagePin.Value = "sha256:" + strings.Repeat("3", 64)
	if err := EnsureInstalledFor(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, plan); err == nil || !strings.Contains(err.Error(), "knows no Copilot CLI release") {
		t.Fatalf("an unknown Copilot release: %v", err)
	}
	ops := strings.Join(f.Ops(), " ")
	if strings.Contains(ops, "Pull:agentry-harness") || strings.Contains(ops, "Pull:agentry-claude-code") || strings.Contains(ops, "Build:agentry-claude-code") ||
		strings.Contains(ops, "Pull:agentry-copilot") || strings.Contains(ops, "Build:agentry-copilot") {
		t.Fatalf("pulled or built: %v", f.Ops())
	}
}
