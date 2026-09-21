package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Catalog is what a provider's models endpoint listed, in the provider's
// own order.
type Catalog struct {
	Provider string
	Models   []Model
}

// Model is one listed model: the id a policy names, and the name a
// person reads where the provider gives one.
type Model struct {
	ID          string
	DisplayName string
}

// anthropicVersion is the API version header every Anthropic request
// carries; anthropicOAuthBeta is the beta a subscription OAuth token
// needs, the way Claude Code sends it — unverified here against a real
// token (agentry-cli#170), reported as the endpoint's own answer.
const (
	anthropicVersion   = "2023-06-01"
	anthropicOAuthBeta = "oauth-2025-04-20"
)

// Bounds: one listing is a few requests at most, and a hung endpoint is
// reported, not waited on.
const (
	modelsRequestTimeout = 20 * time.Second
	modelsMaxPages       = 10
	modelsMaxBody        = 4 << 20
	modelsMessageMax     = 200
)

// httpClient carries the key, so it follows no redirect: Go would keep
// x-api-key and x-goog-api-key across a redirect to any host, and a
// bearer across one to a subdomain — an https-to-http hop included — and
// a listing endpoint has no honest reason to redirect. A 30x is reported
// as the status it is.
var httpClient = &http.Client{
	Timeout: modelsRequestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ListModels asks provider's models endpoint with key and returns what
// it listed. baseURL is required for custom and ignored for the table
// providers. Plain http is refused for any host but loopback before a
// request is made; no redirect is followed; the key is scrubbed from any
// error, the provider's own message included. An empty listing is an
// error: a form offering nothing is worse than one saying so.
func ListModels(ctx context.Context, provider, baseURL, key string) (*Catalog, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	key = strings.TrimSpace(key)
	cat, err := listModels(ctx, provider, baseURL, key)
	if err != nil {
		return nil, fmt.Errorf("%s: list models: %w", provider, withoutKey(err, key))
	}
	return cat, nil
}

func listModels(ctx context.Context, provider, baseURL, key string) (*Catalog, error) {
	if key == "" {
		return nil, errors.New("no key to ask with")
	}
	endpoint, pg, err := modelsEndpoint(provider, baseURL)
	if err != nil {
		return nil, err
	}
	if err := requireHTTPS(endpoint); err != nil {
		return nil, err
	}
	cat := &Catalog{Provider: provider}
	cursor := ""
	for n := 0; n < modelsMaxPages; n++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pg.url(endpoint, cursor), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		pg.auth(req, key)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxBody))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the answer: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s%s", resp.Status, providerMessage(body))
		}
		models, next, err := pg.decode(body)
		if err != nil {
			return nil, err
		}
		cat.Models = append(cat.Models, models...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(cat.Models) == 0 {
		return nil, errors.New("the endpoint listed no models a run can use")
	}
	return cat, nil
}

// modelsEndpoint is the listing URL and the shape behind it: the table
// row's for a table provider, <baseURL>/models with the OpenAI shape for
// custom.
func modelsEndpoint(provider, baseURL string) (string, *pager, error) {
	if provider == "custom" {
		base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
		if base == "" {
			return "", nil, errors.New("the base URL is required (the endpoint the openai client is pointed at)")
		}
		return base + "/models", openAIPager(keyHeaderBearer), nil
	}
	d := DefaultsFor(provider)
	if d == nil {
		return "", nil, fmt.Errorf("no such provider (%s)", strings.Join(Providers(), ", "))
	}
	switch provider {
	case "anthropic":
		return d.ModelsURL, anthropicPager(d.KeyHeader), nil
	case "google":
		return d.ModelsURL, googlePager(d.KeyHeader), nil
	default:
		return d.ModelsURL, openAIPager(d.KeyHeader), nil
	}
}

// requireHTTPS refuses a plain-http endpoint on any host but loopback:
// the key rides the request, and a host on the operator's own machine is
// the one place it may ride in clear (agentry-desktop#123).
func requireHTTPS(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL with a host", endpoint)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%s is plain http off this machine — the key would travel in clear; use https", u.Redacted())
	default:
		return fmt.Errorf("%q: the scheme must be https", endpoint)
	}
}

// pager is one listing shape: how the URL carries the page cursor, how
// the key is sent, how a page decodes.
type pager struct {
	url    func(endpoint, cursor string) string
	auth   func(req *http.Request, key string)
	decode func(body []byte) (models []Model, next string, err error)
}

