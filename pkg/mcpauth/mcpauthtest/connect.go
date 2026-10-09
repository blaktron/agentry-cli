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

// keyVendor is what the three connect fakes share: a TLS server, the codes
// it issued, and every key or token it handed out, so a test can search
// for them.
type keyVendor struct {
	*httptest.Server

	mu sync.Mutex
	// Authorizations is every authorization request's query.
	Authorizations []url.Values
	// Issued is every key or token handed out.
	Issued []string
	codes  map[string]keyCode
	seq    int
}

type keyCode struct{ client, redirect, challenge string }

func newKeyVendor(t *testing.T, mux *http.ServeMux) *keyVendor {
	t.Helper()
	k := &keyVendor{Server: httptest.NewTLSServer(mux), codes: map[string]keyCode{}}
	t.Cleanup(k.Close)
	return k
}

// Guard is an egress guard that trusts the fake.
func (k *keyVendor) Guard() mcpauth.Guard {
	pool := x509.NewCertPool()
	pool.AddCert(k.Certificate())
	return mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

// connect is a KeyConnect on the fake, with a browser that approves.
func (k *keyVendor) connect() mcpauth.KeyConnect {
	c := k.Client()
	return mcpauth.KeyConnect{
		HTTP: k.Guard().Client(),
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

func (k *keyVendor) next(prefix string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seq++
	return fmt.Sprintf("%s%d-%s", prefix, k.seq, random())
}

// Handed reports whether s was issued by the fake.
func (k *keyVendor) Handed(s string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, v := range k.Issued {
		if v == s {
			return true
		}
	}
	return false
}

// authorize checks a loopback redirect and PKCE, keeps the code, and sends
// the browser back with the code and the state; deny sends it back refused.
func (k *keyVendor) authorize(w http.ResponseWriter, r *http.Request, redirect, client, challenge string, deny bool) {
	q := r.URL.Query()
	k.mu.Lock()
	k.Authorizations = append(k.Authorizations, q)
	k.mu.Unlock()
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != mcpauth.CallbackPath {
		http.Error(w, "the redirect must be http://127.0.0.1:<port>/callback", http.StatusBadRequest)
		return
	}
	if challenge == "" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		http.Error(w, "PKCE S256 and a state are required", http.StatusBadRequest)
		return
	}
	back := url.Values{"state": {q.Get("state")}}
	if deny {
		back.Set("error", "access_denied")
	} else {
		code := k.next("code-")
		k.mu.Lock()
		k.codes[code] = keyCode{client: client, redirect: redirect, challenge: challenge}
		k.mu.Unlock()
		back.Set("code", code)
	}
	http.Redirect(w, r, redirect+"?"+back.Encode(), http.StatusFound)
}

// redeem spends a code: the client, the redirect (when given) and the
// verifier must be the ones it was issued for.
func (k *keyVendor) redeem(code, client, redirect, verifier string) bool {
	k.mu.Lock()
	c, ok := k.codes[code]
	delete(k.codes, code)
	k.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	return ok && c.client == client && (redirect == "" || c.redirect == redirect) &&
		base64.RawURLEncoding.EncodeToString(sum[:]) == c.challenge
}

func (k *keyVendor) issue(prefix string) string {
	s := k.next(prefix)
	k.mu.Lock()
	k.Issued = append(k.Issued, s)
	k.mu.Unlock()
	return s
}

func (k *keyVendor) issuedBearer(r *http.Request) bool {
	return k.Handed(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

// OpenRouter is a fake of OpenRouter's PKCE key flow: /auth sends the
// browser back to the callback with a code and the state, and
// /api/v1/auth/keys trades the code and its verifier for an sk-or- key.
type OpenRouter struct {
	*keyVendor
	// Deny refuses the authorization in the browser.
	Deny bool
	// KeyPrefix starts every key issued (sk-or-v1-PROBE- by default).
	KeyPrefix string
	// Exchanges is every key request's body.
	Exchanges []map[string]string
}

// NewOpenRouter starts the fake.
func NewOpenRouter(t *testing.T) *OpenRouter {
	mux := http.NewServeMux()
	v := &OpenRouter{KeyPrefix: "sk-or-v1-PROBE-"}
	v.keyVendor = newKeyVendor(t, mux)
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		v.authorize(w, r, q.Get("callback_url"), "", q.Get("code_challenge"), v.Deny)
	})
	mux.HandleFunc("/api/v1/auth/keys", v.keys)
	return v
}

// Flow is a mcpauth.OpenRouter pointed at the fake.
func (v *OpenRouter) Flow() mcpauth.OpenRouter {
	return mcpauth.OpenRouter{KeyConnect: v.connect(), AuthURL: v.URL + "/auth", KeysURL: v.URL + "/api/v1/auth/keys"}
}

func (v *OpenRouter) keys(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "a JSON POST is required", http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	v.Exchanges = append(v.Exchanges, body)
	v.mu.Unlock()
	if body["code_challenge_method"] != "S256" || !v.redeem(body["code"], "", "", body["code_verifier"]) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":{"code":403,"message":"Invalid code or code_verifier %s"}}`, body["code_verifier"])
		return
	}
	writeJSON(w, map[string]any{"key": v.issue(v.KeyPrefix), "user_id": "user_fixture"})
}

// Poe is a fake of Sign in with Poe: /oauth/authorize for one client and
// the apikey:create scope, /token minting a key with ExpiresIn, and the
// balance route that names the account.
type Poe struct {
	*keyVendor
	ClientID string
	// ExpiresIn is the key's lifetime in seconds; 0 sends null (no expiry).
	ExpiresIn int
	Email     string
	// Deny refuses the authorization in the browser.
	Deny bool
	// Requests is every token request's form.
	Requests []url.Values
}

// NewPoe starts the fake.
func NewPoe(t *testing.T) *Poe {
	mux := http.NewServeMux()
	v := &Poe{ClientID: "poe-client-fixture", Email: "person@example.com"}
	v.keyVendor = newKeyVendor(t, mux)
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != v.ClientID || q.Get("scope") != mcpauth.PoeScope || q.Get("response_type") != "code" {
			http.Error(w, "unknown client or scope", http.StatusBadRequest)
			return
		}
		v.authorize(w, r, q.Get("redirect_uri"), q.Get("client_id"), q.Get("code_challenge"), v.Deny)
	})
	mux.HandleFunc("/token", v.token)
	mux.HandleFunc("/usage/current_balance", func(w http.ResponseWriter, r *http.Request) {
		if !v.issuedBearer(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"email": v.Email, "current_point_balance": 1000})
	})
	return v
}

