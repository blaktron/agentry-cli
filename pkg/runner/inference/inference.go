// Package inference is the client of a registry's credits sessions: the
// sessions a run, or a persistent agent, pays for its model with when its
// policy names model provider agentry. The host opens one with the
// person's registry sign-in, hands the run only the session's token,
// renews the session while the run lives, and ends it when the run ends.
// The token reaches nothing but the registry's model endpoint; the
// sign-in never leaves the host.
package inference

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

	"github.com/blaktron/agentry-cli/internal/origin"
)

// Purposes a session is opened for.
const (
	PurposeRun   = "run"
	PurposeAgent = "agent"
)

// sessionsPath is where a registry serves its credits sessions.
const sessionsPath = "/api/v1/x/inference/sessions"

// DefaultRenewEvery is how often a session is renewed when the registry
// names no interval. A session not renewed for an hour ends.
const DefaultRenewEvery = 15 * time.Minute

// ErrSignedOut is a client with no sign-in to open or renew a session with.
var ErrSignedOut = errors.New("not signed in to the registry")

// ErrEnded is a session the registry has ended: renewed too late, ended
// already, or its sign-in revoked.
var ErrEnded = errors.New("the credits session has ended")

// ErrNoCredits is a payer whose credits are used up.
var ErrNoCredits = errors.New("the Agentry credits are used up")

// View is a session as the registry reports it.
type View struct {
	ID           string    `json:"id"`
	Purpose      string    `json:"purpose"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Ended        bool      `json:"ended"`
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	Credits      int64     `json:"credits"`
}

// Opened is a new session: its token, answered once and never again, the
// model the registry forwards to, and how often to renew it.
type Opened struct {
	Session    View
	Token      string
	Model      string
	RenewEvery time.Duration
}

// Client is one registry, as the account its sign-in speaks for.
type Client struct {
	registry string
	signIn   func() (string, error)
	hc       *http.Client
}

// New is the client of the registry at regURL (no trailing slash). signIn
// answers the account's registry sign-in at each call, so a renewal made
// an hour later uses the sign-in as it is then; an empty answer is
// ErrSignedOut. hc is the HTTP client every request goes through (nil:
// one with a minute's timeout).
func New(regURL string, signIn func() (string, error), hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	return &Client{registry: strings.TrimRight(regURL, "/"), signIn: signIn, hc: origin.Guard(hc)}
}

// Registry is the registry's address.
func (c *Client) Registry() string { return c.registry }

// Open opens a session for purpose (PurposeRun or PurposeAgent).
func (c *Client) Open(ctx context.Context, purpose string) (Opened, error) {
	var out struct {
		Session           View   `json:"session"`
		Token             string `json:"token"`
		Model             string `json:"model"`
		RenewEverySeconds int64  `json:"renewEverySeconds"`
	}
	if err := c.do(ctx, http.MethodPost, sessionsPath, map[string]string{"purpose": purpose}, http.StatusCreated, &out); err != nil {
		return Opened{}, fmt.Errorf("open a credits session at %s: %w", c.registry, err)
	}
	if out.Token == "" || out.Session.ID == "" {
		return Opened{}, fmt.Errorf("open a credits session at %s: the registry answered no token", c.registry)
	}
	every := time.Duration(out.RenewEverySeconds) * time.Second
	if every <= 0 {
		every = DefaultRenewEvery
	}
	return Opened{Session: out.Session, Token: out.Token, Model: out.Model, RenewEvery: every}, nil
}

// Renew settles what the session has used since the last settlement and
// moves its expiry an hour out. A session the registry has ended is
// ErrEnded.
func (c *Client) Renew(ctx context.Context, id string) (View, error) {
	var out struct {
		Session View `json:"session"`
	}
	if err := c.do(ctx, http.MethodPost, sessionsPath+"/"+id+"/renew", nil, http.StatusOK, &out); err != nil {
		return View{}, fmt.Errorf("renew credits session %s: %w", id, err)
	}
	return out.Session, nil
}

// End settles what is left and ends the session: its token is refused
// from then on. Ending a session already ended answers its final view.
func (c *Client) End(ctx context.Context, id string) (View, error) {
	var out struct {
		Session View `json:"session"`
	}
	if err := c.do(ctx, http.MethodDelete, sessionsPath+"/"+id, nil, http.StatusOK, &out); err != nil {
		return View{}, fmt.Errorf("end credits session %s: %w", id, err)
	}
	return out.Session, nil
}

// problem is the RFC 9457 body the registry refuses with.
type problem struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int, out any) error {
	token, err := c.signIn()
	if err != nil {
		return err
	}
	if token == "" {
		return ErrSignedOut
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.registry+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode == want {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("the registry's answer is not a session: %w", err)
		}
		return nil
	}
	var p problem
	_ = json.Unmarshal(raw, &p)
	why := p.Detail
	if why == "" {
		why = p.Title
	}
	if why == "" {
		why = http.StatusText(res.StatusCode)
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s (sign in again with agentry login)", ErrSignedOut, why)
	case http.StatusPaymentRequired:
		return fmt.Errorf("%w: %s", ErrNoCredits, why)
	case http.StatusGone:
		return fmt.Errorf("%w: %s", ErrEnded, why)
	}
	return fmt.Errorf("%s (HTTP %d)", why, res.StatusCode)
}
