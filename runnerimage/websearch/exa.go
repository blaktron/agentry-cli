package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Exa's search API: a provider a person may name for their own key. The
// registry's own provider stays Brave. Exa's terms license it to use
// queries to improve its products, which the platform's own provider must
// not allow; a person who names Exa chooses those terms for their own
// searches.
//
// The call is POST /search with the key in x-api-key. It asks for Exa's
// default search type, auto (not instant, cheaper and shallower: a draft
// is research), and each result's highlights, the passages most relevant
// to the query, cut by Exa to the snippet's own bound. Never a summary
// (billed per page), a deep search, full text or subpages, and never more
// than ten results, past which Exa bills extra. Only results[] is read;
// the cost, the images and the favicons are ignored.
const (
	ExaName           = "exa"
	ExaDefaultBaseURL = "https://api.exa.ai"
	exaPath           = "/search"
	exaSnippetJoin    = " … "
)

// Exa is the Exa search API client. It is safe for concurrent use.
type Exa struct{ c *client }

// NewExa builds the client, refusing a missing key.
func NewExa(cfg ClientConfig) (*Exa, error) {
	c, err := newClient(ExaName, cfg, ExaDefaultBaseURL, exaPath)
	if err != nil {
		return nil, err
	}
	return &Exa{c: c}, nil
}

// Name implements Provider.
func (*Exa) Name() string { return ExaName }

// Search implements Provider.
func (e *Exa) Search(ctx context.Context, query string, count int) ([]Result, error) {
	raw, err := e.c.postJSON(ctx, map[string]any{
		"query":      query,
		"type":       "auto",
		"numResults": ClampCount(count),
		"contents":   map[string]any{"highlights": map[string]any{"maxCharacters": maxSnippet}},
	}, func(r *http.Request) { r.Header.Set("x-api-key", e.c.apiKey) })
	if err != nil {
		return nil, err
	}
	var body struct {
		Results []struct {
			URL        string   `json:"url"`
			Title      string   `json:"title"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("websearch: exa response is not the result shape: %w", err)
	}
	out := make([]Result, 0, len(body.Results))
	for _, r := range body.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: strings.Join(r.Highlights, exaSnippetJoin)})
	}
	return out, nil
}