// Flow is a mcpauth.Poe pointed at the fake.
func (v *Poe) Flow() mcpauth.Poe {
	return mcpauth.Poe{KeyConnect: v.connect(), AuthorizeURL: v.URL + "/oauth/authorize", TokenURL: v.URL + "/token", BalanceURL: v.URL + "/usage/current_balance"}
}

func (v *Poe) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	v.mu.Lock()
	v.Requests = append(v.Requests, f)
	v.mu.Unlock()
	if f.Get("grant_type") != "authorization_code" || !v.redeem(f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier")) {
		oauthErr(w, "invalid_grant")
		return
	}
	var expires any
	if v.ExpiresIn > 0 {
		expires = v.ExpiresIn
	}
	writeJSON(w, map[string]any{"api_key": v.issue("PROBE-POE-KEY-"), "api_key_expires_in": expires})
}

// HuggingFace is a fake of Hugging Face's OAuth: discovery (with no
// revocation endpoint unless Revocation is set), authorization for a
// client metadata document it serves or a registered public client,
// tokens with refresh (rotated when Rotate is set), and userinfo.
type HuggingFace struct {
	*keyVendor
	// AppID is a public app's client id the fake also takes.
	AppID string
	// Username is the account userinfo names.
	Username string
	// NoInference grants the scopes asked but inference-api; NoRefresh
	// issues no refresh token; Rotate rotates refresh tokens; Refused
	// refuses every refresh; Revocation names a revocation endpoint;
	// NoCIMD serves no metadata document.
	NoInference, NoRefresh, Rotate, Refused, Revocation, NoCIMD bool
	// Requests is every token request's form; Revocations every revocation's.
	Requests, Revocations []url.Values
	refresh               map[string]bool
}

