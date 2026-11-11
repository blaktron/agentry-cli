// Package modelfacts is what the runner knows about a hosted model without
// asking anyone at run time: its context window, its output limit and its
// prices.
//
// The facts are a snapshot compiled into the CLI (catalog.json), a trimmed
// copy of models.dev's public list for the providers a policy can name;
// its licence is in LICENSE.models-dev. scripts/modelfacts/update.py
// regenerates it. Nothing here reaches the network: a model the snapshot
// does not hold has no facts, and the runner goes on without them.
package modelfacts

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed catalog.json
var raw []byte

// Facts is what the catalogue holds about one model.
type Facts struct {
	// ContextWindow is the model's window in tokens, and MaxOutput the
	// most output one request may ask for; zero when not listed.
	ContextWindow int64
	MaxOutput     int64
	// Priced reports that the catalogue lists the model's prices. The
	// four prices are dollars a million tokens: input read fresh, input
	// read from the provider's cache, input written to it, and output.
	// A cache price the catalogue does not list is the fresh price.
	Priced     bool
	Input      float64
	CacheRead  float64
	CacheWrite float64
	Output     float64
}

type entry struct {
	Ctx        int64    `json:"ctx"`
	Out        int64    `json:"out"`
	In         *float64 `json:"in"`
	O          *float64 `json:"o"`
	CacheRead  *float64 `json:"cr"`
	CacheWrite *float64 `json:"cw"`
}

type catalog struct {
	Source    string                      `json:"source"`
	Fetched   string                      `json:"fetched"`
	Providers map[string]map[string]entry `json:"providers"`
}

var load = sync.OnceValue(func() catalog {
	var c catalog
	// The file is compiled in and its shape is held by a test.
	_ = json.Unmarshal(raw, &c)
	return c
})

// Lookup returns the facts for a provider's model, by the policy's
// provider name and the model's id as the provider knows it.
func Lookup(provider, model string) (Facts, bool) {
	e, ok := load().Providers[provider][model]
	if !ok {
		return Facts{}, false
	}
	f := Facts{ContextWindow: e.Ctx, MaxOutput: e.Out}
	if e.In != nil && e.O != nil {
		f.Priced, f.Input, f.Output = true, *e.In, *e.O
		f.CacheRead, f.CacheWrite = f.Input, f.Input
		if e.CacheRead != nil {
			f.CacheRead = *e.CacheRead
		}
		if e.CacheWrite != nil {
			f.CacheWrite = *e.CacheWrite
		}
	}
	return f, true
}

// Fetched is the day the snapshot was taken, as YYYY-MM-DD.
func Fetched() string { return load().Fetched }
