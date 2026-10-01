// `agentry runner-tools` (RUNNER-DEVIATIONS DR-7): the hidden stdio mirror
// of the tools container. The spec decode is strict — an unknown field
// means a compiler/server version skew and must refuse, not half-work.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// toolSpecJSON renders a minimal valid tool spec (pure-text surface:
// write_report only).
func toolSpecJSON(t *testing.T) string {
	t.Helper()
	doc := map[string]any{
		"specVersion":  toolsrv.SpecVersion,
		"workspaceDir": t.TempDir(),
		"outDir":       t.TempDir(),
		"fsRead":       map[string]any{"enabled": false},
		"fsWrite":      map[string]any{"enabled": false},
		"fetch":        map[string]any{"enabled": false, "allowAll": false},
		"exec":         map[string]any{},
		"limits":       map[string]any{},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunnerToolsSpecGuards(t *testing.T) {
	_, _, err := runCLI(t, nil, "runner-tools")
	if err == nil || !strings.Contains(err.Error(), "--spec is required") {
		t.Errorf("missing --spec: %v", err)
	}

	_, _, err = runCLI(t, nil, "runner-tools", "--spec", filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !os.IsNotExist(errUnwrapAll(err)) {
		t.Errorf("absent spec file: %v", err)
	}

	// An unknown field is a schema skew between compiler and server —
	// refused by the strict decoder before serving anything.
	skewed := strings.Replace(toolSpecJSON(t), `"specVersion"`, `"bogusField":1,"specVersion"`, 1)
	specPath := filepath.Join(t.TempDir(), "skew.json")
	if err := os.WriteFile(specPath, []byte(skewed), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, nil, "runner-tools", "--spec", specPath)
	if err == nil || !strings.Contains(err.Error(), "parse "+specPath) {
		t.Errorf("unknown-field spec: %v", err)
	}
}

// TestRunnerToolsServeSmoke: one MCP initialize over stdio, exactly as the
// gateway's health probe does (§R6), then EOF shuts the server down clean.
func TestRunnerToolsServeSmoke(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "toolspec.json")
	if err := os.WriteFile(specPath, []byte(toolSpecJSON(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	stdout, _, err := runCLI(t, stdin, "runner-tools", "--spec", specPath)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ServerInfo map[string]any `json:"serverInfo"`
		} `json:"result"`
	}
	line := strings.TrimSpace(stdout)
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("initialize response is not JSON-RPC: %v\n%q", err, line)
	}
	if resp.JSONRPC != "2.0" || resp.ID != 1 || len(resp.Result.ServerInfo) == 0 {
		t.Errorf("initialize response = %+v", resp)
	}
}
