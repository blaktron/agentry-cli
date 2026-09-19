package harnessprobe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeHarness writes an executable named like the adapter's that prints
// version as its first line, and points the PATH seam at it.
func fakeHarness(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	ad, err := adapter.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, ad.(adapter.HostHarness).Executable())
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho '"+version+"'\necho 'second line is ignored'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	old := lookPath
	lookPath = func(file string) (string, error) {
		if p := filepath.Join(dir, file); fileExists(p) {
			return p, nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { lookPath = old })
	return exe
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func noHarness(t *testing.T) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = old })
}

func TestProbeFoundOnPathWithMatchingVersion(t *testing.T) {
	exe := fakeHarness(t, adapter.ClaudeAdapterName, "2.1.263 (Claude Code)")
	r := Probe(context.Background(), policy.Default(), adapter.ClaudeAdapterName)
	if !r.Known || !r.Host || !r.Found || r.Path != exe || r.PathFromPolicy {
		t.Fatalf("result = %+v", r)
	}
	if r.Version != "2.1.263 (Claude Code)" || !r.VersionMatches || r.WrittenAgainst != "2.1.263" || r.VersionError != "" {
		t.Fatalf("version = %+v", r)
	}
	if r.Endpoint != nil {
		t.Fatalf("no baseUrl in the policy, yet an endpoint check: %+v", r.Endpoint)
	}
}

func TestProbeVersionMismatchIsReportedNotRefused(t *testing.T) {
	exe := fakeHarness(t, adapter.CodexAdapterName, "codex-cli 9.9.9")
	r := Probe(context.Background(), nil, adapter.CodexAdapterName)
	if !r.Found || r.VersionMatches || r.Version != "codex-cli 9.9.9" {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Warnings) != 1 || r.Warnings[0] != exe+" (codex-cli 9.9.9); this adapter was written against 0.153.4, so flags or event shapes may differ" {
		t.Fatalf("warnings = %q", r.Warnings)
	}
	if r.InstallURL != "https://github.com/openai/codex" {
		t.Fatalf("installUrl = %q", r.InstallURL)
	}
}

func TestProbeVersionUnreadableIsAWarningWithStderr(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'stdout noise'\necho 'no version for you' >&2\nexit 7\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: exe}}
	r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	if !r.Found || r.Version != "" || !strings.Contains(r.VersionError, "exit status 7") || !strings.Contains(r.VersionError, "no version for you") {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], exe+": cannot read its version: ") {
		t.Fatalf("warnings = %q", r.Warnings)
	}
}

func TestProbeVersionReadsStdoutOnly(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'warning: deprecated flag' >&2\necho '2.1.263'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: exe}}
	r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	if r.Version != "2.1.263" || !r.VersionMatches || len(r.Warnings) != 0 {
		t.Fatalf("a stderr line reached the version: %+v", r)
	}
}

func TestProbeHonoursTheCallerDeadline(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: exe}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := Probe(ctx, pol, adapter.ClaudeAdapterName)
	if time.Since(start) > 5*time.Second || r.VersionError == "" || r.Version != "" {
		t.Fatalf("a hanging harness was waited on: %+v after %s", r, time.Since(start))
	}
}

func TestProbeNotOnPath(t *testing.T) {
	noHarness(t)
	r := Probe(context.Background(), policy.Default(), adapter.ClaudeAdapterName)
	if !r.Known || !r.Host || r.Found || r.Path != "" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Reason, "not installed on this machine") || !strings.Contains(r.Reason, "harness.claude.path") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestProbePolicyPathMissing(t *testing.T) {
	noHarness(t)
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: "/nowhere/claude"}}
	r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	if r.Found || !strings.Contains(r.Reason, "policy harness.claude.path /nowhere/claude") {
		t.Fatalf("result = %+v", r)
	}
}

func TestProbePolicyPathWins(t *testing.T) {
	exe := fakeHarness(t, adapter.ClaudeAdapterName, "2.1.263")
	other := filepath.Join(t.TempDir(), "claude-elsewhere")
	if err := os.WriteFile(other, []byte("#!/bin/sh\necho 'claude 2.0.0'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: other}}
	r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	if !r.Found || r.Path != other || !r.PathFromPolicy || r.Path == exe {
		t.Fatalf("the policy path must win over PATH: %+v", r)
	}
	if r.VersionMatches {
		t.Fatalf("2.0.0 does not contain 2.1.263: %+v", r)
	}
}

func TestProbeEndpointAgainstAllowlist(t *testing.T) {
	noHarness(t) // the endpoint check does not need the executable
	pol := policy.Default()
	pol.Model.ProxyAllow = []string{"api.example.test:443"}
	for _, tc := range []struct {
		base    string
		valid   bool
		allowed bool
		hp      string
	}{
		{"https://api.example.test/v1", true, true, "api.example.test:443"},
		{"https://user:secret@api.example.test/v1", true, true, "api.example.test:443"},
		{"https://api.example.test:8443/v1", true, false, "api.example.test:8443"},
		{"http://api.example.test/v1", true, false, "api.example.test:80"},
		{"not a url", false, false, ""},
	} {
		pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {BaseURL: tc.base}}
		r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
		if r.Endpoint == nil {
			t.Fatalf("%s: no endpoint check", tc.base)
		}
		if r.Endpoint.Valid != tc.valid || r.Endpoint.Allowed != tc.allowed || r.Endpoint.HostPort != tc.hp {
			t.Fatalf("%s: endpoint = %+v", tc.base, *r.Endpoint)
		}
		if strings.Contains(r.Endpoint.BaseURL, "secret") {
			t.Fatalf("%s: userinfo reached the result: %+v", tc.base, *r.Endpoint)
		}
	}
}

func TestProbeContainerisedAdapter(t *testing.T) {
	r := Probe(context.Background(), policy.Default(), adapter.DefaultName)
	if !r.Known || r.Host || r.Found || r.Reason != "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestProbeUnknownAdapter(t *testing.T) {
	r := Probe(context.Background(), policy.Default(), "no-such-harness")
	if r.Known || r.Host || !strings.Contains(r.Reason, "unknown adapter") {
		t.Fatalf("result = %+v", r)
	}
}

func TestProbeWritesNothingAndRunsOnlyTheVersionFlag(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	// The fixture records its argv; anything but the version flag fails.
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"$@\" > \""+filepath.Join(dir, "argv")+"\"\n[ \"$1\" = --version ] || exit 7\necho 2.1.263\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: exe}}
	r := Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if strings.TrimSpace(string(argv)) != "--version" || r.VersionError != "" || r.Version != "2.1.263" {
		t.Fatalf("argv=%q result=%+v", argv, r)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 { // the executable and its argv record
		t.Fatalf("the probe wrote into the harness directory: %v", entries)
	}
}

func TestProbeLeavesThePolicyFileAlone(t *testing.T) {
	// policy.DefaultPath resolves under the user's home, so the home is
	// isolated first: a test that writes a policy anywhere else writes
	// the operator's own.
	t.Setenv("HOME", t.TempDir())
	home := filepath.Join(os.Getenv("HOME"), ".agentry", "runner")
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	path, err := policy.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, os.Getenv("HOME")) {
		t.Fatalf("the policy path %s is outside the isolated home", path)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {BaseURL: "https://api.anthropic.com"}}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	noHarness(t)
	_ = Probe(context.Background(), pol, adapter.ClaudeAdapterName)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the probe changed the policy file")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 1 {
		t.Fatalf("the probe wrote into the runner home: %v", entries)
	}
}
