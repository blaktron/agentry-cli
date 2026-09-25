package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// handMadePack is a pack of one page citing one source.
func handMadePack(t *testing.T, name string) string {
	t.Helper()
	return knowledgetest.Make(t, name, map[string]string{
		"wiki/topics/runbook.md": "---\ntitle: Runbook\ntype: topic\nsummary: What to do\nsources: [doc:notes.txt]\nupdated: 2026-09-25\n---\n\nRestart the thing [src: doc:notes.txt].\n",
		"sources/notes.txt":      "restart it\n",
	}).Dir
}

func TestRunKnowledgeFlag(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")
	dry := func(extra ...string) (string, string, error) {
		args := append([]string{"run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath}, extra...)
		return runCLI(t, strings.NewReader(""), args...)
	}

	t.Run("an attached pack does not change the plan", func(t *testing.T) {
		without, _, err := dry()
		if err != nil {
			t.Fatal(err)
		}
		with, stderr, err := dry("--knowledge", handMadePack(t, "runbooks"))
		if err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if with != without {
			t.Fatal("attaching a pack changed the printed plan")
		}
		if !strings.Contains(stderr, "knowledge pack runbooks: 1 page") || !strings.Contains(stderr, "not scanned") {
			t.Fatalf("stderr does not say what is attached and that it is not scanned: %q", stderr)
		}
	})
	t.Run("not a pack", func(t *testing.T) {
		_, _, err := dry("--knowledge", t.TempDir())
		wantClassified(t, err, engine.ExitCLI)
		if !strings.Contains(err.Error(), "--knowledge") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a pack whose files no longer match its digest", func(t *testing.T) {
		dir := handMadePack(t, "runbooks")
		if err := os.WriteFile(filepath.Join(dir, "sources", "notes.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := dry("--knowledge", dir)
		wantClassified(t, err, engine.ExitVerify)
	})
	t.Run("two packs of one name", func(t *testing.T) {
		_, _, err := dry("--knowledge", handMadePack(t, "runbooks"), "--knowledge", handMadePack(t, "runbooks"))
		wantClassified(t, err, engine.ExitCLI)
		if !strings.Contains(err.Error(), "both packs named runbooks") {
			t.Fatalf("err = %v", err)
		}
	})
}
