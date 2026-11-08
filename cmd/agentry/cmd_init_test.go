// `agentry init`: every scaffold passes every manifest rule and scan
// `agentry validate` runs, and validate refuses it only for the lines left
// for its author to write (the TODOs and the licence note) until they are
// written; plus the guard rails: never overwrite an agent.yaml, keep
// existing files, refuse invalid handles.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitScaffoldsValidate: for each mode set, init a workspace and run the
// REAL validate command over it: every manifest rule, the S1 inventory and
// the S3/S4 scan with the embedded bundle pass, the scaffold's own lines are
// refused by name, and once they are written the workspace PASSES. The scaffold is schema
// v2: modes in the manifest, AGENT.md as the instruction file, and an
// interactive block only when interactive is chosen.
func TestInitScaffoldsValidate(t *testing.T) {
	isolateEnv(t)
	for _, modes := range []string{"autonomous", "interactive", "autonomous,interactive"} {
		t.Run(modes, func(t *testing.T) {
			name := "my-" + strings.ReplaceAll(modes, ",", "-")
			dir := filepath.Join(t.TempDir(), name)
			stdout, _, err := runCLI(t, nil, "init", dir, "--modes", modes, "--namespace", "acme")
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			if !strings.Contains(stdout, "initialized QuickAgent acme/"+name+" (modes: "+modes+")") {
				t.Errorf("init banner = %s", stdout)
			}

			for _, f := range []string{"agent.yaml", "README.md", "LICENSE", "AGENT.md"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Errorf("scaffold lacks %s", f)
				}
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "agent.yaml"))
			yaml := string(raw)
			if !strings.Contains(yaml, "schemaVersion: \"2\"") || strings.Contains(yaml, "\nkind:") {
				t.Errorf("scaffold is not a schema v2 manifest:\n%s", yaml)
			}
			if want := "modes: [" + strings.ReplaceAll(modes, ",", ", ") + "]"; !strings.Contains(yaml, want) {
				t.Errorf("manifest lacks %q:\n%s", want, yaml)
			}
			interactive := strings.Contains(modes, "interactive")
			if got := strings.Contains(yaml, "\ninteractive:\n  opening:"); got != interactive {
				t.Errorf("interactive block present = %v, want %v:\n%s", got, interactive, yaml)
			}

			wantOnlyScaffold(t, dir, "agent.yaml:TODO summary", "README.md:TODO", "LICENSE:placeholder licence note", "AGENT.md:TODO", "AGENT.md:TODO")

			// The author writes what the scaffold left them; then it passes.
			summary := "Reads the notes you give it and lists the decisions in them"
			finish := map[string]func(string) string{
				"agent.yaml": func(s string) string { return strings.Replace(s, todoSummary, summary, 1) },
				"AGENT.md": func(s string) string {
					s = strings.Replace(s, todoSummary, summary, 1)
					return strings.Replace(s, "TODO write the instructions the agent follows.", "Read each note and list the decisions it records.", 1)
				},
				"README.md": func(s string) string {
					return strings.Replace(s, "TODO describe this QuickAgent.", "Lists the decisions in the notes you give it.", 1)
				},
				"LICENSE": func(string) string { return "Licensed under the Apache License, Version 2.0.\n" },
			}
			for f, edit := range finish {
				raw, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, f), []byte(edit(string(raw))), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			vout, _, err := runCLI(t, nil, "validate", dir)
			if err != nil || !strings.Contains(vout, "validate: PASSED") {
				t.Fatalf("finished scaffold does not validate: %v\n%s", err, vout)
			}
			for _, want := range []string{"ran here:", "S4 injection signatures", "not run here:", "S5 AI Review", "S9 the diff against the previous version", "its verdict decides"} {
				if !strings.Contains(vout, want) {
					t.Errorf("validate's closing block lacks %q:\n%s", want, vout)
				}
			}
			if strings.Contains(vout, "clean") {
				t.Errorf("validate says clean:\n%s", vout)
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
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "fine"), "--modes", "daemon")
	if err == nil || !strings.Contains(err.Error(), "unknown mode \"daemon\"") {
		t.Errorf("invalid mode: %v", err)
	}
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "fine"), "--modes", "autonomous,autonomous")
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("repeated mode: %v", err)
	}
	// The withdrawn v1 flag points at its replacement instead of "unknown flag".
	_, _, err = runCLI(t, nil, "init", filepath.Join(t.TempDir(), "fine"), "--kind", "skill")
	if err == nil || !strings.Contains(err.Error(), "--modes") {
		t.Errorf("withdrawn --kind: %v", err)
	}
}

// TestDefaultNamespace: the $USER-derived scaffold default is sanitized to
// the handle grammar and falls back to "your-namespace" rather than emitting an
// invalid manifest.
func TestDefaultNamespace(t *testing.T) {
	tests := map[string]string{
		"chris":         "chris",
		"Chris.Dupres!": "chrisdupres",
		"--weird--":     "weird",
		"":              "your-namespace",
		"!!!":           "your-namespace",
	}
	for user, want := range tests {
		t.Setenv("USER", user)
		if got := defaultNamespace(); got != want {
			t.Errorf("USER=%q → %q, want %q", user, got, want)
		}
	}
}
