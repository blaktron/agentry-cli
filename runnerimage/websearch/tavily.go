package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Tavily's Search API: a provider a person may name for their own key. The
// registry's own provider stays Brave. Tavily's terms keep a right to use
// queries to train models, which the platform's own provider must not
// allow; a person who names Tavily chooses those terms for their own
// searches.
//
// The call is POST /search with the key as a bearer token. It asks for the
// basic depth (one credit a search), two chunks of each page, and for none
// of the paid or unscanned extras: no generated answer, no raw page
// content, no images, and no automatic parameters, which can raise the
// depth. Only results[] is read.
const (
	TavilyName           = "tavily"
	TavilyDefaultBaseURL = "https://api.tavily.com"
	tavilyPath           = "/search"
	// A basic result's content is its chunks, each up to 500 characters;
	// two fill the snippet's thousand, the default three would be cut.
	tavilyChunks = 2
)

// Tavily is the Tavily Search API client. It is safe for concurrent use.
type Tavily struct{ c *client }

// NewTavily builds the client, refusing a missing key.
func NewTavily(cfg ClientConfig) (*Tavily, error) {
	c, err := newClient(TavilyName, cfg, TavilyDefaultBaseURL, tavilyPath)
	if err != nil {
		return nil, err
	}
	return &Tavily{c: c}, nil
}

// Name implements Provider.
func (*Tavily) Name() string { return TavilyName }

// Search implements Provider.
func (t *Tavily) Search(ctx context.Context, query string, count int) ([]Result, error) {
	raw, err := t.c.postJSON(ctx, map[string]any{
		"query":               query,
		"max_results":         ClampCount(count),
		"search_depth":        "basic",
		"chunks_per_source":   tavilyChunks,
		"topic":               "general",
		"include_answer":      false,
		"include_raw_content": false,
		"include_images":      false,
		"auto_parameters":     false,
	}, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+t.c.apiKey) })
	if err != nil {
		return nil, err
	}
	var body struct {
		Results []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("websearch: tavily response is not the result shape: %w", err)
	}
	out := make([]Result, 0, len(body.Results))
	for _, r := range body.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}
