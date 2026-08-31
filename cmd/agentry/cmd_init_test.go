// `agentry init` (§11): every scaffold must be a workspace `agentry
// validate` PASSES as-is — a template that fails its own toolchain teaches
// authors to ignore the tool — plus the guard rails: never overwrite an
// agent.yaml, keep existing files, refuse invalid handles.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitScaffoldsValidate: for each kind, init a workspace and run the
// REAL validate command over it, expecting a full PASS (manifest V1-V8, S1
// inventory, S3/S4 scan with the embedded bundle).
func TestInitScaffoldsValidate(t *testing.T) {
	isolateEnv(t)
	for _, kind := range []string{"skill", "prompt-agent", "mcp-server"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "my-"+kind)
			stdout, _, err := runCLI(t, nil, "init", dir, "--kind", kind, "--namespace", "acme")
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			if !strings.Contains(stdout, "initialized "+kind+" BriefAgent acme/my-"+kind) {
				t.Errorf("init banner = %s", stdout)
			}

			for _, f := range []string{"agent.yaml", "README.md", "LICENSE"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Errorf("scaffold lacks %s", f)
				}
			}
			entry := map[string]string{
				"skill": "SKILL.md", "prompt-agent": "prompts/main.md", "mcp-server": "src/server.js",
			}[kind]
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(entry))); err != nil {
				t.Errorf("scaffold lacks the %s entrypoint %s", kind, entry)
			}
			if kind == "mcp-server" {
				if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err != nil {
					t.Error("mcp-server scaffold lacks the V6 lockfile")
				}
			}

			vout, _, err := runCLI(t, nil, "validate", dir)
			if err != nil || !strings.Contains(vout, "validate: PASSED") {
				t.Errorf("scaffold does not validate: %v\n%s", err, vout)
			}
		})
	}
}

// TestInitGuards: existing files are kept (with a note), an existing
// agent.yaml refuses the whole scaffold, and invalid handles are refused
// with the grammar named.
func TestInitGuards(t *testing.T) {
	isolateEnv(t)

	// Pre-existing README: kept, not overwritten.
	dir := filepath.Join(t.TempDir(), "keeper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("MINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runCLI(t, nil, "init", dir, "--namespace", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "keeping existing README.md") {
		t.Errorf("keep note = %q", stderr)
	}
	kept, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(kept) != "MINE" {
		t.Error("init overwrote an existing README.md")
	}

	// A second init on the same dir: agent.yaml exists → refuse everything.
	_, _, err = runCLI(t, nil, "init", dir, "--namespace", "acme")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("re-init over agent.yaml: %v", err)
	}

	// Handle grammar violations name the fix.
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "Bad_Name"), "--namespace", "acme")
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Errorf("invalid derived name: %v", err)
	}
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "fine"), "--namespace", "Not-Valid-")
	if err == nil || !strings.Contains(err.Error(), "--namespace") {
		t.Errorf("invalid namespace: %v", err)
	}
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "fine"), "--kind", "daemon")
	if err == nil || !strings.Contains(err.Error(), "unknown --kind") {
		t.Errorf("invalid kind: %v", err)
	}
}

// TestDefaultNamespace: the $USER-derived scaffold default is sanitized to
// the handle grammar and falls back to "example" rather than emitting an
// invalid manifest.
func TestDefaultNamespace(t *testing.T) {
	tests := map[string]string{
		"chris":         "chris",
		"Chris.Dupres!": "chrisdupres",
		"--weird--":     "weird",
		"":              "example",
		"!!!":           "example",
	}
	for user, want := range tests {
		t.Setenv("USER", user)
		if got := defaultNamespace(); got != want {
			t.Errorf("USER=%q → %q, want %q", user, got, want)
		}
	}
}
