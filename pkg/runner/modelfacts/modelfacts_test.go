package modelfacts

import (
	"encoding/json"
	"regexp"
	"testing"
)

func TestTheCatalogueIsWellFormed(t *testing.T) {
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("catalog.json: %v", err)
	}
	if c.Source != "https://models.dev/api.json" || !regexp.MustCompile(`^\d{4}-\d\d-\d\d$`).MatchString(c.Fetched) {
		t.Errorf("source %q, fetched %q", c.Source, c.Fetched)
	}
	if Fetched() != c.Fetched {
		t.Errorf("Fetched() = %q", Fetched())
	}
	for _, p := range []string{"anthropic", "google", "huggingface", "openai", "openrouter", "poe", "vultr"} {
		if len(c.Providers[p]) == 0 {
			t.Errorf("no models for the provider %s", p)
		}
	}
	if len(c.Providers) != 7 {
		t.Errorf("%d providers, want the seven a policy can name", len(c.Providers))
	}
	for p, models := range c.Providers {
		for id, e := range models {
			// The list is its contributors' and has entries whose output
			// limit is larger than their window; only what the runner
			// relies on is held here.
			if e.Ctx <= 0 || e.Out < 0 {
				t.Errorf("%s/%s: window %d, output %d", p, id, e.Ctx, e.Out)
			}
			if (e.In == nil) != (e.O == nil) {
				t.Errorf("%s/%s: half a price", p, id)
			}
			for _, v := range []*float64{e.In, e.O, e.CacheRead, e.CacheWrite} {
				if v != nil && (*v < 0 || *v > 10000) {
					t.Errorf("%s/%s: a price of %v dollars a million tokens", p, id, *v)
				}
			}
		}
	}
}

func TestLookup(t *testing.T) {
	// Any priced Anthropic model: the catalogue changes with every
	// refresh, so the test picks one it holds.
	var id string
	for m, e := range load().Providers["anthropic"] {
		if e.In != nil && e.CacheRead != nil && e.CacheWrite != nil && e.Out > 0 {
			id = m
			break
		}
	}
	if id == "" {
		t.Fatal("the catalogue holds no priced Anthropic model with cache prices")
	}
	f, ok := Lookup("anthropic", id)
	if !ok || !f.Priced || f.ContextWindow <= 0 || f.MaxOutput <= 0 || f.Input <= 0 || f.Output <= 0 {
		t.Fatalf("%s: %+v, %v", id, f, ok)
	}
	if f.CacheRead > f.Input || f.CacheWrite < f.Input {
		t.Errorf("%s: cache read %v and write %v against fresh input %v", id, f.CacheRead, f.CacheWrite, f.Input)
	}

	for _, miss := range [][2]string{{"anthropic", "no-such-model"}, {"custom", id}, {"", ""}} {
		if f, ok := Lookup(miss[0], miss[1]); ok || f != (Facts{}) {
			t.Errorf("Lookup(%q, %q) = %+v, %v", miss[0], miss[1], f, ok)
		}
	}
}

func TestAPriceNotListedIsNotZero(t *testing.T) {
	raw := []byte(`{"providers":{"p":{"free":{"ctx":1000,"out":100},"flat":{"ctx":1000,"out":100,"in":2,"o":8}}}}`)
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if e := c.Providers["p"]["free"]; e.In != nil || e.O != nil {
		t.Errorf("a model with no price listed reads as priced: %+v", e)
	}
	// A model with no cache prices listed is charged the fresh price for
	// cached input, never nothing.
	e := c.Providers["p"]["flat"]
	if e.CacheRead != nil || e.CacheWrite != nil || *e.In != 2 {
		t.Errorf("flat: %+v", e)
	}
}
