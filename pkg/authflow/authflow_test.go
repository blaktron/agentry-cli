package authflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // loopback test URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHappyPathDeliversToken(t *testing.T) {
	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if !strings.Contains(f.AuthorizeURL("https://reg.example"), fmt.Sprintf("port=%d", f.Port)) {
		t.Fatalf("authorize URL misses the port: %s", f.AuthorizeURL("https://reg.example"))
	}

	resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=%s&token=agy_test", f.Port, f.State))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := f.Wait(ctx)
	if err != nil || tok != "agy_test" {
		t.Fatalf("Wait = %q, %v", tok, err)
	}
}

func TestStateMismatchIsRefused(t *testing.T) {
	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=WRONG&token=agy_evil", f.Port))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched state answered %d, want 403", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if tok, err := f.Wait(ctx); err == nil || tok != "" {
		t.Fatalf("Wait = (%q, %v) after a mismatched state, want no token and still waiting", tok, err)
	}
}

// TestBogusStateDoesNotEndTheFlow: a request without the nonce is refused
// and the real callback still completes the sign-in — anything local can
// reach the port, and must not be able to abort a sign-in (agentry-cli#164).
func TestBogusStateDoesNotEndTheFlow(t *testing.T) {
	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=WRONG&token=agy_evil", f.Port)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched state answered %d, want 403", resp.StatusCode)
	}
	if resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=%s&token=agy_real", f.Port, url.QueryEscape(f.State))); resp.StatusCode != http.StatusOK {
		t.Fatalf("the real callback answered %d, want 200", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if tok, err := f.Wait(ctx); err != nil || tok != "agy_real" {
		t.Fatalf("Wait = (%q, %v), want the real token", tok, err)
	}
}

func TestContextCancelUnblocksWait(t *testing.T) {
	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Wait(ctx); err == nil {
		t.Fatal("Wait returned no error on a canceled context")
	}
}

// The hardened shape (agentry#19): the callback carries a code, and the flow
// exchanges it against the registry it was started for, presenting the same
// state nonce. The browser sees success only after the exchange succeeded.
func TestCodeIsExchangedForToken(t *testing.T) {
	var gotState, gotCode string
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/desktop/exchange" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		var body struct{ Code, State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCode, gotState = body.Code, body.State
		if body.Code != "agd_one-time" {
			http.Error(w, `{"title":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"agy_minted-at-exchange","scopes":["user:profile"]}`))
	}))
	defer registry.Close()

	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = f.AuthorizeURL(registry.URL + "/")

	resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=%s&code=agd_one-time", f.Port, f.State))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := f.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token != "agy_minted-at-exchange" {
		t.Fatalf("token = %q", token)
	}
	if gotCode != "agd_one-time" || gotState != f.State {
		t.Fatalf("exchange sent code=%q state=%q; want the callback's code and this flow's state", gotCode, gotState)
	}
}

// A refused exchange is an error to the app and a failure page to the
// browser — never a silent success.
func TestRefusedExchangeFails(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"title":"forbidden"}`, http.StatusForbidden)
	}))
	defer registry.Close()
	f, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = f.AuthorizeURL(registry.URL)
	resp := get(t, fmt.Sprintf("http://127.0.0.1:%d/callback?state=%s&code=agd_stale", f.Port, f.State))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("callback status %d want 502", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.Wait(ctx); err == nil {
		t.Fatal("refused exchange reported success")
	}
}
