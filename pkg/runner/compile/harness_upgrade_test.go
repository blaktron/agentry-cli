package compile

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// An update must move the image an existing installation runs with the
// adapter's schema, without rewriting the operator's file.
func TestASavedFormerHarnessDefaultCompilesWithTheCurrentImage(t *testing.T) {
	for _, ref := range []string{
		"docker/docker-agent:v1.128.0",
		"docker/docker-agent:1.128.0",
		"ghcr.io/blaktron/agentry-docker-agent:1.128.0",
		"ghcr.io/blaktron/agentry-docker-agent:1.128.0@sha256:50bef17dfbc43c56f8c7c68eade9fc36cd4de5a2fc7ad0f28e34d0030002d5f1",
	} {
		t.Run(ref, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			written := testPolicy()
			written.Images.HarnessDockerAgent = ref
			written.Model.Name = "operator-model"
			if err := policy.Save(path, written); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			loaded, hash, err := policy.EnsureDefault(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := *written
			expected.Images.HarnessDockerAgent = policy.DefaultHarnessImage
			if !reflect.DeepEqual(loaded, &expected) {
				t.Errorf("the saved policy must resolve only its former harness default")
			}
			m := parseManifest(t, pureTextYAML)
			dir := t.TempDir()
			plan, err := Compile(Inputs{
				Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local",
				WorkspaceDir: workspaceFor(t, m, nil), Policy: loaded, PolicySHA: hash,
				RunnerImage: "agentry-runner:test",
			}, dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Harness.Agent.Image; got != policy.DefaultHarnessImage {
				t.Fatalf("an upgraded run launches %s, want %s", got, policy.DefaultHarnessImage)
			}
			config, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(config, []byte("version: \"16\"\n")) {
				t.Fatal("the current image's config must use schema 16")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("loading rewrote the policy: %v", err)
			}
			if want := fmt.Sprintf("%x", sha256.Sum256(before)); hash != want || plan.PolicySHA256 != want {
				t.Fatalf("policy hash changed: loaded %s, plan %s, want %s", hash, plan.PolicySHA256, want)
			}
		})
	}
}

func TestASavedCustomHarnessPinIsPreserved(t *testing.T) {
	for _, c := range []struct {
		ref     string
		refused bool
	}{
		{"registry.example/agent:current", false},
		{"ghcr.io/blaktron/agentry-docker-agent:1.150.0@sha256:" + strings.Repeat("0", 64), false},
		{"ghcr.io/blaktron/agentry-docker-agent:1.128.0@sha256:" + strings.Repeat("0", 64), true},
		{"docker.io/docker/docker-agent:1.128.0", true},
	} {
		t.Run(c.ref, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			written := testPolicy()
			written.Images.HarnessDockerAgent = c.ref
			if err := policy.Save(path, written); err != nil {
				t.Fatal(err)
			}
			loaded, hash, err := policy.EnsureDefault(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded, written) {
				t.Fatal("loading changed the custom policy")
			}
			m := parseManifest(t, pureTextYAML)
			plan, err := Compile(Inputs{
				Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local",
				WorkspaceDir: workspaceFor(t, m, nil), Policy: loaded, PolicySHA: hash,
				RunnerImage: "agentry-runner:test",
			}, t.TempDir())
			if c.refused {
				var refusal *PolicyRefusal
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "images.harnessDockerAgent") || !strings.Contains(err.Error(), "schema 16") || !strings.Contains(err.Error(), "update") {
					t.Fatalf("known incompatible custom pin must be refused with a remedy, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.Harness.Agent.Image != c.ref {
				t.Fatalf("compiled image = %s, want the custom pin %s", plan.Harness.Agent.Image, c.ref)
			}
		})
	}
}
