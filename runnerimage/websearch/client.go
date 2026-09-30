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

// What every provider's client shares: its configuration, the HTTP client
// that follows no redirect, the call that reads a bounded answer, and the
// errors that never carry the key. A provider adds only its request and
// the shape of its answer.

// ClientConfig builds a provider's client.
type ClientConfig struct {
	// BaseURL is the API root; empty is the provider's own.
	BaseURL string
	// APIKey is required, never logged, and redacted from every error.
	APIKey string
	// HTTPClient carries the requests; nil is a client with a 20-second
	// timeout.
	HTTPClient *http.Client
}

// ErrNoKey is a provider built without its key.
var ErrNoKey = errors.New("websearch: a search provider key is required")

// maxResponse is the most of an answer read: two MiB, far past what ten
// results with bounded snippets need.
const maxResponse = 2 << 20

// client is the part of a provider's client every provider shares.
type client struct {
	provider string
	endpoint string
	apiKey   string
	http     *http.Client
}

// newClient checks the key and builds the HTTP client. A redirect is never
// followed: each provider sends its key in a header, and on a redirect Go
// carries a custom header (X-API-Key, X-Subscription-Token, x-api-key) to
// any other host, and Authorization to one in the same domain, so following
// one could hand the key there. A 3xx is then a
// non-200 answer, and an error.
func newClient(provider string, cfg ClientConfig, defaultBase, path string) (*client, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, ErrNoKey
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBase
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{provider: provider, endpoint: base + path, apiKey: key, http: hc}, nil
}

// do sends req and reads at most maxResponse bytes of a 200 answer. Any other
// status is a StatusError; no error it returns carries the key.
func (c *client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("websearch: %s request: %w", c.provider, redactKey(err, c.apiKey))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("websearch: %s response: %w", c.provider, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(c.provider, resp.StatusCode, raw, c.apiKey)
	}
	return raw, nil
}

// postJSON sends payload as JSON to the endpoint with the key in the
// header auth sets, and reads the answer as do does. The key is never in the
// URL or the body.
func (c *client) postJSON(ctx context.Context, payload any, auth func(*http.Request)) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("websearch: %s payload: %w", c.provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("websearch: %s request: %w", c.provider, redactKey(err, c.apiKey))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	auth(req)
	return c.do(req)
}

// redactKey strips a provider's key from an error's text. An error that
// does not carry the key is returned as it is; one that does is replaced by
// a plain error with the same text redacted, so its wrapped chain is lost.
func redactKey(err error, key string) error {
	if !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
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

// statusError is a non-200 answer as a StatusError. The key is redacted
// before the body is cut, so no half of it survives the cut.
func statusError(provider string, status int, body []byte, key string) *StatusError {
	snippet := strings.ReplaceAll(string(body), key, "[key]")
	if len(snippet) > 200 {
		snippet = strings.ToValidUTF8(snippet[:200], "")
	}
	return &StatusError{Provider: provider, Status: status, Snippet: snippet}
}

// Providers lists the names a deployment may configure, "none" first.
func Providers() []string {
	return []string{"none", YouComName, BraveName, TavilyName, ExaName, PerplexityName}
}

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
// (nil, nil), "youcom" is You.com, "brave" is Brave's LLM Context API, and
// "tavily", "exa" and "perplexity" are those providers' search APIs.
// An unknown name, or a named provider without its key, is an error the
// server refuses to start on.
func New(provider, baseURL, apiKey string) (Provider, error) {
	cfg := ClientConfig{BaseURL: baseURL, APIKey: apiKey}
	switch strings.TrimSpace(provider) {
	case "", "none":
		return nil, nil
	case YouComName:
		return NewYouCom(cfg)
	case BraveName:
		return NewBrave(cfg)
	case TavilyName:
		return NewTavily(cfg)
	case ExaName:
		return NewExa(cfg)
	case PerplexityName:
		return NewPerplexity(cfg)
	}
	return nil, fmt.Errorf("websearch: unknown provider %q (%s)", provider, strings.Join(Providers(), "|"))
}
