package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

func TestKnowledgeCommands(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("the warning comes before anything is built", func(t *testing.T) {
		// The fake refuses the first image build: whatever the build does
		// first, the person has already been told the pack is not scanned.
		prev := knowledgeDocker
		t.Cleanup(func() { knowledgeDocker = prev })
		knowledgeDocker = func() dockerx.Client {
			return &dockerx.Fake{FailBuild: map[string]error{}, FailPull: map[string]error{}, AvailableErr: errors.New("no docker here")}
		}
		dir := filepath.Join(t.TempDir(), "notes")
		_, stderr, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", dir, "--file", doc, "--policy", policyPath)
		if err == nil {
			t.Fatal("the build went through a docker client that refuses")
		}
		warning := strings.Index(stderr, "nothing it reads or writes is scanned")
		if warning < 0 {
			t.Fatalf("no warning in stderr:\n%s", stderr)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Fatal("a failed build left a pack directory")
		}
	})
	t.Run("create refuses a name that is not a pack name", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "create", "Bad Name", "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("create refuses a directory that holds a pack", func(t *testing.T) {
		p := knowledgetest.Make(t, "notes", nil)
		_, _, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", p.Dir, "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "knowledge add") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("add refuses a directory with no pack", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "add", t.TempDir(), "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("a build names its documents", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "n"), "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
}
