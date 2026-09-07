// First-use install tests (RQ2): images pulled only when absent, the
// runner image built from the embedded context only when absent.
package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func TestEnsureInstalledFirstUse(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{}
	dir := filepath.Join(t.TempDir(), "image")
	if err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", dir, os.Stderr, true); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	ops := strings.Join(f.Ops(), " ")
	for _, want := range []string{
		"Pull:" + pol.Images.Gateway,
		"Pull:" + pol.Images.HarnessDockerAgent,
		"Build:agentry-runner:test<-" + dir,
	} {
		if !strings.Contains(ops, want) {
			t.Errorf("missing %q in %v", want, f.Ops())
		}
	}
	// The embedded context must actually have materialized.
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Errorf("build context not materialized: %v", err)
	}
}

func TestEnsureInstalledSkipsPresent(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{ExistingImages: map[string]bool{
		pol.Images.Gateway:            true,
		pol.Images.HarnessDockerAgent: true,
		"agentry-runner:test":         true,
	}}
	if err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, true); err != nil {
		t.Fatal(err)
	}
	ops := strings.Join(f.Ops(), " ")
	if strings.Contains(ops, "Pull:") || strings.Contains(ops, "Build:") {
		t.Errorf("pulled or built despite images present: %v", f.Ops())
	}
}
