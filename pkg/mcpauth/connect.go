package mcpauth

import (
	"bytes"
	"context"
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

// NativeClientMetadataURL is Agentry's Client ID Metadata Document for the
// CLI and the desktop ("Agentry Desktop", loopback redirects), served by
// the site (agentry-web#1180). A CIMD's client id is its own address.
const NativeClientMetadataURL = "https://secureagentry.net/oauth/clients/agentry-desktop.json"

// NativeClientName is the name a dynamically registered client gives.
const NativeClientName = "Agentry Desktop"

// CallbackPath is the loopback redirect's path.
const CallbackPath = "/callback"

// ConsentTimeout bounds how long a consent may wait on the person.
const ConsentTimeout = 10 * time.Minute

// NeedsOwnClientError is a vendor that takes no client Agentry can present
// for itself (no CIMD, no dynamic registration), so the person's own OAuth
// client is the way (D16, D23): Google is the first.
type NeedsOwnClientError struct{ Issuer string }

func (e *NeedsOwnClientError) Error() string {
	return fmt.Sprintf("%s registers no client on the fly, so a sign-in there needs your own OAuth client: make one at the vendor, then enter its client id and secret", e.Issuer)
}

// Consent is one sign-in to a remote server.
type Consent struct {
	// HTTP reaches the vendor; Guard{}.Client() in production.
	HTTP *http.Client
	// ServerURL is the remote server's address; Scopes the manifest's.
	ServerURL string
	Scopes    []string
	// OwnClient is the person's own client for an issuer, or nil for none.
	OwnClient func(issuer string) (*Client, error)
	// CIMDURL is Agentry's client metadata document; "" leaves it out.
	CIMDURL string
	// Present shows the consent address and, where it can, opens it.
	Present func(authURL string)
	// Pasted carries redirect addresses the person pasted back (D21); nil
	// where none can be.
	Pasted <-chan string
	// Say tells the person something mid-consent.
	Say func(string)
	// Timeout bounds the wait; ConsentTimeout when zero.
	Timeout time.Duration
}

// Grant is a finished consent: what to keep, and the first access token.
type Grant struct {
	Discovery Discovery
	Client    Client
	// Scopes are what was asked; Token.Scopes what was granted, if said.
	Scopes []string
	Token  Token
}

// Run discovers the server, picks a client, opens consent, and exchanges
// the code the redirect brings back.
func (c Consent) Run(ctx context.Context) (*Grant, error) {
	d, err := DiscoverRemote(ctx, c.HTTP, c.ServerURL)
	if err != nil {
		return nil, err
	}
	lb, err := Listen()
	if err != nil {
		return nil, err
	}
	defer lb.Close()
	client, err := c.pickClient(ctx, d.Server, lb.RedirectURI)
	if err != nil {
		return nil, err
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = d.ChallengeScopes
	}
	if len(scopes) == 0 {
		scopes = d.ResourceScopes
	}
	req, err := NewRequest(d.Server, client, lb.RedirectURI, d.Resource, scopes)
	if err != nil {
		return nil, err
	}
	lb.Expect(req)
	if c.Present != nil {
		c.Present(req.URL())
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = ConsentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := c.wait(ctx, lb, req)
	if err != nil {
		return nil, err
	}
	tok, err := Exchange(ctx, c.HTTP, req, code)
	if err != nil {
		return nil, fmt.Errorf("finish the sign-in to %s: %w", c.ServerURL, err)
	}
	return &Grant{Discovery: *d, Client: client, Scopes: scopes, Token: *tok}, nil
}

// wait takes the first good answer from the loopback listener or the
// pasted addresses; a pasted address that is not this sign-in's is said
// and waited past.
func (c Consent) wait(ctx context.Context, lb *Loopback, req *Request) (string, error) {
	for {
		select {
		case o := <-lb.result:
			return o.code, o.err
		case line, ok := <-c.Pasted:
			if !ok {
				c.Pasted = nil
				continue
			}
			u, err := url.Parse(strings.TrimSpace(line))
			if err != nil || u.RawQuery == "" {
				c.say("that is not the address the browser was sent to; paste the whole address from its address bar")
				continue
			}
			code, err := req.Callback(u)
			if errors.Is(err, ErrStateMismatch) {
				c.say(err.Error())
				continue
			}
			return code, err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("the sign-in timed out waiting for the browser; start it again")
			}
			return "", ctx.Err()
		}
	}
}

func (c Consent) say(s string) {
	if c.Say != nil {
		c.Say(s)
	}
}

// pickClient is the client a sign-in presents (plan §6, D16, D23): the
// person's own for this issuer where they keep one, else Agentry's CIMD
// where the vendor takes one and the document is served, else a dynamic
// registration, else the person must make their own.
func (c Consent) pickClient(ctx context.Context, md ServerMetadata, redirectURI string) (Client, error) {
	if c.OwnClient != nil {
		own, err := c.OwnClient(md.Issuer)
		if err != nil {
			return Client{}, err
		}
		if own != nil {
			cl := *own
			cl.Kind = ClientOwn
			if cl.AuthMethod == "" {
				cl.AuthMethod = secretMethod(md, cl.Secret)
			}
			return cl, nil
		}
	}
	if c.CIMDURL != "" && md.CIMDSupported && supportsNone(md) {
		if CIMDServed(ctx, c.HTTP, c.CIMDURL) {
			return Client{ID: c.CIMDURL, Kind: ClientCIMD, AuthMethod: "none"}, nil
		}
	}
	if md.RegistrationEndpoint != "" {
		return Register(ctx, c.HTTP, md, redirectURI)
	}
	return Client{}, &NeedsOwnClientError{Issuer: md.Issuer}
}

// supportsNone reports whether the token endpoint takes a public client;
// a server that lists no methods defaults to client_secret_basic (RFC
// 8414), which a public client cannot use.
func supportsNone(md ServerMetadata) bool {
	for _, m := range md.TokenEndpointAuthMethodsSupported {
		if m == "none" {
			return true
		}
	}
	return false
}

// secretMethod is how the person's own client authenticates: post where
// the server lists it or lists nothing (Google's documented way), else
// basic; none for a client with no secret.
func secretMethod(md ServerMetadata, secret string) string {
	if secret == "" {
		return "none"
	}
	if len(md.TokenEndpointAuthMethodsSupported) == 0 {
		return "client_secret_post"
	}
	for _, m := range md.TokenEndpointAuthMethodsSupported {
		if m == "client_secret_post" {
			return m
		}
	}
	return "client_secret_basic"
}

// CIMDServed reports whether Agentry's client metadata document is served
// at its address and names itself there, with a loopback redirect: a
// vendor fetching it would otherwise refuse the sign-in in the browser,
// past where the CLI can fall back to registering.
func CIMDServed(ctx context.Context, c *http.Client, docURL string) bool {
	var doc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := getJSON(ctx, c, docURL, &doc); err != nil || doc.ClientID != docURL {
		return false
	}
	for _, r := range doc.RedirectURIs {
		if u, err := url.Parse(r); err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Path == CallbackPath {
			return true
		}
	}
	return false
}

// Register registers a public native client for this redirect (RFC 7591).
func Register(ctx context.Context, c *http.Client, md ServerMetadata, redirectURI string) (Client, error) {
	method := "none"
	if !supportsNone(md) && len(md.TokenEndpointAuthMethodsSupported) > 0 {
		method = secretMethod(md, "registered")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                NativeClientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"application_type":           "native",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return Client{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return Client{}, fmt.Errorf("register with %s: %w", md.Issuer, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return Client{}, fmt.Errorf("register with %s: %w", md.Issuer, oauthError(res.StatusCode, raw))
	}
	var out struct {
		ClientID   string `json:"client_id"`
		Secret     string `json:"client_secret"`
		AuthMethod string `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ClientID == "" {
		return Client{}, fmt.Errorf("register with %s: the answer carries no client id", md.Issuer)
	}
	cl := Client{ID: out.ClientID, Secret: out.Secret, Kind: ClientDCR, AuthMethod: out.AuthMethod}
	if cl.AuthMethod == "" {
		cl.AuthMethod = method
	}
	if cl.Secret == "" {
		cl.AuthMethod = "none"
	}
	return cl, nil
}

// donePage is what the browser shows once the redirect came back.
const donePage = `<!doctype html><meta charset="utf-8"><title>Connected</title>
<body style="font-family:system-ui;background:#141210;color:#e7e5e4;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1 style="font-weight:600">Connected</h1>
<p>You can close this tab and return to Agentry.</p></div>`

// Loopback is the one-shot redirect listener on 127.0.0.1.
type Loopback struct {
	RedirectURI string

	srv    *http.Server
	mu     sync.Mutex
	req    *Request
	result chan outcome
	once   sync.Once
}

type outcome struct {
	code string
	err  error
}

// Listen binds 127.0.0.1:0 and serves CallbackPath.
func Listen() (*Loopback, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind the loopback listener: %w", err)
	}
	l := &Loopback{
		RedirectURI: fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, CallbackPath),
		result:      make(chan outcome, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, l.handle)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// Expect sets the request whose redirect the listener takes.
func (l *Loopback) Expect(r *Request) {
	l.mu.Lock()
	l.req = r
	l.mu.Unlock()
}

func (l *Loopback) handle(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	req := l.req
	l.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if req == nil {
		http.Error(w, "no sign-in is waiting here", http.StatusNotFound)
		return
	}
	code, err := req.Callback(r.URL)
	if errors.Is(err, ErrStateMismatch) {
		// Anything on this machine can reach the port: a request without
		// the state ends nothing (agentry-cli#164's rule).
		http.Error(w, "this is not the sign-in Agentry started", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "the sign-in did not complete; return to Agentry", http.StatusBadRequest)
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(donePage))
	}
	l.once.Do(func() { l.result <- outcome{code: code, err: err} })
}

// Close stops the listener.
func (l *Loopback) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.srv.Shutdown(ctx)
}
