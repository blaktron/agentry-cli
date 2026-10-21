package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// consoleScriptAgent is a QuickAgent declaring one PyPI server whose only
// console script is not named after its project.
const consoleScriptAgent = `schemaVersion: "2"
name: word-probe
namespace: fixtures
modes: [autonomous]
version: 1.0.0
summary: "Declares one PyPI server whose console script is not named after its project"
license: Apache-2.0
tags: [fixture]
entrypoints:
  instruction: SKILL.md
capabilities:
  filesystem:
    read: none
    write: none
  network:
    egress: none
  exec: none
  mcpServers:
    - pypi: "some-word-server"
      version: 1.1.11
      tools: [create_document]
publish:
  include: [agent.yaml, SKILL.md]
`

// TestRunLaunchesTheConsoleScriptItsFirstInstallFound: the first run of a
// PyPI server installs it after the plan was compiled from the resolution's
// guess at its command; the plan the run executes launches the console
// script the install found, and the next run reuses that plan as it is.
func TestRunLaunchesTheConsoleScriptItsFirstInstallFound(t *testing.T) {
	isolateEnv(t)
	home, polPath := isolateRunnerHome(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pypi/some-word-server/1.1.11/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"requires_python":">=3.10"},"urls":[
		  {"packagetype":"bdist_wheel","filename":"some_word_server-1.1.11-py3-none-any.whl","digests":{"sha256":"cafe"}}]}`))
	})
	registry := httptest.NewTLSServer(mux)
	t.Cleanup(registry.Close)
	before := http.DefaultTransport
	http.DefaultTransport = registry.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = before })

	pol, _, err := policy.EnsureDefault(polPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.MCPRegistries.PyPI = registry.URL
	if err := policy.Save(polPath, pol); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"agent.yaml": consoleScriptAgent, "SKILL.md": "Say hello.\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	builds := 0
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"agentry-mcpinstall-pypi-some-word-server": {Before: func(r dockerx.RunSpec) {
			builds++
			if err := os.WriteFile(filepath.Join(r.Mounts[0].Source, "console-scripts"), []byte("word_server\n"), 0o600); err != nil {
				t.Error(err)
			}
		}},
	}}
	// The runner image, which the fake daemon does not keep once it is
	// built: the install proxy runs from it.
	f.ExistingImages = map[string]bool{pol.RunnerImageTag(runnerimage.ContentVersion()): true}
	seedHostLinkImage(t, f, polPath)
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })

	args := []string{"run", "--local", dir, "--allow-unsigned", "--yes", "--policy", polPath, "--prepare"}
	if _, stderr, err := runCLI(t, strings.NewReader(""), args...); err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr)
	}
	if builds != 1 {
		t.Fatalf("the server was built %d times, want once", builds)
	}
	want := spec.MCPStorePrefix + "/pypi-some-word-server/venv/bin/word_server"
	planDir := onlyPlanDir(t, home)
	assertLaunches(t, planDir, want)
	compiled, err := os.Stat(filepath.Join(planDir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, stderr)
	}
	if builds != 1 {
		t.Fatalf("the second run built the server again (%d builds)", builds)
	}
	// A compile replaces the plan's folder whole, so a reused plan is the
	// same file.
	if reused, err := os.Stat(filepath.Join(planDir, "plan.json")); err != nil || !os.SameFile(compiled, reused) {
		t.Fatalf("the second run compiled its plan again (%v):\n%s", err, stderr)
	}
	assertLaunches(t, planDir, want)
}

// onlyPlanDir is the one compiled plan's folder under the runner home.
func onlyPlanDir(t *testing.T, home string) string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(home, "agents", "*", "*", "*", "plan", "autonomous"))
	if err != nil || len(found) != 1 {
		t.Fatalf("plan folders = %v (%v)", found, err)
	}
	return found[0]
}

// assertLaunches holds the plan in planDir, and the gateway catalog compiled
// with it, to launching the one declared server with want.
func assertLaunches(t *testing.T, planDir, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(planDir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan spec.Plan
	if err := json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.MCPServers) != 1 || len(plan.MCPServers[0].Command) != 1 || plan.MCPServers[0].Command[0] != want {
		t.Fatalf("plan launches %+v, want %s", plan.MCPServers, want)
	}
	catalog, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), want) || strings.Contains(string(catalog), "/venv/bin/some-word-server") {
		t.Fatalf("the gateway catalog does not launch %s:\n%s", want, catalog)
	}
}
