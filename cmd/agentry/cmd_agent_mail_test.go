package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestAgentConfigureMailProviders: --mail-provider enables Google and
// --no-mail-provider disables it, keeping its sign-in; show lists the
// providers; an unknown provider, one both enabled and disabled, the flags
// with another change, and the Coding agent are refused as usage.
func TestAgentConfigureMailProviders(t *testing.T) {
	home := agentHome(t)
	if err := persistent.SaveConfig(home, "assistant", persistent.Config{Enabled: true, SignIns: map[string]persistent.ImageSignIn{"gog": {Account: "person@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--mail-provider", "google")
	if err != nil || !strings.Contains(out, "google\tGoogle: enabled, signed in as person@example.com") {
		t.Fatalf("--mail-provider google: %v\n%s", err, out)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); !slices.Equal(cfg.MailProviders, []string{"google"}) {
		t.Fatalf("enabled: %+v", cfg.MailProviders)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "assistant")
	if err != nil || !strings.Contains(out, "mail providers (agentry agent configure assistant --mail-provider <key>):") || !strings.Contains(out, "Google: enabled") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--no-mail-provider", "google"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := persistent.LoadConfig(home, "assistant")
	if len(cfg.MailProviders) != 0 || cfg.SignIns["gog"].Account != "person@example.com" {
		t.Fatalf("disabled: %+v", cfg)
	}
	for _, args := range [][]string{
		{"assistant", "--mail-provider", "yahoo"},
		{"assistant", "--mail-provider", "google", "--no-mail-provider", "google"},
		{"assistant", "--mail-provider", "google", "--name", "Ada"},
		{"coder", "--mail-provider", "google"},
	} {
		if _, _, err := runCLI(t, nil, append([]string{"agent", "configure"}, args...)...); err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
