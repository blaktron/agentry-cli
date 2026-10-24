package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestAgentConfigureMicrosoft: --microsoft-client records the Entra app
// (with --microsoft-tenant) and enables Microsoft, --microsoft-signoff sets
// the signoff and --no-microsoft-signoff clears it; Softeria's client, a
// tenant without a client, both signoff flags, the flags with another
// change, and the Coding agent are refused as usage.
func TestAgentConfigureMicrosoft(t *testing.T) {
	home := agentHome(t)
	out, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--microsoft-client", "11111111-2222-3333-4444-555555555555", "--microsoft-tenant", "organizations")
	if err != nil || !strings.Contains(out, "--microsoft-sign-in") {
		t.Fatalf("--microsoft-client: %v\n%s", err, out)
	}
	cfg, _ := persistent.LoadConfig(home, "assistant")
	if in := cfg.SignIns["ms365"]; in.ClientID != "11111111-2222-3333-4444-555555555555" || in.Tenant != "organizations" || !slices.Contains(cfg.MailProviders, "microsoft") {
		t.Fatalf("after --microsoft-client: %+v", cfg)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--microsoft-signoff", "Sent by my assistant"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); cfg.SignIns["ms365"].Signoff != "Sent by my assistant" {
		t.Fatalf("after --microsoft-signoff: %+v", cfg.SignIns)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--no-microsoft-signoff"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); cfg.SignIns["ms365"].Signoff != "" {
		t.Fatalf("after --no-microsoft-signoff: %+v", cfg.SignIns)
	}
	for _, args := range [][]string{
		{"assistant", "--microsoft-client", "084a3e9f-a9f4-43f7-89f9-d229cf97853e"},
		{"assistant", "--microsoft-tenant", "consumers"},
		{"assistant", "--microsoft-signoff", "x", "--no-microsoft-signoff"},
		{"assistant", "--microsoft-client", "11111111-2222-3333-4444-555555555555", "--name", "Ada"},
		{"coder", "--microsoft-client", "11111111-2222-3333-4444-555555555555"},
	} {
		if _, _, err := runCLI(t, nil, append([]string{"agent", "configure"}, args...)...); err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
