package mcpauthtest

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// ChatGPT is a fake of OpenAI's "Sign in with ChatGPT" plan usage, as its
// pages describe it (pkg/mcpauth/chatgpt.go): the first authorization with
// dynamic_agent_client registers a client and the redirect carries its id;
// the token endpoint exchanges codes and rotates refresh tokens for that
// client alone; the discovery document names the revocation endpoint; and
// /v1/models lists the plan's models, or answers 403
// subscription_sharing_user_not_eligible. Fields set before the first
// request shape it.
type ChatGPT struct {
	*httptest.Server

	// Subject and Email are the account the ID token names.
	Subject, Email string
	// NoPlanScope leaves chatgpt.tokens.use.direct out of the granted scopes.
	NoPlanScope bool
	// NotEligible answers the model list with OpenAI's 403.
	NotEligible bool
	// Refused refuses every refresh with RefuseCode (invalid_grant when "").
	Refused    bool
	RefuseCode string
	// IssuedClient is the client id a registration issues.
	IssuedClient string
	// AccessPrefix and RefreshPrefix start every token issued, so a test can
	// search for them.
	AccessPrefix, RefreshPrefix string
	// RevocationURL, when set, is the revocation endpoint the discovery
	// document names instead of the fake's own; NoExpiry leaves exp out
	// of the ID token.
	RevocationURL string
	NoExpiry      bool

	mu sync.Mutex
	// Authorizations is every authorization request's query.
	Authorizations []url.Values
	// Requests is every token-endpoint form; Revocations every revocation's.
	Requests, Revocations []url.Values
	// Issued is every refresh token issued; ModelCalls every model list's
	// Authorization header.
	Issued, ModelCalls []string
	codes              map[string]chatgptCode
	refresh            map[string]string // live refresh token → client id
	clients            map[string]bool   // every client id registered
	seq                int
}

type chatgptCode struct{ client, redirect, challenge, nonce string }

// NewChatGPT starts the fake.
func NewChatGPT(t *testing.T) *ChatGPT {
	t.Helper()
	v := &ChatGPT{
		Subject: "user-fixture", Email: "person@example.com", IssuedClient: "oaiapp_fixture",
		AccessPrefix: "PROBE-ACCESS-", RefreshPrefix: "PROBE-REFRESH-",
		codes: map[string]chatgptCode{}, refresh: map[string]string{}, clients: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/authorize", v.authorize)
	mux.HandleFunc("/api/accounts/oauth/token", v.token)
	mux.HandleFunc("/.well-known/openid-configuration", v.discovery)
	mux.HandleFunc("/oauth/revoke", v.revoke)
	mux.HandleFunc("/v1/models", v.models)
	v.Server = httptest.NewTLSServer(mux)
	t.Cleanup(v.Close)
	return v
}

// Flow is a mcpauth.ChatGPT pointed at the fake, with a browser that
// approves.
func (v *ChatGPT) Flow() mcpauth.ChatGPT {
	pool := x509.NewCertPool()
	pool.AddCert(v.Certificate())
	g := mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	c := v.Client()
	return mcpauth.ChatGPT{
		HTTP: g.Client(), Issuer: v.URL,
		AuthorizeURL: v.URL + "/api/accounts/authorize", TokenURL: v.URL + "/api/accounts/oauth/token",
		DiscoveryURL: v.URL + "/.well-known/openid-configuration", APIBase: v.URL + "/v1",
		Present: func(authURL string) {
			go func() {
				if res, err := c.Get(authURL); err == nil {
					_ = res.Body.Close()
				}
			}()
		},
		Timeout: 10 * time.Second,
	}
}

// Live reports whether rt is a refresh token the fake would still honour.
func (v *ChatGPT) Live(rt string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.refresh[rt]
	return ok
}

func (v *ChatGPT) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v.mu.Lock()
	v.Authorizations = append(v.Authorizations, q)
	v.mu.Unlock()
	redirect := q.Get("redirect_uri")
	u, err := url.Parse(redirect)
	switch {
	case err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/auth/callback":
		http.Error(w, "redirect_uri must be http://127.0.0.1:<port>/auth/callback", http.StatusBadRequest)
		return
	case q.Get("response_type") != "code", q.Get("code_challenge_method") != "S256", q.Get("code_challenge") == "",
		q.Get("state") == "", q.Get("nonce") == "", q.Get("ext_agent_host_id") == "",
		q.Get("resource") != mcpauth.ChatGPTResource:
		http.Error(w, "missing or wrong authorization parameter", http.StatusBadRequest)
		return
	}
	client := q.Get("client_id")
	back := url.Values{"state": {q.Get("state")}}
	switch client {
	case mcpauth.ChatGPTDynamicClient:
		if q.Get("agent_name_hint") == "" {
			http.Error(w, "a registration needs agent_name_hint", http.StatusBadRequest)
			return
		}
		client = v.IssuedClient
		v.mu.Lock()
		v.clients[client] = true
		v.mu.Unlock()
		back.Set("client_id", client)
	default:
		v.mu.Lock()
		known := v.clients[client]
		v.mu.Unlock()
		if !known {
			http.Error(w, "unknown client", http.StatusBadRequest)
			return
		}
		if q.Get("agent_name_hint") != "" {
			http.Error(w, "agent_name_hint is for a registration only", http.StatusBadRequest)
			return
		}
	}
	code := v.next("code-")
	v.mu.Lock()
	v.codes[code] = chatgptCode{client: client, redirect: redirect, challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	v.mu.Unlock()
	back.Set("code", code)
	back.Set("scope", q.Get("scope"))
	http.Redirect(w, r, redirect+"?"+back.Encode(), http.StatusFound)
}

func (v *ChatGPT) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	v.mu.Lock()
	v.Requests = append(v.Requests, f)
	v.mu.Unlock()
	if f.Get("resource") != mcpauth.ChatGPTResource {
		oauthErr(w, "invalid_target")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		v.mu.Lock()
		c, ok := v.codes[f.Get("code")]
		delete(v.codes, f.Get("code"))
		v.mu.Unlock()
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || c.client != f.Get("client_id") || c.redirect != f.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			oauthErr(w, "invalid_grant")
			return
		}
		v.issue(w, c.client, c.nonce)
	case "refresh_token":
		if f.Get("scope") != "" {
			oauthErr(w, "invalid_scope")
			return
		}
		v.mu.Lock()
		client, ok := v.refresh[f.Get("refresh_token")]
		delete(v.refresh, f.Get("refresh_token"))
		v.mu.Unlock()
		if v.Refused || !ok || client != f.Get("client_id") {
			code := v.RefuseCode
			if code == "" {
				code = "invalid_grant"
			}
			oauthErr(w, code)
			return
		}
		v.issue(w, client, "")
	default:
		oauthErr(w, "unsupported_grant_type")
	}
}

