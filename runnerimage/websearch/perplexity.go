package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Perplexity's Search API: the raw results endpoint, not its chat
// completions, and a provider a person may name for their own key. The
// registry's own provider stays Brave. Perplexity's published no-training
// statement covers its chat completions, not this endpoint, so the
// platform provider's no-training condition is not shown to hold for it; a
// person who names Perplexity chooses its terms for their own searches.
//
// The call is POST /search with the key as a bearer token, one query (an
// array would be billed as several), the standard web search type (not
// fast, cheaper and shallower: a draft is research), and a small per-page
// budget of extracted text, since Clean cuts a snippet to a thousand
// characters anyway. Only results[] is read.
const (
	PerplexityName           = "perplexity"
	PerplexityDefaultBaseURL = "https://api.perplexity.ai"
	perplexityPath           = "/search"
	perplexityTokensPerPage  = 256
)

// Perplexity is the Perplexity Search API client. It is safe for
// concurrent use.
type Perplexity struct{ c *client }

// NewPerplexity builds the client, refusing a missing key.
func NewPerplexity(cfg ClientConfig) (*Perplexity, error) {
	c, err := newClient(PerplexityName, cfg, PerplexityDefaultBaseURL, perplexityPath)
	if err != nil {
		return nil, err
	}
	return &Perplexity{c: c}, nil
}

// Name implements Provider.
func (*Perplexity) Name() string { return PerplexityName }

// Search implements Provider.
func (p *Perplexity) Search(ctx context.Context, query string, count int) ([]Result, error) {
	raw, err := p.c.postJSON(ctx, map[string]any{
		"query":               query,
		"max_results":         ClampCount(count),
		"search_type":         "web",
		"max_tokens_per_page": perplexityTokensPerPage,
	}, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.c.apiKey) })
	if err != nil {
		return nil, err
	}
	var body struct {
		Results []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("websearch: perplexity response is not the result shape: %w", err)
	}
	out := make([]Result, 0, len(body.Results))
	for _, r := range body.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out, nil
}