// setKey puts the key in the header the table names.
func setKey(req *http.Request, header, key string) {
	if header == keyHeaderBearer {
		req.Header.Set(header, "Bearer "+key)
		return
	}
	req.Header.Set(header, key)
}

func errNotAListing(err error) error {
	return fmt.Errorf("the answer is not the models listing: %w", err)
}

// anthropicPager: data[].id/display_name, has_more/last_id → after_id,
// limit at its maximum; an API key in x-api-key, a subscription OAuth
// token as a bearer with the beta that admits one.
func anthropicPager(keyHeader string) *pager {
	return &pager{
		url: func(endpoint, cursor string) string {
			q := url.Values{"limit": {"1000"}}
			if cursor != "" {
				q.Set("after_id", cursor)
			}
			return endpoint + "?" + q.Encode()
		},
		auth: func(req *http.Request, key string) {
			req.Header.Set("anthropic-version", anthropicVersion)
			if policy.CredentialKindOf(policy.ProviderAnthropic, key) == policy.KeyKindSubscription {
				setKey(req, keyHeaderBearer, key)
				req.Header.Set("anthropic-beta", anthropicOAuthBeta)
				return
			}
			setKey(req, keyHeader, key)
		},
		decode: func(body []byte) ([]Model, string, error) {
			var page struct {
				Data []struct {
					ID          string `json:"id"`
					DisplayName string `json:"display_name"`
				} `json:"data"`
				HasMore bool   `json:"has_more"`
				LastID  string `json:"last_id"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, "", errNotAListing(err)
			}
			out := make([]Model, 0, len(page.Data))
			for _, m := range page.Data {
				out = append(out, Model{ID: m.ID, DisplayName: m.DisplayName})
			}
			if page.HasMore && page.LastID != "" {
				return out, page.LastID, nil
			}
			return out, "", nil
		},
	}
}

// openAIPager: data[].id, one page, the key as the table says.
func openAIPager(keyHeader string) *pager {
	return &pager{
		url:  func(endpoint, _ string) string { return endpoint },
		auth: func(req *http.Request, key string) { setKey(req, keyHeader, key) },
		decode: func(body []byte) ([]Model, string, error) {
			var page struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, "", errNotAListing(err)
			}
			out := make([]Model, 0, len(page.Data))
			for _, m := range page.Data {
				out = append(out, Model{ID: m.ID})
			}
			return out, "", nil
		},
	}
}

// googlePager: models[].name ("models/<id>"), displayName,
// supportedGenerationMethods filtered to those that answer a chat
// (embeddings and the like are listed beside them), pageSize/pageToken/
// nextPageToken.
func googlePager(keyHeader string) *pager {
	return &pager{
		url: func(endpoint, cursor string) string {
			q := url.Values{"pageSize": {"1000"}}
			if cursor != "" {
				q.Set("pageToken", cursor)
			}
			return endpoint + "?" + q.Encode()
		},
		auth: func(req *http.Request, key string) { setKey(req, keyHeader, key) },
		decode: func(body []byte) ([]Model, string, error) {
			var page struct {
				Models []struct {
					Name        string   `json:"name"`
					DisplayName string   `json:"displayName"`
					Methods     []string `json:"supportedGenerationMethods"`
				} `json:"models"`
				NextPageToken string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, "", errNotAListing(err)
			}
			var out []Model
			for _, m := range page.Models {
				generates := false
				for _, method := range m.Methods {
					if method == "generateContent" {
						generates = true
						break
					}
				}
				if !generates {
					continue
				}
				out = append(out, Model{ID: strings.TrimPrefix(m.Name, "models/"), DisplayName: m.DisplayName})
			}
			return out, page.NextPageToken, nil
		},
	}
}

// providerMessage is the provider's own sentence for a refusal, as the
// three shapes carry it ({"error":{"message":…}}), bounded; nothing of a
// body that is not that shape, which may be anything.
func providerMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || strings.TrimSpace(e.Error.Message) == "" {
		return ""
	}
	msg := strings.TrimSpace(e.Error.Message)
	if len(msg) > modelsMessageMax {
		cut := modelsMessageMax
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return ": " + msg
}

// withoutKey scrubs the key from an error before it leaves: a provider's
// message can echo it (a custom endpoint may say anything), a proxy's
// text could, and the wording of a transport error is not ours.
func withoutKey(err error, key string) error {
	if key == "" {
		return err
	}
	if s := err.Error(); strings.Contains(s, key) {
		return errors.New(strings.ReplaceAll(s, key, "<key>"))
	}
	return err
}