// NewHuggingFace starts the fake.
func NewHuggingFace(t *testing.T) *HuggingFace {
	mux := http.NewServeMux()
	v := &HuggingFace{AppID: "hf-app-fixture", Username: "person", refresh: map[string]bool{}}
	v.keyVendor = newKeyVendor(t, mux)
	mux.HandleFunc("/.well-known/oauth-authorization-server", v.discovery)
	mux.HandleFunc("/oauth/authorize", v.authorizeHF)
	mux.HandleFunc("/oauth/token", v.token)
	mux.HandleFunc("/oauth/revoke", v.revoke)
	mux.HandleFunc("/oauth/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !v.issuedBearer(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"sub": "u-fixture", "preferred_username": v.Username})
	})
	mux.HandleFunc("/cimd.json", func(w http.ResponseWriter, _ *http.Request) {
		if v.NoCIMD {
			http.NotFound(w, nil)
			return
		}
		writeJSON(w, map[string]any{
			"client_id": v.CIMDURL(), "client_name": "Agentry",
			"redirect_uris":              []string{"http://127.0.0.1/callback", "http://localhost/callback"},
			"token_endpoint_auth_method": "none",
		})
	})
	return v
}

// CIMDURL is the client metadata document the fake serves.
func (v *HuggingFace) CIMDURL() string { return v.URL + "/cimd.json" }

// Flow is a mcpauth.HuggingFace pointed at the fake.
func (v *HuggingFace) Flow() mcpauth.HuggingFace {
	return mcpauth.HuggingFace{KeyConnect: v.connect(), Issuer: v.URL}
}

// Live reports whether rt is a refresh token the fake would still honour.
func (v *HuggingFace) Live(rt string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refresh[rt]
}

func (v *HuggingFace) discovery(w http.ResponseWriter, _ *http.Request) {
	md := map[string]any{
		"issuer": v.URL, "authorization_endpoint": v.URL + "/oauth/authorize", "token_endpoint": v.URL + "/oauth/token",
		"userinfo_endpoint": v.URL + "/oauth/userinfo", "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"client_id_metadata_document_supported": true,
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
	}
	if v.Revocation {
		md["revocation_endpoint"] = v.URL + "/oauth/revoke"
	}
	writeJSON(w, md)
}

func (v *HuggingFace) knownClient(id string) bool { return id == v.AppID || id == v.CIMDURL() }

func (v *HuggingFace) authorizeHF(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !v.knownClient(q.Get("client_id")) || !strings.Contains(" "+q.Get("scope")+" ", " "+mcpauth.HuggingFaceInferenceScope+" ") || q.Get("response_type") != "code" {
		http.Error(w, "unknown client or scope", http.StatusBadRequest)
		return
	}
	v.authorize(w, r, q.Get("redirect_uri"), q.Get("client_id"), q.Get("code_challenge"), false)
}

func (v *HuggingFace) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	v.mu.Lock()
	v.Requests = append(v.Requests, f)
	v.mu.Unlock()
	if f.Get("client_secret") != "" || r.Header.Get("Authorization") != "" {
		oauthErr(w, "invalid_client")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		if !v.redeem(f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier")) {
			oauthErr(w, "invalid_grant")
			return
		}
		v.grant(w, "")
	case "refresh_token":
		rt := f.Get("refresh_token")
		v.mu.Lock()
		live := v.refresh[rt]
		if live && v.Rotate {
			delete(v.refresh, rt)
		}
		v.mu.Unlock()
		if v.Refused || !live || !v.knownClient(f.Get("client_id")) {
			oauthErr(w, "invalid_grant")
			return
		}
		keep := rt
		if v.Rotate {
			keep = ""
		}
		v.grant(w, keep)
	default:
		oauthErr(w, "unsupported_grant_type")
	}
}

// grant issues an access token and, unless NoRefresh, a refresh token: the
// one kept (no rotation) or a new one.
func (v *HuggingFace) grant(w http.ResponseWriter, kept string) {
	scope := "openid profile " + mcpauth.HuggingFaceInferenceScope
	if v.NoInference {
		scope = "openid profile"
	}
	out := map[string]any{"access_token": v.issue("hf_oauth_PROBE-ACCESS-"), "token_type": "bearer", "expires_in": 28800, "scope": scope}
	if !v.NoRefresh {
		rt := kept
		if rt == "" {
			rt = v.issue("PROBE-HF-REFRESH-")
			v.mu.Lock()
			v.refresh[rt] = true
			v.mu.Unlock()
		}
		out["refresh_token"] = rt
	}
	writeJSON(w, out)
}

func (v *HuggingFace) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	v.mu.Lock()
	v.Revocations = append(v.Revocations, r.PostForm)
	delete(v.refresh, r.PostForm.Get("token"))
	v.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
