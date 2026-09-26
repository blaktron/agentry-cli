package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// You.com's Web Search API (agentry#608): the provider the registry
// brokers, chosen on its published terms — a plain REST call with a key,
// the online MSA's "only use Customer Data as necessary to provide … the
// Services" and no training on queries, outputs assigned to the customer,
// no display or attribution rule. The research and the one condition it
// left (written confirmation of which contract governs an API-key account)
// are on agentry#608 and the operator's agentry-web#466.
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
	youComMaxResponse    = 2 << 20
)

// YouCom is the You.com Web Search API client. It is safe for concurrent
// use.
type YouCom struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// YouComConfig builds a YouCom client.
type YouComConfig struct {
	// BaseURL is the API root; empty is YouComDefaultBaseURL.
	BaseURL string
	// APIKey is required, never logged, and redacted from every error.
	APIKey string
	// HTTPClient carries the requests; nil is a client with a 20-second
	// timeout.
	HTTPClient *http.Client
}

// ErrNoKey is a provider built without its key.
var ErrNoKey = errors.New("websearch: a search provider key is required")

// NewYouCom builds the client, refusing a missing key.
func NewYouCom(cfg YouComConfig) (*YouCom, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, ErrNoKey
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = YouComDefaultBaseURL
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	// A redirect is never followed: Go drops Authorization on a cross-host
	// redirect but not X-API-Key, so following one could hand the key to
	// another host. A 3xx is then a non-200 answer, and an error.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &YouCom{endpoint: base + youComPath, apiKey: key, http: hc}, nil
}

// Name implements Provider.
func (*YouCom) Name() string { return YouComName }

// Search implements Provider.
func (y *YouCom) Search(ctx context.Context, query string, count int) ([]Result, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "count": ClampCount(count)})
	if err != nil {
		return nil, fmt.Errorf("websearch: youcom payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("websearch: youcom request: %w", y.redact(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", y.apiKey)
	resp, err := y.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("websearch: youcom request: %w", y.redact(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, youComMaxResponse))
	if err != nil {
		return nil, fmt.Errorf("websearch: youcom response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Redact before cutting, so no half of the key survives the cut.
		snippet := strings.ReplaceAll(string(raw), y.apiKey, "[key]")
		if len(snippet) > 200 {
			snippet = strings.ToValidUTF8(snippet[:200], "")
		}
		return nil, &StatusError{Provider: YouComName, Status: resp.StatusCode, Snippet: snippet}
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

func (y *YouCom) redact(err error) error {
	if !strings.Contains(err.Error(), y.apiKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), y.apiKey, "[key]"))
}

// StatusError is a provider's non-200 answer: its status and a bounded
// snippet of the body with the key redacted. The snippet can echo the
// query, so a caller that must not log queries logs Status alone.
type StatusError struct {
	Provider string
	Status   int
	Snippet  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("websearch: %s returned %d: %s", e.Provider, e.Status, e.Snippet)
}

// Providers lists the names a deployment may configure, "none" first.
func Providers() []string { return []string{"none", YouComName} }

// Known reports whether a configured name is one of Providers; "" is none.
func Known(provider string) bool {
	p := strings.TrimSpace(provider)
	if p == "" {
		return true
	}
	for _, v := range Providers() {
		if v == p {
			return true
		}
	}
	return false
}

// New builds the provider a deployment names: "" or "none" is no provider
// (nil, nil), "youcom" is You.com. An unknown name, or a named provider
// without its key, is an error the server refuses to start on.
func New(provider, baseURL, apiKey string) (Provider, error) {
	switch strings.TrimSpace(provider) {
	case "", "none":
		return nil, nil
	case YouComName:
		return NewYouCom(YouComConfig{BaseURL: baseURL, APIKey: apiKey})
	}
	return nil, fmt.Errorf("websearch: unknown provider %q (%s)", provider, strings.Join(Providers(), "|"))
}
