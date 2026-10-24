package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestAgentConfigureFastmail: --fastmail-token asks Fastmail about the
// token read from stdin and, once it accepts, keeps it in the keychain and
// enables the fastmail provider, never printing it; a token Fastmail
// refuses is not kept; --no-fastmail removes it; the flags together, with
// another change, or for the Coding agent are refused as usage.
func TestAgentConfigureFastmail(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	accept := persistent.FastmailTokenAccepted
	t.Cleanup(func() { persistent.FastmailTokenAccepted = accept })
	var asked []string
	refuse := false
	persistent.FastmailTokenAccepted = func(_ context.Context, token string) error {
		asked = append(asked, token)
		if refuse {
			return persistent.ErrFastmailRefused
		}
		return nil
	}
	const token = "fmu1-test-token-0123456789"
	account := persistent.SecretAccount("assistant", assistant.FastmailToken)

	refuse = true
	_, _, err := runCLI(t, strings.NewReader(token+"\n"), "agent", "configure", "assistant", "--fastmail-token", "--yes")
	if err == nil || exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "the token was refused by Fastmail") {
		t.Fatalf("a refused token: %v", err)
	}
	if _, kept := keys[account]; kept {
		t.Fatal("a token Fastmail refused was kept")
	}
	refuse = false
	out, _, err := runCLI(t, strings.NewReader(token+"\n"), "agent", "configure", "assistant", "--fastmail-token", "--yes")
	if err != nil || strings.Contains(out, token) || !strings.Contains(out, "Fastmail accepted the token") {
		t.Fatalf("--fastmail-token: %v\n%s", err, out)
	}
	if keys[account] != token || !slices.Equal(asked, []string{token, token}) {
		t.Fatalf("the keychain holds %q; Fastmail was asked %d times", keys[account], len(asked))
	}
	cfg, err := persistent.LoadConfig(home, "assistant")
	if err != nil || !slices.Contains(cfg.MailProviders, assistant.MailFastmail) || !slices.ContainsFunc(cfg.Secrets, func(s persistent.StoredSecret) bool { return s.Name == assistant.FastmailToken }) {
		t.Fatalf("config: %v %+v", err, cfg)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "assistant")
	if err != nil || !strings.Contains(out, "Fastmail: enabled") || strings.Contains(out, token) {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--no-fastmail"); err != nil {
		t.Fatal(err)
	}
	if _, kept := keys[account]; kept {
		t.Fatal("--no-fastmail kept the token")
	}
	cfg, _ = persistent.LoadConfig(home, "assistant")
	if slices.ContainsFunc(cfg.Secrets, func(s persistent.StoredSecret) bool { return s.Name == assistant.FastmailToken }) {
		t.Fatalf("--no-fastmail kept the token's name: %+v", cfg.Secrets)
	}
	for _, args := range [][]string{
		{"assistant", "--fastmail-token", "--no-fastmail"},
		{"assistant", "--fastmail-token", "--name", "Pat"},
		{"assistant", "--no-fastmail"},
		{"coder", "--fastmail-token"},
	} {
		_, _, err := runCLI(t, strings.NewReader(token), append([]string{"agent", "configure"}, args...)...)
		if err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
