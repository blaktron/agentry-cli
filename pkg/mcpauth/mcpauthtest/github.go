package mcpauthtest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// GitHub is a fake of GitHub's device flow and the endpoints a Copilot
// sign-in uses (pkg/mcpauth/github.go): the device code, the token
// endpoint's polling and its secret-free refresh (which rotates the
// refresh token and spends the old one), /user, and the unauthenticated
// credential revocation. Fields set before the first request shape it.
type GitHub struct {
	*httptest.Server

	// Login is the account /user names.
	Login string
	// Pending is how many polls answer authorization_pending first;
	// SlowDown answers slow_down once before them.
	Pending  int
	SlowDown bool
	// Declined answers access_denied; Expired expired_token.
	Declined, Expired bool
	// NoExpiry issues tokens without expiry or refresh token, as an OAuth
	// App whose tokens do not expire does.
	NoExpiry bool
	// AccessPrefix and RefreshPrefix start every token issued (ghu_, ghr_
	// by default), so a test can search for them.
	AccessPrefix, RefreshPrefix string

	mu sync.Mutex
	// DeviceRequests is every device-code request's form; TokenRequests
	// every token-endpoint form.
	DeviceRequests, TokenRequests []url.Values
	// Revoked is every credential revoked; RevokeAuth the Authorization
	// header any revocation carried.
	Revoked    []string
	RevokeAuth []string
	// UserCalls is every /user call's Authorization header.
	UserCalls []string
	// Waits are the waits between polls the flow asked for.
	Waits   []time.Duration
	polls   int
	slowed  bool
	seq     int
	refresh map[string]string // live refresh token → client id
	access  map[string]bool   // live access tokens
}

// GitHubClientID is the fake's client id.
const GitHubClientID = "Iv1.fixtureclient"

// NewGitHub starts the fake.
func NewGitHub(t *testing.T) *GitHub {
	t.Helper()
	v := &GitHub{Login: "octo-fixture", AccessPrefix: "ghu_PROBEACCESS", RefreshPrefix: "ghr_PROBEREFRESH",
		refresh: map[string]string{}, access: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", v.deviceCode)
	mux.HandleFunc("/login/oauth/access_token", v.token)
	mux.HandleFunc("/user", v.user)
	mux.HandleFunc("/credentials/revoke", v.revoke)
	v.Server = httptest.NewTLSServer(mux)
	t.Cleanup(v.Close)
	return v
}

// Flow is a mcpauth.GitHub pointed at the fake, polling without waiting
// (each wait it would make is kept in Waits).
func (v *GitHub) Flow() mcpauth.GitHub {
	pool := x509.NewCertPool()
	pool.AddCert(v.Certificate())
	g := mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return mcpauth.GitHub{
		HTTP: g.Client(), DeviceCodeURL: v.URL + "/login/device/code", TokenURL: v.URL + "/login/oauth/access_token",
		DevicePage: v.URL + "/login/device", API: v.URL,
		Present: func(string, string) {},
		Sleep: func(ctx context.Context, d time.Duration) error {
			v.mu.Lock()
			v.Waits = append(v.Waits, d)
			v.mu.Unlock()
			return ctx.Err()
		},
	}
}

// Live reports whether rt is a refresh token the fake would still honour,
// and LiveAccess whether at is an access token it still accepts.
func (v *GitHub) Live(rt string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.refresh[rt]
	return ok
}

// LiveAccess reports whether at is an access token the fake accepts.
func (v *GitHub) LiveAccess(at string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.access[at]
}

// Issue grants a pair outside the device flow, as a sign-in kept before.
func (v *GitHub) Issue(client string) (access, refresh string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.issueLocked(client)
}

func (v *GitHub) issueLocked(client string) (string, string) {
	v.seq++
	at := fmt.Sprintf("%s%04d", v.AccessPrefix, v.seq)
	v.access[at] = true
	if v.NoExpiry {
		return at, ""
	}
	rt := fmt.Sprintf("%s%04d", v.RefreshPrefix, v.seq)
	v.refresh[rt] = client
	return at, rt
}

func githubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (v *GitHub) deviceCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	v.mu.Lock()
	v.DeviceRequests = append(v.DeviceRequests, r.PostForm)
	v.mu.Unlock()
	if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" || r.PostForm.Get("client_id") != GitHubClientID {
		githubJSON(w, http.StatusOK, map[string]any{"error": "incorrect_client_credentials"})
		return
	}
	githubJSON(w, http.StatusOK, map[string]any{"device_code": "dc-fixture", "user_code": "WDJB-MJHT",
		"verification_uri": v.URL + "/login/device", "expires_in": 900, "interval": 5})
}

func (v *GitHub) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	v.mu.Lock()
	defer v.mu.Unlock()
	v.TokenRequests = append(v.TokenRequests, f)
	if f.Get("client_secret") != "" {
		githubJSON(w, http.StatusOK, map[string]any{"error": "incorrect_client_credentials", "error_description": "this fake never takes a secret"})
		return
	}
	switch f.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		if f.Get("device_code") != "dc-fixture" || f.Get("client_id") != GitHubClientID {
			githubJSON(w, http.StatusOK, map[string]any{"error": "incorrect_device_code"})
			return
		}
		v.polls++
		switch {
		case v.SlowDown && !v.slowed:
			v.slowed = true
			githubJSON(w, http.StatusOK, map[string]any{"error": "slow_down", "interval": 10})
			return
		case v.polls <= v.Pending:
			githubJSON(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
			return
		case v.Declined:
			githubJSON(w, http.StatusOK, map[string]any{"error": "access_denied"})
			return
		case v.Expired:
			githubJSON(w, http.StatusOK, map[string]any{"error": "expired_token"})
			return
		}
		githubJSON(w, http.StatusOK, v.grantLocked(f.Get("client_id")))
	case "refresh_token":
		client, ok := v.refresh[f.Get("refresh_token")]
		if !ok || client != f.Get("client_id") {
			githubJSON(w, http.StatusOK, map[string]any{"error": "bad_refresh_token", "error_description": "The refresh token passed is incorrect or expired."})
			return
		}
		delete(v.refresh, f.Get("refresh_token"))
		githubJSON(w, http.StatusOK, v.grantLocked(client))
	default:
		githubJSON(w, http.StatusOK, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (v *GitHub) grantLocked(client string) map[string]any {
	at, rt := v.issueLocked(client)
	if rt == "" {
		return map[string]any{"access_token": at, "token_type": "bearer", "scope": ""}
	}
	return map[string]any{"access_token": at, "expires_in": 28800, "refresh_token": rt,
		"refresh_token_expires_in": 15897600, "token_type": "bearer", "scope": ""}
}

func (v *GitHub) user(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	v.mu.Lock()
	v.UserCalls = append(v.UserCalls, auth)
	ok := v.access[strings.TrimPrefix(auth, "Bearer ")]
	v.mu.Unlock()
	if !ok {
		githubJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	githubJSON(w, http.StatusOK, map[string]any{"login": v.Login, "id": 1})
}

func (v *GitHub) revoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Credentials []string `json:"credentials"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	v.mu.Lock()
	defer v.mu.Unlock()
	if a := r.Header.Get("Authorization"); a != "" {
		v.RevokeAuth = append(v.RevokeAuth, a)
		githubJSON(w, http.StatusForbidden, map[string]any{"message": "Authenticated requests are not allowed"})
		return
	}
	for _, c := range body.Credentials {
		v.Revoked = append(v.Revoked, c)
		delete(v.refresh, c)
		delete(v.access, c)
	}
	w.WriteHeader(http.StatusAccepted)
}
