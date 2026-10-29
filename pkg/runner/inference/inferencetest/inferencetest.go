// Package inferencetest is a fake registry's credits sessions for tests:
// it opens, renews and ends sessions as a SecureAgentry registry does, and
// records each call.
package inferencetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SignIn is the registry sign-in the fake accepts; Token prefixes the
// session tokens it answers. Both are fake values of the real shapes.
const (
	SignIn = "agy_fakesigninfortestsonly0000000000000000000000000"
	Token  = "agym_fakesessiontokenfortestsonly"
	Model  = "fake-model-1"
)

// Registry is the fake. Set NoCredits before a call to answer 402.
type Registry struct {
	*httptest.Server
	mu        sync.Mutex
	opened    []string // purposes
	renewed   []string // session ids
	ended     []string
	endedAt   map[string]time.Time
	renewedAt []time.Time
	NoCredits bool
	// RenewEverySeconds is what an open answers (0: 900).
	RenewEverySeconds int64
	// Usage is the tokens and credits each session's view reports.
	InputTokens, OutputTokens, Credits int64
	// EndRenewals makes every renewal answer 410, as for a session ended.
	EndRenewals bool
}

// New starts a fake registry, closed when the test ends.
func New(t *testing.T) *Registry {
	t.Helper()
	r := &Registry{endedAt: map[string]time.Time{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

// SetEndRenewals sets EndRenewals while the fake is serving.
func (r *Registry) SetEndRenewals(v bool) { r.mu.Lock(); defer r.mu.Unlock(); r.EndRenewals = v }

// Opened, Renewed and Ended are the calls so far.
func (r *Registry) Opened() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.opened...)
}

func (r *Registry) Renewed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.renewed...)
}

func (r *Registry) Ended() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.ended...)
}

// RenewedAt is when each renewal arrived.
func (r *Registry) RenewedAt() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time{}, r.renewedAt...)
}

// EndedAt is when session id was ended (zero if it was not).
func (r *Registry) EndedAt(id string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endedAt[id]
}

// TokenFor is the token the n-th session opened (from 1) was answered.
func TokenFor(n int) string { return fmt.Sprintf("%s%04d", Token, n) }

func (r *Registry) view(id string, ended bool) map[string]any {
	return map[string]any{"id": id, "purpose": "run", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ended": ended, "requests": 2, "inputTokens": r.InputTokens, "outputTokens": r.OutputTokens, "credits": r.Credits}
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+SignIn {
		problem(w, http.StatusUnauthorized, "The bearer token was not recognized.")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	const base = "/api/v1/x/inference/sessions"
	switch {
	case req.Method == http.MethodPost && req.URL.Path == base:
		if r.NoCredits {
			problem(w, http.StatusPaymentRequired, "Your Agentry credits are used up")
			return
		}
		var body struct {
			Purpose string `json:"purpose"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.opened = append(r.opened, body.Purpose)
		n := len(r.opened)
		every := r.RenewEverySeconds
		if every == 0 {
			every = 900
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"session": r.view(fmt.Sprintf("s-%d", n), false), "token": TokenFor(n),
			"model": Model, "renewEverySeconds": every})
	case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, base+"/") && strings.HasSuffix(req.URL.Path, "/renew"):
		id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, base+"/"), "/renew")
		if r.EndRenewals || !r.endedAt[id].IsZero() {
			problem(w, http.StatusGone, "This session has ended.")
			return
		}
		r.renewed, r.renewedAt = append(r.renewed, id), append(r.renewedAt, time.Now())
		_ = json.NewEncoder(w).Encode(map[string]any{"session": r.view(id, false)})
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, base+"/"):
		id := strings.TrimPrefix(req.URL.Path, base+"/")
		r.ended = append(r.ended, id)
		r.endedAt[id] = time.Now()
		_ = json.NewEncoder(w).Encode(map[string]any{"session": r.view(id, true)})
	default:
		problem(w, http.StatusNotFound, "Not found.")
	}
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
}
