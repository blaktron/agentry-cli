// Package mcpauthtest is a fake vendor for tests of OAuth connections: a
// remote MCP server that answers 401 with its resource metadata, and the
// authorization server behind it (metadata, consent, the token endpoint
// with refresh-token rotation, dynamic registration, revocation), on one
// TLS httptest server. Browser plays the person's browser.
package mcpauthtest

import (
	"crypto/rand"
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

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// Vendor is the fake. Fields set before the first request shape it.
type Vendor struct {
	*httptest.Server

	// DCR serves dynamic registration; CIMD advertises client metadata
	// documents; NoISS leaves the iss parameter off the redirect.
	DCR, CIMD, NoISS bool
	// RefreshPrefix starts every refresh token issued, so a test can
	// search for it (D8's probe); ClientSecret is what a registration
	// issues when the server takes no public client.
	RefreshPrefix string
	ClientSecret  string
	// NoPublic leaves "none" out of the token endpoint's auth methods.
	NoPublic bool
	// Scope, when set, is the WWW-Authenticate scope.
	Scope string
	// IssuerOnly is a sign-in at the issuer for a package server
	// (agentry-cli#665): the authorization request must carry no resource.
	IssuerOnly bool
	// SecretAnyway issues ClientSecret to every registration, one that
	// asked to be public included, and names no auth method in the answer
	// (a vendor that makes every client confidential).
	SecretAnyway bool

	mu       sync.Mutex
	codes    map[string]codeGrant
	refresh  map[string]string // refresh token → client id
	clients  map[string]string // client id → secret ("" for public)
	Revoked  []string
	Requests []url.Values // every token-endpoint form
	Refused  bool         // refuse every refresh with invalid_grant
	Issued   []string     // every refresh token issued
	// Registrations is every registration request's body.
	Registrations []map[string]any
}

type codeGrant struct {
	clientID, redirect, challenge, resource string
}

// New starts a vendor.
func New(t *testing.T) *Vendor {
	t.Helper()
	v := &Vendor{RefreshPrefix: "PROBE-REFRESH-", codes: map[string]codeGrant{}, refresh: map[string]string{}, clients: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", v.mcp)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", v.prm)
	mux.HandleFunc("/.well-known/oauth-authorization-server", v.asm)
	mux.HandleFunc("/authorize", v.authorize)
	mux.HandleFunc("/token", v.token)
	mux.HandleFunc("/register", v.register)
	mux.HandleFunc("/revoke", v.revoke)
	v.Server = httptest.NewTLSServer(mux)
	t.Cleanup(v.Close)
	return v
}

// ServerURL is the fake MCP server's address.
func (v *Vendor) ServerURL() string { return v.URL + "/mcp" }

// Guard trusts the fake's certificate and its loopback address.
func (v *Vendor) Guard() mcpauth.Guard {
	pool := x509.NewCertPool()
	pool.AddCert(v.Certificate())
	return mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

// AddClient registers a client the person made themselves.
func (v *Vendor) AddClient(id, secret string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clients[id] = secret
}

// Browser plays the person: it opens the consent address and, approving,
// follows the vendor's redirect to wherever it points (the loopback).
func (v *Vendor) Browser() func(authURL string) {
	c := v.Client()
	return func(authURL string) {
		go func() {
			res, err := c.Get(authURL)
			if err == nil {
				_ = res.Body.Close()
			}
		}()
	}
}

// Redirect is the address the vendor would send the browser to for
// authURL, without following it: what a person pastes back.
func (v *Vendor) Redirect(authURL string) (string, error) {
	c := v.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Get(authURL)
	if err != nil {
		return "", err
	}
	_ = res.Body.Close()
	return res.Header.Get("Location"), nil
}

func (v *Vendor) mcp(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(http.StatusOK)
		return
	}
	h := fmt.Sprintf(`Bearer realm="OAuth", resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, v.URL)
	if v.Scope != "" {
		h += fmt.Sprintf(`, scope="%s"`, v.Scope)
	}
	w.Header().Set("WWW-Authenticate", h)
	w.WriteHeader(http.StatusUnauthorized)
}

func (v *Vendor) prm(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"resource": v.ServerURL(), "authorization_servers": []string{v.URL}, "scopes_supported": []string{"read", "write"}})
}

func (v *Vendor) asm(w http.ResponseWriter, _ *http.Request) {
	methods := []string{"client_secret_basic", "client_secret_post", "none"}
	if v.NoPublic {
		methods = methods[:2]
	}
	md := map[string]any{
		"issuer": v.URL, "authorization_endpoint": v.URL + "/authorize", "token_endpoint": v.URL + "/token",
		"revocation_endpoint": v.URL + "/revoke", "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported":          methods,
		"authorization_response_iss_parameter_supported": !v.NoISS,
		"client_id_metadata_document_supported":          v.CIMD,
	}
	if v.DCR {
		md["registration_endpoint"] = v.URL + "/register"
	}
	writeJSON(w, md)
}

func (v *Vendor) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v.mu.Lock()
	_, known := v.clients[q.Get("client_id")]
	v.mu.Unlock()
	if !known && !strings.HasPrefix(q.Get("client_id"), "https://") {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	resource := v.ServerURL()
	if v.IssuerOnly {
		resource = ""
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("resource") != resource {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := random()
	v.mu.Lock()
	v.codes[code] = codeGrant{clientID: q.Get("client_id"), redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), resource: q.Get("resource")}
	v.mu.Unlock()
	back := url.Values{"code": {code}, "state": {q.Get("state")}}
	if !v.NoISS {
		back.Set("iss", v.URL)
	}
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
}

func (v *Vendor) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Requests = append(v.Requests, form)
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = form.Get("client_id"), form.Get("client_secret")
	}
	if want, ok := v.clients[id]; ok && want != secret {
		oauthErr(w, "invalid_client")
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		g, ok := v.codes[form.Get("code")]
		delete(v.codes, form.Get("code"))
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || g.clientID != id || g.redirect != form.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthErr(w, "invalid_grant")
			return
		}
	case "refresh_token":
		owner, ok := v.refresh[form.Get("refresh_token")]
		if !ok || owner != id || v.Refused {
			oauthErr(w, "invalid_grant")
			return
		}
		delete(v.refresh, form.Get("refresh_token")) // rotated
	default:
		oauthErr(w, "unsupported_grant_type")
		return
	}
	rt := v.RefreshPrefix + random()
	v.refresh[rt] = id
	v.Issued = append(v.Issued, rt)
	writeJSON(w, map[string]any{"access_token": "access-" + random(), "token_type": "Bearer", "expires_in": 3600, "refresh_token": rt, "scope": "read"})
}

func (v *Vendor) register(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	method, _ := body["token_endpoint_auth_method"].(string)
	id := "dcr-" + random()
	out := map[string]any{"client_id": id, "token_endpoint_auth_method": method}
	secret := ""
	switch {
	case v.SecretAnyway:
		secret = v.ClientSecret
		out["client_secret"] = secret
		delete(out, "token_endpoint_auth_method")
	case method != "none":
		secret = v.ClientSecret
		out["client_secret"] = secret
	}
	v.mu.Lock()
	v.clients[id] = secret
	v.Registrations = append(v.Registrations, body)
	v.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, out)
}

func (v *Vendor) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	v.mu.Lock()
	v.Revoked = append(v.Revoked, r.PostForm.Get("token"))
	delete(v.refresh, r.PostForm.Get("token"))
	v.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Live is whether a refresh token is still good at the vendor.
func (v *Vendor) Live(rt string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.refresh[rt]
	return ok
}

func oauthErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "refused by the fake"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func random() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
