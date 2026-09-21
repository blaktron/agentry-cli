package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A fake provider endpoint: records the request headers and answers the
// shape its provider uses.
type fakeModels struct {
	t       *testing.T
	headers []http.Header
	paths   []string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeModels(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*fakeModels, *httptest.Server) {
	t.Helper()
	f := &fakeModels{t: t, handler: handler}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.RequestURI())
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// pointProviderAt makes a table provider's fixed endpoint the test server.
func pointProviderAt(t *testing.T, provider, url string) {
	t.Helper()
	d := DefaultsFor(provider)
	if d == nil {
		t.Fatalf("no table row for %s", provider)
	}
	old := d.ModelsURL
	d.ModelsURL = url
	t.Cleanup(func() { d.ModelsURL = old })
}

func ids(c *Catalog) []string {
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.ID)
	}
	return out
}

func TestListModelsAnthropicPaginatesAndUsesTheAPIKeyHeader(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("after_id") {
		case "":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "claude-opus-5", "display_name": "Claude Opus 5", "type": "model"}, {"id": "claude-sonnet-5", "display_name": "Claude Sonnet 5", "type": "model"}}, "has_more": true, "last_id": "claude-sonnet-5"})
		case "claude-sonnet-5":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "claude-haiku-4-5", "display_name": "Claude Haiku 4.5", "type": "model"}}, "has_more": false, "last_id": "claude-haiku-4-5"})
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	})
	pointProviderAt(t, "anthropic", srv.URL+"/v1/models")
	c, err := ListModels(context.Background(), "anthropic", "", "sk-ant-api03-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "claude-opus-5,claude-sonnet-5,claude-haiku-4-5" {
		t.Fatalf("ids = %s", got)
	}
	if c.Models[0].DisplayName != "Claude Opus 5" || c.Provider != "anthropic" {
		t.Fatalf("catalog = %+v", c)
	}
	if len(f.headers) != 2 {
		t.Fatalf("requests = %d", len(f.headers))
	}
	for _, h := range f.headers {
		if h.Get("x-api-key") != "sk-ant-api03-secret" || h.Get("anthropic-version") == "" || h.Get("Authorization") != "" || h.Get("anthropic-beta") != "" {
			t.Errorf("headers = %v", h)
		}
	}
	if !strings.Contains(f.paths[1], "after_id=claude-sonnet-5") {
		t.Errorf("second page = %s", f.paths[1])
	}
}

func TestListModelsAnthropicSubscriptionTokenIsABearerWithTheOAuthBeta(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "claude-opus-5", "display_name": "Claude Opus 5"}}, "has_more": false})
	})
	pointProviderAt(t, "anthropic", srv.URL+"/v1/models")
	if _, err := ListModels(context.Background(), "anthropic", "", "sk-ant-oat01-secret"); err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	if h.Get("Authorization") != "Bearer sk-ant-oat01-secret" || h.Get("anthropic-beta") != "oauth-2025-04-20" || h.Get("x-api-key") != "" {
		t.Errorf("headers = %v", h)
	}
}

func TestListModelsOpenAIIsABearer(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{{"id": "gpt-5.2", "object": "model"}, {"id": "gpt-5.2-mini", "object": "model"}}})
	})
	pointProviderAt(t, "openai", srv.URL+"/v1/models")
	c, err := ListModels(context.Background(), "openai", "", "sk-proj-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "gpt-5.2,gpt-5.2-mini" {
		t.Fatalf("ids = %s", got)
	}
	if f.headers[0].Get("Authorization") != "Bearer sk-proj-secret" {
		t.Errorf("headers = %v", f.headers[0])
	}
}

func TestListModelsGoogleFiltersToGeneratingModelsAndPaginates(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pageToken") {
		case "":
			writeJSON(w, map[string]any{"models": []map[string]any{
				{"name": "models/gemini-3-pro", "displayName": "Gemini 3 Pro", "supportedGenerationMethods": []string{"generateContent", "countTokens"}},
				{"name": "models/embedding-001", "displayName": "Embedding", "supportedGenerationMethods": []string{"embedContent"}},
			}, "nextPageToken": "p2"})
		case "p2":
			writeJSON(w, map[string]any{"models": []map[string]any{
				{"name": "models/gemini-3-flash", "displayName": "Gemini 3 Flash", "supportedGenerationMethods": []string{"generateContent"}},
			}})
		}
	})
	pointProviderAt(t, "google", srv.URL+"/v1beta/models")
	c, err := ListModels(context.Background(), "google", "", "AIza-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "gemini-3-pro,gemini-3-flash" {
		t.Fatalf("ids = %s", got)
	}
	if c.Models[0].DisplayName != "Gemini 3 Pro" {
		t.Fatalf("display = %+v", c.Models[0])
	}
	if f.headers[0].Get("x-goog-api-key") != "AIza-secret" || f.headers[0].Get("Authorization") != "" {
		t.Errorf("headers = %v", f.headers[0])
	}
	if !strings.Contains(f.paths[1], "pageToken=p2") {
		t.Errorf("second page = %s", f.paths[1])
	}
}