func (v *ChatGPT) issue(w http.ResponseWriter, client, nonce string) {
	access, refresh := v.next(v.AccessPrefix), v.next(v.RefreshPrefix)
	v.mu.Lock()
	v.refresh[refresh] = client
	v.Issued = append(v.Issued, refresh)
	v.mu.Unlock()
	scope := "chatgpt.tokens.use.direct email offline_access openid profile resource.invoke"
	if v.NoPlanScope {
		scope = "email offline_access openid profile resource.invoke"
	}
	c := map[string]any{"iss": v.URL, "aud": []string{client}, "sub": v.Subject, "email": v.Email, "nonce": nonce}
	if !v.NoExpiry {
		c["exp"] = time.Now().Add(time.Hour).Unix()
	}
	claims, _ := json.Marshal(c)
	idToken := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
	writeJSON(w, map[string]any{
		"access_token": access, "refresh_token": refresh, "id_token": idToken,
		"token_type": "Bearer", "expires_in": 3600, "scope": scope,
	})
}

func (v *ChatGPT) discovery(w http.ResponseWriter, _ *http.Request) {
	revoke := v.URL + "/oauth/revoke"
	if v.RevocationURL != "" {
		revoke = v.RevocationURL
	}
	writeJSON(w, map[string]any{
		"issuer": v.URL, "authorization_endpoint": v.URL + "/api/accounts/authorize",
		"token_endpoint": v.URL + "/api/accounts/oauth/token", "revocation_endpoint": revoke,
	})
}

func (v *ChatGPT) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	v.mu.Lock()
	v.Revocations = append(v.Revocations, f)
	if f.Get("token_type_hint") == "refresh_token" && v.refresh[f.Get("token")] == f.Get("client_id") {
		delete(v.refresh, f.Get("token"))
	}
	v.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (v *ChatGPT) models(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.ModelCalls = append(v.ModelCalls, r.Header.Get("Authorization"))
	v.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+v.AccessPrefix) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if v.NotEligible {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_user_not_eligible","message":"This account is not eligible for ChatGPT plan usage in this app.","param":null}}`))
		return
	}
	writeJSON(w, map[string]any{"models": []map[string]any{
		{"slug": "gpt-fixture", "visibility": "list"},
		{"slug": "gpt-fixture-mini", "visibility": "list"},
		{"slug": "gpt-internal", "visibility": "hide"},
	}})
}

func (v *ChatGPT) next(prefix string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seq++
	return fmt.Sprintf("%s%d-%s", prefix, v.seq, random())
}
