package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// On a remote runhost the per-run tools spec says a workspace-writing
// command is held by ownership; the rest of the spec is kept as it was.
func TestTheRunhostsToolSpecHoldsWritesByOwnership(t *testing.T) {
	p := filepath.Join(t.TempDir(), "toolspec.json")
	if err := os.WriteFile(p, []byte(`{"workspaceDir":"/workspace","owner":{"uid":999,"gid":999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stampWorkspaceOwnership(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["workspaceOwnership"] != true || doc["workspaceDir"] != "/workspace" || doc["owner"] == nil {
		t.Fatalf("stamped %s", b)
	}
	if err := stampWorkspaceOwnership(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Fatalf("a plan without a tools server: %v", err)
	}
}
