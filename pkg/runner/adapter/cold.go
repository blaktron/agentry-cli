package adapter

import (
	"fmt"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A persistent agent's old tool results leave its history at the one moment
// that costs no prompt cache: when a turn starts after the cache has expired
// and the whole history is re-sent uncached anyway. coldAfter says when that
// is: how long after a model response a harness's prompt cache, on a
// provider, is taken as expired. A row with no provider holds for every
// provider of that harness. Each row is re-measured when its harness is
// bumped, as the result budgets are.
var coldAfter = map[coldKey]time.Duration{
	// docker-agent marks its cache points for 5 minutes on Anthropic.
	{DefaultName, policy.ProviderAnthropic}: 5 * time.Minute,
	// Another provider's cache lives longer or not at all. A Claude model
	// reached through another provider (OpenRouter, Poe) takes the hour too:
	// what such a provider caches, and for how long, is its own.
	{DefaultName, ""}: coldAfterLong,
	// Claude Code on a Claude subscription caches for an hour.
	{ClaudeImageAdapterName, ""}: coldAfterLong,
	// Codex on a ChatGPT plan and Copilot CLI on a Copilot plan: the
	// providers' caches live up to an hour.
	{CodexImageAdapterName, ""}:   coldAfterLong,
	{CopilotImageAdapterName, ""}: coldAfterLong,
}

// coldKey is a row of coldAfter: an adapter, and a provider or "" for any.
type coldKey struct{ adapter, provider string }

// coldAfterLong is the lifetime taken for a harness or provider with no
// shorter row.
const coldAfterLong = 60 * time.Minute

// What a cold-cache clear keeps whole.
const (
	// coldKeepTurns is how many turns before the current one keep their
	// tool results.
	coldKeepTurns = 2
	// coldKeepBrowser is how many of the newest browser results stay,
	// wherever they fall: a page snapshot is stale at the next navigation.
	coldKeepBrowser = 2
)

// coldAfterFor is how long after a model response the adapter named name, on
// provider (the policy's name), takes its prompt cache as expired: the
// adapter's row for that provider, else its row for any, else the longest.
func coldAfterFor(name, provider string) time.Duration {
	if d, ok := coldAfter[coldKey{name, provider}]; ok {
		return d
	}
	if d, ok := coldAfter[coldKey{name, ""}]; ok {
		return d
	}
	return coldAfterLong
}

// coldCacheClearYAML is the harness's cold_cache_clear block: past `after`
// since the previous model response,
// a turn's first request sends the tool results older than the newest
// keep_turns turns, and the browser results but the newest keep_browser, as a
// placeholder naming the call.
type coldCacheClearYAML struct {
	After        string   `yaml:"after"`
	KeepTurns    int      `yaml:"keep_turns"`
	KeepBrowser  int      `yaml:"keep_browser"`
	BrowserTools []string `yaml:"browser_tools,omitempty"`
}

// coldCacheClear is the block for a plan on provider whose browser server, if
// it has one, passes browserTools. It is written on every plan: an autonomous
// run is one turn and never reaches it.
func coldCacheClear(provider string, browserTools []string) *coldCacheClearYAML {
	return &coldCacheClearYAML{
		After:        durationYAML(coldAfterFor(DefaultName, provider)),
		KeepTurns:    coldKeepTurns,
		KeepBrowser:  coldKeepBrowser,
		BrowserTools: browserTools,
	}
}

// durationYAML writes d in whole hours or minutes where it can, as
// docker-agent's duration fields read it ("5m", "1h").
func durationYAML(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
