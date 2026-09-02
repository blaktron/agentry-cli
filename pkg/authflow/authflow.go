// Package authflow is the client half of browser sign-in (core DEV-058,
// hardened per agentry#19): a one-shot loopback listener that receives a
// single-use CODE from the registry's /settings/tokens/desktop page after
// the user signs in (through the directory service, DEV-057) and clicks
// Authorize, then exchanges that code for the PAT from this process —
// POST /api/v1/auth/desktop/exchange — so the token never rides a URL or
// lands in the browser's history. Promoted from agentry-desktop so
// `agentry login` (agentry-cli#5) and the desktop app share one
// implementation.
//
// Trust model: the listener binds 127.0.0.1 only, accepts exactly one
// callback, and requires the state nonce this process generated — a
// browser tab cannot plant a code the app did not ask for, and a code
// meant for this app cannot be delivered anywhere else (the registry pins
// its redirect to 127.0.0.1:<port>). The code is bound to the same nonce
// server-side, is single-use, and expires in a minute. The token is still
// verified against the registry (whoami) before it is stored, exactly
// like a pasted PAT.
//
// Older registries (pre-#19) send ?token= directly; that is still accepted
// so the app keeps working against them, with the history caveat D-015
// records.
package authflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Timeout bounds how long a sign-in may sit waiting on the browser.
const Timeout = 5 * time.Minute

// donePage is what the browser shows once the token has been handed over.
const donePage = `<!doctype html><meta charset="utf-8"><title>Signed in</title>
<body style="font-family:system-ui;background:#141210;color:#e7e5e4;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1 style="font-weight:600">Signed in</h1>
<p>You can close this tab and return to Agentry Desktop.</p></div>`

// Flow is one in-flight browser sign-in.
type Flow struct {
	// Port is the loopback port the registry must redirect to.
	Port int
	// State is the nonce the callback must echo.
	State string
	// Registry is the origin the code is exchanged against; set by
	// AuthorizeURL, which is always called before the callback can arrive.
	Registry string
	// HTTP performs the exchange; tests substitute it.
	HTTP *http.Client

	srv  *http.Server
	ch   chan outcome
	once sync.Once
}

type outcome struct {
	token string
	err   error
}

// Start binds 127.0.0.1:0 and begins serving the one-shot callback.
func Start() (*Flow, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener: %w", err)
	}
	stateBytes := make([]byte, 24)
	if _, err := rand.Read(stateBytes); err != nil {
		_ = ln.Close()
		return nil, err
	}
	f := &Flow{
		Port:  ln.Addr().(*net.TCPAddr).Port,
		State: base64.RawURLEncoding.EncodeToString(stateBytes),
		ch:    make(chan outcome, 1),
		HTTP:  &http.Client{Timeout: 15 * time.Second},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", f.handleCallback)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// AuthorizeURL is the page to open in the system browser.
func (f *Flow) AuthorizeURL(registry string) string {
	f.Registry = strings.TrimRight(registry, "/")
	return fmt.Sprintf("%s/settings/tokens/desktop?port=%d&state=%s",
		f.Registry, f.Port, url.QueryEscape(f.State))
}

func (f *Flow) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	token := r.URL.Query().Get("token")
	if subtle.ConstantTimeCompare([]byte(state), []byte(f.State)) != 1 {
		http.Error(w, "state mismatch — start the sign-in again from the app", http.StatusForbidden)
		f.deliver(outcome{err: errors.New("callback state mismatch")})
		return
	}
	if code := r.URL.Query().Get("code"); code != "" {
		// The hardened shape: exchange the single-use code for the PAT from
		// this process. The browser only sees "signed in" once that worked.
		t, err := f.exchange(r.Context(), code)
		if err != nil {
			http.Error(w, "sign-in could not be completed — start it again from the app", http.StatusBadGateway)
			f.deliver(outcome{err: err})
			return
		}
		token = t
	}
	if token == "" {
		http.Error(w, "no code or token in callback", http.StatusBadRequest)
		f.deliver(outcome{err: errors.New("callback carried no code")})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The token must never be cached or logged by anything in between —
	// there is nothing in between (loopback), but belt and braces.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(donePage))
	f.deliver(outcome{token: token})
}

// exchange trades the code for the PAT at the registry the flow was started
// against. Any failure is an error to the caller; the code is single-use, so
// a retry means a new sign-in.
func (f *Flow) exchange(ctx context.Context, code string) (string, error) {
	if f.Registry == "" {
		return "", errors.New("exchange: registry not set (AuthorizeURL was not called)")
	}
	body, _ := json.Marshal(map[string]string{"code": code, "state": f.State})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Registry+"/api/v1/auth/desktop/exchange", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := f.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange sign-in code: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("exchange sign-in code: registry answered %d", res.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", errors.New("exchange sign-in code: registry returned no token")
	}
	return out.Token, nil
}

func (f *Flow) deliver(o outcome) {
	f.once.Do(func() { f.ch <- o })
}

// Wait blocks until the callback arrives, the context ends, or Timeout.
func (f *Flow) Wait(ctx context.Context) (string, error) {
	t := time.NewTimer(Timeout)
	defer t.Stop()
	select {
	case o := <-f.ch:
		return o.token, o.err
	case <-t.C:
		return "", errors.New("browser sign-in timed out — start it again from Settings")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close tears the listener down. Safe after Wait, and on every exit path.
func (f *Flow) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(ctx)
}
