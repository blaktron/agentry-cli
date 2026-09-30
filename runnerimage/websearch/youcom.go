package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// You.com's Web Search API (agentry#608): the provider the registry
// brokers, chosen on its published terms — a plain REST call with a key,
// the online MSA's "only use Customer Data as necessary to provide … the
// Services" and no training on queries, outputs assigned to the customer,
// no display or attribution rule. The research and the one condition it
// left (written confirmation of which contract governs an API-key account)
// are on agentry#608 and the operator's agentry-web#466. Since agentry#631
// dev and production use Brave instead (DEV-157); this client stays a
// provider a deployment may name.
//
// The call is POST /v1/search with the key in X-API-Key. Only the `web`
// results are read; `news` is ignored. Neither `extraction` (full page
// text, billed per page) nor `knowledge` (results that carry a display
// credit) is ever asked for, and the thumbnail and favicon addresses a
// result carries are dropped, never fetched.
const (
	YouComName           = "youcom"
	YouComDefaultBaseURL = "https://ydc-index.io"
	youComPath           = "/v1/search"
)

// YouCom is the You.com Web Search API client. It is safe for concurrent
// use.
type YouCom struct{ c *client }

// YouComConfig builds a YouCom client.
type YouComConfig = ClientConfig

// NewYouCom builds the client, refusing a missing key.
func NewYouCom(cfg YouComConfig) (*YouCom, error) {
	c, err := newClient(YouComName, cfg, YouComDefaultBaseURL, youComPath)
	if err != nil {
		return nil, err
	}
	return &YouCom{c: c}, nil
}

// Name implements Provider.
func (*YouCom) Name() string { return YouComName }

// Search implements Provider.
func (y *YouCom) Search(ctx context.Context, query string, count int) ([]Result, error) {
	raw, err := y.c.postJSON(ctx, map[string]any{"query": query, "count": ClampCount(count)},
		func(r *http.Request) { r.Header.Set("X-API-Key", y.c.apiKey) })
	if err != nil {
		return nil, err
	}
	var body struct {
		Results struct {
			Web []struct {
				URL         string   `json:"url"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				Snippets    []string `json:"snippets"`
			} `json:"web"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("websearch: youcom response is not the result shape: %w", err)
	}
	out := make([]Result, 0, len(body.Results.Web))
	for _, w := range body.Results.Web {
		snippet := w.Description
		if strings.TrimSpace(snippet) == "" && len(w.Snippets) > 0 {
			snippet = w.Snippets[0]
		}
		out = append(out, Result{Title: w.Title, URL: w.URL, Snippet: snippet})
	}
	return out, nil
}
