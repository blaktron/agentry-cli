package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func fakeFilesDocker(t *testing.T, f *dockerx.Fake) {
	t.Helper()
	prev := filesDocker
	t.Cleanup(func() { filesDocker = prev })
	filesDocker = func() dockerx.Client { return f }
}

func pullsOf(f *dockerx.Fake) []string {
	var refs []string
	for _, c := range f.Calls {
		if c.Op == "Pull" {
			refs = append(refs, c.Arg)
		}
	}
	return refs
}

// With no policy file anywhere, the CLI's own pin is pulled and nothing is
// written under the runner home (agentry-cli#572).
func TestFilesEnsurePullsTheCLIsPinAndWritesNothing(t *testing.T) {
	isolateEnv(t)
	home := t.TempDir()
	t.Setenv(policy.HomeEnv, home)
	f := &dockerx.Fake{}
	fakeFilesDocker(t, f)

	out, _, err := runCLI(t, nil, "files", "ensure")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != policy.DefaultFilesImage {
		t.Fatalf("stdout %q, want %s", out, policy.DefaultFilesImage)
	}
	if got := pullsOf(f); len(got) != 1 || got[0] != policy.DefaultFilesImage {
		t.Fatalf("pulls %v", got)
	}
	if _, err := os.Stat(policy.PathIn(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a policy was written: %v", err)
	}

	// Here already: the same line, no pull.
	f2 := &dockerx.Fake{ExistingImages: map[string]bool{policy.DefaultFilesImage: true}}
	fakeFilesDocker(t, f2)
	out, _, err = runCLI(t, nil, "files", "ensure")
	if err != nil || strings.TrimSpace(out) != policy.DefaultFilesImage || len(pullsOf(f2)) != 0 {
		t.Fatalf("stdout %q, err %v, pulls %v", out, err, pullsOf(f2))
	}
}

func TestFilesEnsureFollowsThePolicysImageAndRefusesAnUnpinnedOne(t *testing.T) {
	isolateEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	pinned := "ghcr.io/blaktron/agentry-files:2026.10.03.1@sha256:" + strings.Repeat("ab", 32)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	pol := policy.Default()
	pol.Images.Files = pinned
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	f := &dockerx.Fake{}
	fakeFilesDocker(t, f)
	out, _, err := runCLI(t, nil, "files", "ensure", "--policy", path)
	if err != nil || strings.TrimSpace(out) != pinned {
		t.Fatalf("stdout %q, err %v", out, err)
	}

	// Save refuses an unpinned image, so the file is edited by hand, as a
	// person might; the policy's own check refuses it on load.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), pinned, "ghcr.io/blaktron/agentry-files:latest", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	f2 := &dockerx.Fake{}
	fakeFilesDocker(t, f2)
	_, _, err = runCLI(t, nil, "files", "ensure", "--policy", path)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "pinned by digest") {
		t.Fatalf("err %v", err)
	}
	if len(f2.Calls) != 0 {
		t.Fatalf("docker was called: %v", f2.Calls)
	}
}

// A --policy that names no file is an error, never the defaults. (A failed
// pull's retries and sentence are decode.PullFiles's, tested there.)
func TestFilesEnsureRefusesAMissingNamedPolicy(t *testing.T) {
	isolateEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	f := &dockerx.Fake{}
	fakeFilesDocker(t, f)
	_, _, err := runCLI(t, nil, "files", "ensure", "--policy", filepath.Join(t.TempDir(), "nope.yaml"))
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "nope.yaml does not exist") {
		t.Fatalf("err %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("docker was called: %v", f.Calls)
	}
}
