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

func fakeBrowserDocker(t *testing.T, f *dockerx.Fake) {
	t.Helper()
	prev := browserDocker
	t.Cleanup(func() { browserDocker = prev })
	browserDocker = func() dockerx.Client { return f }
}

// browser ensure pulls the CLI's pin once, prints its reference alone, and
// writes nothing under the runner home: a sandbox host's bootstrap reads
// stdout and verifies the signature of what it names.
func TestBrowserEnsurePullsTheCLIsPinOnce(t *testing.T) {
	isolateEnv(t)
	home := t.TempDir()
	t.Setenv(policy.HomeEnv, home)
	f := &dockerx.Fake{}
	fakeBrowserDocker(t, f)
	out, _, err := runCLI(t, nil, "browser", "ensure")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != policy.DefaultBrowserImage {
		t.Fatalf("stdout %q, want %s", out, policy.DefaultBrowserImage)
	}
	if got := pullsOf(f); len(got) != 1 || got[0] != policy.DefaultBrowserImage {
		t.Fatalf("pulls %v", got)
	}
	if _, err := os.Stat(policy.PathIn(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a policy was written: %v", err)
	}

	f2 := &dockerx.Fake{ExistingImages: map[string]bool{policy.DefaultBrowserImage: true}}
	fakeBrowserDocker(t, f2)
	out, _, err = runCLI(t, nil, "browser", "ensure")
	if err != nil || strings.TrimSpace(out) != policy.DefaultBrowserImage || len(pullsOf(f2)) != 0 {
		t.Fatalf("stdout %q, err %v, pulls %v", out, err, pullsOf(f2))
	}
}

func TestBrowserEnsureFollowsThePolicysImageAndRefusesAnUnpinnedOne(t *testing.T) {
	isolateEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	pinned := "ghcr.io/blaktron/agentry-browser:2026.10.20.1@sha256:" + strings.Repeat("cd", 32)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	pol := policy.Default()
	pol.Images.Browser = pinned
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	f := &dockerx.Fake{}
	fakeBrowserDocker(t, f)
	out, _, err := runCLI(t, nil, "browser", "ensure", "--policy", path)
	if err != nil || strings.TrimSpace(out) != pinned || len(pullsOf(f)) != 1 {
		t.Fatalf("stdout %q, err %v, pulls %v", out, err, pullsOf(f))
	}

	raw, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	unpinned := strings.Replace(string(raw), pinned, "ghcr.io/blaktron/agentry-browser:latest", 1)
	if err := os.WriteFile(path, []byte(unpinned), 0o600); err != nil {
		t.Fatal(err)
	}
	f3 := &dockerx.Fake{}
	fakeBrowserDocker(t, f3)
	_, _, err = runCLI(t, nil, "browser", "ensure", "--policy", path)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "pinned by digest") || len(f3.Calls) != 0 {
		t.Fatalf("an unpinned image: err %v, docker calls %v", err, f3.Calls)
	}
}

// TestHostedForReadsEverySign: `agentry run` marks the engine's run hosted
// (Params.Hosted, which holds a browser to public sites) by any sign a
// sandbox host gives it, and a local run by none.
func TestHostedForReadsEverySign(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		opts runOpts
		want bool
	}{
		{name: "local", want: false},
		{name: "run grant", env: envRunGrant, want: true},
		{name: "job token", env: envJobToken, want: true},
		{name: "AGENTRY_HOSTED", env: envHosted, want: true},
		{name: "connections file", opts: runOpts{connectionsFile: "/x"}, want: true},
		{name: "session protocol", opts: runOpts{sessionProtocol: true}, want: true},
		{name: "input review", opts: runOpts{reviewInputs: "https://registry.example"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, e := range []string{envRunGrant, envJobToken, envHosted} {
				t.Setenv(e, "")
			}
			if tc.env != "" {
				t.Setenv(tc.env, "1")
			}
			if got := hostedFor(tc.opts); got != tc.want {
				t.Errorf("hostedFor = %v, want %v", got, tc.want)
			}
		})
	}
}
