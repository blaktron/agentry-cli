package adapter

import (
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestColdAfterTable: docker-agent on Anthropic takes its cache as expired
// after 5 minutes, every other harness and provider after an hour, and an
// adapter with no row gets the hour too.
func TestColdAfterTable(t *testing.T) {
	for _, c := range []struct {
		adapter, provider string
		want              time.Duration
	}{
		{DefaultName, policy.ProviderAnthropic, 5 * time.Minute},
		{DefaultName, "openai", time.Hour},
		{DefaultName, "custom", time.Hour},
		{ClaudeImageAdapterName, policy.ProviderAnthropic, time.Hour},
		{CodexImageAdapterName, "openai", time.Hour},
		{CopilotImageAdapterName, "", time.Hour},
		{"some-future-harness", policy.ProviderAnthropic, time.Hour},
	} {
		if got := coldAfterFor(c.adapter, c.provider); got != c.want {
			t.Errorf("coldAfterFor(%q, %q) = %v, want %v", c.adapter, c.provider, got, c.want)
		}
	}
}

// TestTheDockerAgentPlanCarriesTheColdCacheClear: every docker-agent plan
// renders the block from the table, and names browser tools only when the
// plan has a browser.
func TestTheDockerAgentPlanCarriesTheColdCacheClear(t *testing.T) {
	for _, provider := range []string{policy.ProviderAnthropic, "openai"} {
		c := coldCacheClear(provider, nil)
		want := "1h"
		if provider == policy.ProviderAnthropic {
			want = "5m"
		}
		if c.After != want || c.KeepTurns != 2 || c.KeepBrowser != 2 || c.BrowserTools != nil {
			t.Errorf("%s: block = %+v, want after %s, 2 of each kept and no browser tools", provider, c, want)
		}
	}
	in := testInput()
	res, err := (&dockerAgent{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(res.Files["agent.yaml"])
	for _, want := range []string{"cold_cache_clear:", "keep_turns: 2", "keep_browser: 2"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("agent.yaml lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "browser_tools") {
		t.Errorf("a plan without a browser names browser tools:\n%s", cfg)
	}
	in.BrowserTools = []string{"browser_navigate", "browser_snapshot"}
	if res, err = (&dockerAgent{}).Compile(in); err != nil {
		t.Fatal(err)
	}
	if cfg = string(res.Files["agent.yaml"]); !strings.Contains(cfg, "browser_tools:\n        - browser_navigate\n        - browser_snapshot") {
		t.Errorf("agent.yaml does not name the browser's tools:\n%s", cfg)
	}
}

// TestDurationYAML: whole hours and minutes are written as such, anything
// else as Go writes it.
func TestDurationYAML(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", time.Hour: "1h", 90 * time.Minute: "90m", 90 * time.Second: "1m30s"} {
		if got := durationYAML(d); got != want {
			t.Errorf("durationYAML(%v) = %q, want %q", d, got, want)
		}
	}
}
