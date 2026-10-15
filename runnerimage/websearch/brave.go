package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Brave's LLM Context API: the registry's own provider, a search whose
// results are page chunks extracted for a model to read, in Brave's plain
// Search plan. Brave's published terms want a plan with storage rights to
// store results "in part or whole". The route stores none; whether what a
// draft keeps of its sources counts is an open question.
//
// The call is GET /res/v1/llm/context with the key in
// X-Subscription-Token. Brave considers its default pool of results and
// returns at most count URLs, within a small token budget with a per-URL
// cap, so one page cannot take all of it: every result is scanned before a
// model reads it, and Clean cuts a snippet to a thousand characters
// anyway. Only grounding.generic is read, each result's snippets joined
// into its snippet; local results (grounding.poi, grounding.map, never
// asked for: enable_local is off) and the sources metadata map are
// ignored.
const (
	BraveName           = "brave"
	BraveDefaultBaseURL = "https://api.search.brave.com"
	bravePath           = "/res/v1/llm/context"
	braveTokenBudget    = 2048
	braveTokensPerURL   = 512
	braveSnippetJoin    = " … "
	// Brave refuses a query of more than 75 words or 600 characters (422).
	// The route admits 400 characters, which can be more than 75 words.
	braveMaxWords = 75
	braveMaxBytes = 600
)

// Brave is the Brave LLM Context API client. It is safe for concurrent use.
type Brave struct{ c *client }

// BraveConfig builds a Brave client.
type BraveConfig = ClientConfig

// NewBrave builds the client, refusing a missing key.
func NewBrave(cfg BraveConfig) (*Brave, error) {
	c, err := newClient(BraveName, cfg, BraveDefaultBaseURL, bravePath)
	if err != nil {
		return nil, err
	}
	return &Brave{c: c}, nil
}

// Name implements Provider.
func (*Brave) Name() string { return BraveName }

// Search implements Provider.
func (b *Brave) Search(ctx context.Context, query string, count int) ([]Result, error) {
	q := url.Values{
		"q":                                {braveQuery(query)},
		"maximum_number_of_urls":           {strconv.Itoa(ClampCount(count))},
		"maximum_number_of_tokens":         {strconv.Itoa(braveTokenBudget)},
		"maximum_number_of_tokens_per_url": {strconv.Itoa(braveTokensPerURL)},
		"enable_local":                     {"false"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.c.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("websearch: brave request: %w", redactKey(err, b.c.apiKey))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.c.apiKey)
	raw, err := b.c.do(req)
	if err != nil {
		return nil, err
	}
	var body struct {
		Grounding struct {
			Generic []struct {
				URL      string   `json:"url"`
				Title    string   `json:"title"`
				Snippets []string `json:"snippets"`
			} `json:"generic"`
		} `json:"grounding"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("websearch: brave response is not the result shape: %w", err)
	}
	out := make([]Result, 0, len(body.Grounding.Generic))
	for _, g := range body.Grounding.Generic {
		out = append(out, Result{Title: g.Title, URL: g.URL, Snippet: strings.Join(g.Snippets, braveSnippetJoin)})
	}
	return out, nil
}

// braveQuery holds a query the route admitted to Brave's own limits: its
// first 75 words, and at most 600 bytes cut at a character (Brave counts
// characters; bytes are the stricter reading of a limit it does not define
// further). A longer query is searched on its opening words, not refused.
func braveQuery(q string) string {
	words := strings.Fields(q)
	if len(words) > braveMaxWords {
		words = words[:braveMaxWords]
	}
	s := strings.Join(words, " ")
	if len(s) <= braveMaxBytes {
		return s
	}
	cut := braveMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
