package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestAgentConfigureAccount: --account reads the token from stdin, tells
// the person what the account means, keeps the token in the keychain and
// the account in the configuration, asking first; a refused agreement keeps
// nothing; setting it again with a new user name drops the old token;
// --no-account takes it out of both.
func TestAgentConfigureAccount(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	const token = "ghp_clitesttoken0123456789"
	account := persistent.AccountKeychainAccount("github.com", "coder-bot")
	args := []string{"agent", "configure", "coder", "--account", "github.com", "--own", "--account-user", "coder-bot", "--account-email", "bot@example.com"}

	_, errOut, err := runCLI(t, strings.NewReader(token+"\n"), args...)
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(errOut, "one free machine account") {
		t.Fatalf("an account given unagreed: %v\n%s", err, errOut)
	}
	if _, kept := keys[account]; kept {
		t.Fatal("a refused account's token was kept in the keychain")
	}
	out, errOut, err := runCLI(t, strings.NewReader(token+"\n"), append(args, "--yes")...)
	if err != nil || strings.Contains(out, token) || strings.Contains(errOut, token) {
		t.Fatalf("configure: %v\n%s\n%s", err, out, errOut)
	}
	if keys[account] != token {
		t.Fatalf("the keychain holds %q", keys[account])
	}
	b, err := os.ReadFile(filepath.Join(persistent.Dir(home, "coder"), "config.yaml"))
	if err != nil || !strings.Contains(string(b), "coder-bot") || !strings.Contains(string(b), "name: coder-bot") || strings.Contains(string(b), token) {
		t.Fatalf("config.yaml: %v\n%s", err, b)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "coder")
	if err != nil || !strings.Contains(out, "account:github.com") || strings.Contains(out, token) {
		t.Fatalf("show: %v\n%s", err, out)
	}
	// Its token set again under another user name: the old one goes.
	if _, _, err := runCLI(t, strings.NewReader(token+"-2\n"), "agent", "configure", "coder", "--account", "github.com", "--account-user", "coder-bot-2", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, kept := keys[account]; kept || keys[persistent.AccountKeychainAccount("github.com", "coder-bot-2")] != token+"-2" {
		t.Fatalf("the keychain after a new user name: %v", keys)
	}
	for name, bad := range map[string][]string{
		"no whose":       {"--account", "gitlab.com", "--account-user", "me", "--account-email", "me@example.com"},
		"both whose":     {"--account", "gitlab.com", "--own", "--yours", "--account-user", "me", "--account-email", "me@example.com"},
		"no kind":        {"--account", "git.example.com", "--yours", "--account-user", "me", "--account-email", "me@example.com"},
		"no email":       {"--account", "gitlab.com", "--yours", "--account-user", "me"},
		"a URL":          {"--account", "https://gitlab.com/", "--yours", "--account-user", "me", "--account-email", "me@example.com"},
		"a short token":  {"--account", "gitlab.com", "--yours", "--account-user", "me", "--account-email", "me@example.com", "--account-kind", "gitlab"},
		"flags alone":    {"--own"},
		"both ways":      {"--no-account", "github.com", "--own"},
		"not the coder":  {"--account", "gitlab.com", "--yours", "--account-user", "me", "--account-email", "me@example.com"},
		"a kind not one": {"--account", "gitlab.com", "--yours", "--account-user", "me", "--account-email", "me@example.com", "--account-kind", "svn"},
	} {
		agent, in := "coder", strings.NewReader(token)
		if name == "not the coder" {
			agent = "assistant"
		}
		if name == "a short token" {
			in = strings.NewReader("short")
		}
		_, _, err := runCLI(t, in, append([]string{"agent", "configure", agent, "--yes"}, bad...)...)
		if err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "coder", "--no-account", "github.com"); err != nil {
		t.Fatal(err)
	}
	if c, _ := persistent.LoadConfig(home, "coder"); len(c.Accounts) != 0 {
		t.Errorf("a removed account is still configured: %+v", c.Accounts)
	}
	if len(keys) != 0 {
		t.Errorf("the keychain holds %v", keys)
	}
}
