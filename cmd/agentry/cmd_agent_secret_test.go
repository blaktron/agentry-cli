package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// memSecrets is a keychain in memory.
type memSecrets map[string]string

func (m memSecrets) Get(account string) (string, error) {
	v, ok := m[account]
	if !ok {
		return "", errors.New("no entry")
	}
	return v, nil
}

func (m memSecrets) Set(account, value string) error { m[account] = value; return nil }

func (m memSecrets) Delete(account string) error { delete(m, account); return nil }

// TestAgentSecretSetListRemove: set reads the value from stdin, keeps it in
// the keychain and the name in the configuration, asking first; list names
// it and never shows the value; a refused agreement keeps nothing; remove
// takes it out of both.
func TestAgentSecretSetListRemove(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	const value = "stored-test-value-0123456789"
	account := persistent.SecretAccount("coder", "GITHUB_TOKEN")

	_, _, err := runCLI(t, strings.NewReader(value+"\n"), "agent", "secret", "set", "coder", "GITHUB_TOKEN")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a secret stored unagreed: %v", err)
	}
	if _, kept := keys[account]; kept {
		t.Fatal("a refused secret's value was kept in the keychain")
	}
	out, _, err := runCLI(t, strings.NewReader(value+"\n"), "agent", "secret", "set", "coder", "GITHUB_TOKEN", "--yes")
	if err != nil || strings.Contains(out, value) {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if keys[account] != value {
		t.Fatalf("the keychain holds %q", keys[account])
	}
	b, err := os.ReadFile(filepath.Join(persistent.Dir(home, "coder"), "config.yaml"))
	if err != nil || !strings.Contains(string(b), "GITHUB_TOKEN") || strings.Contains(string(b), value) {
		t.Fatalf("config.yaml: %v\n%s", err, b)
	}
	out, _, err = runCLI(t, nil, "agent", "secret", "list", "coder")
	if err != nil || !strings.Contains(out, "GITHUB_TOKEN") || !strings.Contains(out, "send it anywhere they reach") || strings.Contains(out, value) {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "coder")
	if err != nil || !strings.Contains(out, "secret:GITHUB_TOKEN") || strings.Contains(out, value) {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "coder", "secret:GITHUB_TOKEN", "off"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ name, value string }{{"PATH", value}, {"github_token", value}, {"SHORT_ONE", "short"}, {"TWO_LINES", "one-line\nand-another"}} {
		_, _, err := runCLI(t, strings.NewReader(bad.value), "agent", "secret", "set", "coder", bad.name, "--yes")
		if err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%s=%q: %v", bad.name, bad.value, err)
		}
	}
	if _, _, err := runCLI(t, nil, "agent", "secret", "remove", "coder", "GITHUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, kept := keys[account]; kept {
		t.Error("a removed secret's value is still in the keychain")
	}
	if c, _ := persistent.LoadConfig(home, "coder"); len(c.Secrets) != 0 {
		t.Errorf("a removed secret is still configured: %+v", c.Secrets)
	}
	if len(keys) != 0 {
		t.Errorf("the keychain holds %v", keys)
	}
	_, _, err = runCLI(t, strings.NewReader(value), "agent", "secret", "set", "main", "GITHUB_TOKEN", "--yes")
	if err == nil {
		t.Error("Main was given a secret")
	}
}