func TestListModelsCustomIsTheOpenAIShapeAtTheBaseURL(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "qwen3.8-flash"}}})
	})
	c, err := ListModels(context.Background(), "custom", srv.URL+"/compatible-mode/v1/", "custom-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "qwen3.8-flash" {
		t.Fatalf("ids = %s", got)
	}
	if f.paths[0] != "/compatible-mode/v1/models" || f.headers[0].Get("Authorization") != "Bearer custom-secret" {
		t.Errorf("request = %s %v", f.paths[0], f.headers[0])
	}
	if _, err := ListModels(context.Background(), "custom", "", "k"); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("custom without a base URL: %v", err)
	}
}

func TestListModelsRefusesPlainHTTPOffLoopbackBeforeAnyRequest(t *testing.T) {
	_, err := ListModels(context.Background(), "custom", "http://llm.example.net/v1", "k")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http to a remote host: %v", err)
	}
	// The loopback exception is what the tests above rely on; a name
	// that resolves to loopback is not enough, the host must be one.
	if _, err := ListModels(context.Background(), "custom", "http://localhost:1/v1", "k"); err == nil || strings.Contains(err.Error(), "https") {
		t.Fatalf("localhost over http is allowed (the refusal must be the connection, not the scheme): %v", err)
	}
}

func TestListModelsErrorsNameTheProviderAndItsMessageNeverTheKey(t *testing.T) {
	_, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"}})
	})
	pointProviderAt(t, "anthropic", srv.URL+"/v1/models")
	_, err := ListModels(context.Background(), "anthropic", "", "sk-ant-api03-verysecret")
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	for _, want := range []string{"anthropic", "401", "invalid x-api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "verysecret") {
		t.Errorf("the key is in the error: %q", err)
	}
	// A body that is not JSON: the status line alone.
	_, srv2 := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream sk-ant-api03-verysecret</html>"))
	})
	pointProviderAt(t, "anthropic", srv2.URL+"/v1/models")
	_, err = ListModels(context.Background(), "anthropic", "", "sk-ant-api03-verysecret")
	if err == nil || !strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "verysecret") {
		t.Fatalf("non-JSON body: %v", err)
	}
}

// The client follows no redirect: a 30x would carry x-api-key or
// x-goog-api-key to any host and a bearer to any subdomain, an https-to-
// http hop included, past the https rule. The second server must see
// nothing, and the answer is the status the endpoint gave.
func TestListModelsFollowsNoRedirect(t *testing.T) {
	elsewhere, other := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "stolen"}}})
	})
	_, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/models", http.StatusFound)
	})
	pointProviderAt(t, "openai", srv.URL+"/v1/models")
	_, err := ListModels(context.Background(), "openai", "", "sk-proj-secret")
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect must be reported as the status it is: %v", err)
	}
	if len(elsewhere.headers) != 0 {
		t.Fatalf("the redirect target was asked, with %v", elsewhere.headers)
	}
}

// A provider's own message can echo the key (a custom endpoint may say
// anything); it is scrubbed before the error leaves.
func TestListModelsScrubsTheKeyFromTheProvidersMessage(t *testing.T) {
	_, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "key " + r.Header.Get("Authorization") + " is not valid"}})
	})
	_, err := ListModels(context.Background(), "custom", srv.URL+"/v1", "custom-verysecret")
	if err == nil || strings.Contains(err.Error(), "verysecret") || !strings.Contains(err.Error(), "<key>") {
		t.Fatalf("error = %v", err)
	}
}

// The page cap bounds an endpoint that never ends its listing.
func TestListModelsStopsAtThePageCap(t *testing.T) {
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "m-" + r.URL.Query().Get("after_id")}}, "has_more": true, "last_id": "again"})
	})
	pointProviderAt(t, "anthropic", srv.URL+"/v1/models")
	c, err := ListModels(context.Background(), "anthropic", "", "sk-ant-api03-k")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != modelsMaxPages || len(c.Models) != modelsMaxPages {
		t.Fatalf("pages = %d, models = %d", len(f.paths), len(c.Models))
	}
}

// Plain http off loopback is refused before any request: a transport
// that fails the test if used proves it.
func TestListModelsMakesNoRequestBeforeTheHTTPSRefusal(t *testing.T) {
	old := httpClient.Transport
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("a request was made to %s", r.URL)
		return nil, nil
	})
	t.Cleanup(func() { httpClient.Transport = old })
	if _, err := ListModels(context.Background(), "custom", "http://llm.example.net/v1", "k"); err == nil {
		t.Fatal("expected the https refusal")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestListModelsRefusesAnUnknownProviderAndAnEmptyKey(t *testing.T) {
	if _, err := ListModels(context.Background(), "nope", "", "k"); err == nil {
		t.Fatal("unknown provider")
	}
	if _, err := ListModels(context.Background(), "openai", "", " "); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("empty key: %v", err)
	}
}

func TestListModelsAnEmptyListingIsAnError(t *testing.T) {
	_, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{}})
	})
	pointProviderAt(t, "openai", srv.URL+"/v1/models")
	if _, err := ListModels(context.Background(), "openai", "", "k"); err == nil || !strings.Contains(err.Error(), "no models") {
		t.Fatalf("empty listing: %v", err)
	}
}
