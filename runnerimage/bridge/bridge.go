// Package bridge is the runner image's bridge role: a stdio MCP server
// the gateway launches like any declared server, which forwards each
// message to one remote MCP server over streamable HTTP, or over the older
// HTTP+SSE transport where that is all the server speaks.
//
// It exists because the pinned gateway would dial a remote server from its
// own process, and the gateway has no network leg. The bridge
// runs in its own container on the gateway's relay network, so the remote
// host is reached through that host's digest-pinned relay and nothing else,
// under the run's docker filter and the policy's runtime.
//
// Its credential is never in its environment and never in the gateway's
// secrets file: it is read from a file in a run-owned folder, mounted
// read-only, at every request, so a token the runner renews mid-run is used
// at once. A 401 or 403 ends that one call with a sentence naming the
// server; the session goes on.
//
// Stdlib only: the package is embedded in the runner image's build context.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// MaxMessage bounds one JSON-RPC message in either direction: a line
	// from the gateway, a response body, or one SSE event's data.
	MaxMessage = 16 << 20
	// MaxToken bounds the bearer file; a token is far shorter.
	MaxToken = 16 << 10
	// maxInFlight bounds the calls forwarded at once.
	maxInFlight = 16
	// codeRemote is the JSON-RPC error code of a call the remote server or
	// the way to it refused (the implementation-defined server range).
	codeRemote = -32001
	// protocolHeader and sessionHeader are streamable HTTP's two headers.
	protocolHeader = "MCP-Protocol-Version"
	sessionHeader  = "Mcp-Session-Id"
)

// Config is one bridge.
type Config struct {
	// URL is the remote server's https address.
	URL string
	// TokenFile, when set, holds the bearer value, read at every request.
	// An empty file sends no Authorization header (an optional secret the
	// run goes without). Unset, the server is declared auth: none.
	TokenFile string
	// OAuth says the token is a connection's OAuth access token (auth:
	// oauth), not a secret the person set: a 401 then names the connection
	// and says a new run is given a fresh token.
	OAuth bool
	// Client is the HTTP client; nil builds one that takes no proxy from
	// the environment and follows no redirect.
	Client *http.Client
	// Lost is called when the HTTP+SSE transport's event stream ends: the
	// only way the server's answers come back is gone, and the gateway must
	// see a dead server. Nil logs and exits the process.
	Lost func(error)
	// Log receives diagnostics (the container's stderr, which the gateway
	// relays into its log). Never a token, never a message body.
	Log io.Writer
}

// Bridge is a running bridge.
type Bridge struct {
	cfg    Config
	target *url.URL
	host   string
	client *http.Client

	outMu sync.Mutex
	out   io.Writer

	mu       sync.Mutex
	session  string // streamable HTTP's session id, once the server names one
	protocol string // the protocol version initialize agreed
	endpoint string // the legacy transport's POST address; "" for streamable
	inFlight sync.WaitGroup
	slots    chan struct{}
}

// New checks the configuration and builds a bridge writing to out.
func New(cfg Config, out io.Writer) (*Bridge, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("the remote server's address does not parse: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("the remote server's address must be https://host/…, with no user information or fragment")
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	client := cfg.Client
	if client == nil {
		client = DefaultClient()
	}
	return &Bridge{cfg: cfg, target: u, host: u.Hostname(), client: client, out: out, slots: make(chan struct{}, maxInFlight)}, nil
}

// DefaultClient is the bridge's HTTP client: no proxy from the environment
// (the relay is reached by the host's own name), TLS 1.2 or later, bounded
// connection set-up, and no redirect followed, since a redirect could carry
// the call, and its bearer, to an address no one declared.
func DefaultClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   30 * time.Second,
			MaxIdleConnsPerHost:   maxInFlight,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Serve forwards messages from in until it closes (the gateway closing
// stdin is the shutdown signal), then ends the remote session.
func (b *Bridge) Serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), MaxMessage)
	first := true
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		var head struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(msg, &head); err != nil {
			b.logf("dropping a line from the gateway that is not JSON: %v", err)
			continue
		}
		if first || head.Method == "initialize" {
			// The handshake goes first and alone: it decides the transport
			// and names the session every later call carries.
			first = false
			if err := b.initialize(ctx, msg, head.ID); err != nil {
				return err
			}
			continue
		}
		b.slots <- struct{}{}
		b.inFlight.Add(1)
		go func() {
			defer func() { <-b.slots; b.inFlight.Done() }()
			b.forward(ctx, msg, head.ID)
		}()
	}
	b.inFlight.Wait()
	b.endSession()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read the gateway's stream: %w", err)
	}
	return nil
}

// initialize sends the handshake. A server that refuses the POST with 400,
// 404 or 405 before any session exists speaks the older HTTP+SSE transport
// (the MCP specification's backwards-compatibility rule): the bridge opens
// its event stream and sends everything to the address it names.
func (b *Bridge) initialize(ctx context.Context, msg []byte, id json.RawMessage) error {
	res, err := b.post(ctx, b.cfg.URL, msg)
	if err != nil {
		b.reply(id, err)
		return nil
	}
	switch res.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed:
		drain(res)
		b.logf("%s refused a streamable HTTP initialize (%s); trying the HTTP+SSE transport", b.host, res.Status)
		if err := b.openLegacy(ctx); err != nil {
			return err
		}
		res, err = b.post(ctx, b.endpoint, msg)
		if err != nil {
			b.reply(id, err)
			return nil
		}
		b.legacyAnswer(res, id)
		return nil
	}
	if s := res.Header.Get(sessionHeader); s != "" {
		b.mu.Lock()
		b.session = s
		b.mu.Unlock()
	}
	b.answer(res, id, true)
	return nil
}

// forward sends one message on whichever transport initialize chose.
func (b *Bridge) forward(ctx context.Context, msg []byte, id json.RawMessage) {
	b.mu.Lock()
	endpoint := b.endpoint
	b.mu.Unlock()
	if endpoint != "" {
		res, err := b.post(ctx, endpoint, msg)
		if err != nil {
			b.reply(id, err)
			return
		}
		b.legacyAnswer(res, id)
		return
	}
	res, err := b.post(ctx, b.cfg.URL, msg)
	if err != nil {
		b.reply(id, err)
		return
	}
	b.answer(res, id, false)
}

// post sends one message with the run's headers. Its error is already the
// sentence a call ends with.
func (b *Bridge) post(ctx context.Context, address string, msg []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(msg))
	if err != nil {
		return nil, b.failed("could not form the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if err := b.authorize(req); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.session != "" {
		req.Header.Set(sessionHeader, b.session)
	}
	if b.protocol != "" {
		req.Header.Set(protocolHeader, b.protocol)
	}
	b.mu.Unlock()
	res, err := b.client.Do(req)
	if err != nil {
		return nil, b.failed("could not be reached: %v", scrub(err))
	}
	return res, nil
}

// authorize reads the bearer file, now: a value the runner rewrote since
// the last call is the one this call carries.
func (b *Bridge) authorize(req *http.Request) error {
	if b.cfg.TokenFile == "" {
		return nil
	}
	f, err := os.Open(b.cfg.TokenFile)
	if err != nil {
		return b.failed("has no credential to send: the run's credential file cannot be read (%v)", errors.Unwrap(err))
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxToken+1))
	_ = f.Close()
	if err != nil {
		return b.failed("has no credential to send: the run's credential file cannot be read")
	}
	if len(raw) > MaxToken {
		return b.failed("has no credential to send: the run's credential file is longer than %d bytes", MaxToken)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil // an optional secret the run goes without
	}
	for _, c := range token {
		if c < 0x21 || c > 0x7e {
			return b.failed("has no credential to send: the value the run was given is not a single printable token")
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// answer passes a streamable HTTP response on to the gateway: a JSON body
// as it is (each message of a batch on its own line), an event stream event
// by event until it ends. A refusal ends the call it answers.
func (b *Bridge) answer(res *http.Response, id json.RawMessage, initialize bool) {
	defer drain(res)
	if err := b.refusal(res); err != nil {
		b.reply(id, err)
		return
	}
	if res.StatusCode == http.StatusAccepted || res.StatusCode == http.StatusNoContent {
		return // a notification or a response, accepted
	}
	ct := res.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		// The stream carries the call's answer, and before it whatever the
		// server sends meanwhile (a progress notification, a request of its
		// own); it ends once the answer is sent.
		answered := false
		err := readEvents(res.Body, func(event string, data []byte) bool {
			if event == "" || event == "message" {
				if answers(data, id) {
					answered = true
				}
				b.emit(data, initialize, id)
			}
			return !answered
		})
		switch {
		case answered || len(id) == 0:
		case errors.Is(err, io.EOF):
			b.reply(id, b.failed("ended its answer's event stream without an answer"))
		default:
			b.reply(id, b.failed("stopped answering part-way: %v", scrub(err)))
		}
	default:
		body, err := io.ReadAll(io.LimitReader(res.Body, MaxMessage+1))
		if err != nil {
			b.reply(id, b.failed("stopped answering part-way: %v", scrub(err)))
			return
		}
		if len(body) > MaxMessage {
			b.reply(id, b.failed("answered with more than %d bytes", MaxMessage))
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) == 0 {
			if len(id) != 0 {
				b.reply(id, b.failed("answered %s with no message", res.Status))
			}
			return
		}
		if body[0] == '[' {
			var batch []json.RawMessage
			if err := json.Unmarshal(body, &batch); err != nil {
				b.reply(id, b.failed("answered with something that is not JSON-RPC"))
				return
			}
			for _, m := range batch {
				b.emit(m, initialize, id)
			}
			return
		}
		b.emit(body, initialize, id)
	}
}

// legacyAnswer reads the HTTP+SSE transport's acknowledgement of a POST:
// the answer itself arrives on the event stream.
func (b *Bridge) legacyAnswer(res *http.Response, id json.RawMessage) {
	defer drain(res)
	if err := b.refusal(res); err != nil {
		b.reply(id, err)
	}
}

// refusal is the sentence a non-success status ends a call with.
func (b *Bridge) refusal(res *http.Response) error {
	switch {
	case res.StatusCode >= 200 && res.StatusCode <= 299:
		return nil
	case res.StatusCode == http.StatusUnauthorized && b.cfg.TokenFile == "":
		return b.failed("asks for a sign-in (HTTP 401), and this QuickAgent declares it with auth: none, so the run sends no credential")
	case res.StatusCode == http.StatusUnauthorized && b.cfg.OAuth:
		return b.failed("did not accept the access token of this run's connection to it (HTTP 401): the token expired during the run or the vendor withdrew the grant; a new run is given a fresh token, and if it is refused again, connect the server again")
	case res.StatusCode == http.StatusUnauthorized:
		return b.failed("did not accept the credential the run sent (HTTP 401): it may be wrong, expired or revoked; set it again and start a new run")
	case res.StatusCode == http.StatusForbidden:
		return b.failed("refused the call with the credential the run sent (HTTP 403): the credential may not allow it")
	case res.StatusCode == http.StatusNotFound && b.sessionID() != "":
		return b.failed("no longer knows this run's session (HTTP 404); start a new run")
	case res.StatusCode >= 300 && res.StatusCode <= 399:
		return b.failed("redirected the call (%s); the bridge follows no redirect, since it would carry the call to an address the QuickAgent did not declare", res.Status)
	}
	return b.failed("answered %s", res.Status)
}

// emit writes one message from the server to the gateway, one line, and
// learns the agreed protocol version from initialize's answer.
func (b *Bridge) emit(msg []byte, initialize bool, id json.RawMessage) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, msg); err != nil {
		b.logf("dropping a message from %s that is not JSON", b.host)
		return
	}
	if initialize {
		var res struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(compact.Bytes(), &res) == nil && bytes.Equal(res.ID, id) && res.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.protocol = res.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	compact.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(compact.Bytes())
}

// reply ends a call with err as a JSON-RPC error. A notification has no
// call to end: its failure is logged.
func (b *Bridge) reply(id json.RawMessage, err error) {
	if len(id) == 0 || string(id) == "null" {
		b.logf("%v", err)
		return
	}
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": codeRemote, "message": err.Error()},
	})
	b.logf("%v", err)
	b.emit(msg, false, nil)
}

// lost ends the bridge when the legacy event stream is gone.
func (b *Bridge) lost(err error) {
	if b.cfg.Lost != nil {
		b.cfg.Lost(err)
		return
	}
	b.logf("%v; the bridge stops", err)
	os.Exit(1)
}

// answers reports whether a message from the server is the response to
// the call id: it carries that id and no method.
func answers(msg []byte, id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal(msg, &m) == nil && m.Method == "" && bytes.Equal(bytes.TrimSpace(m.ID), bytes.TrimSpace(id))
}

// failed is a sentence that names the server.
func (b *Bridge) failed(format string, a ...any) error {
	return fmt.Errorf("the remote MCP server %s "+format, append([]any{b.host}, a...)...)
}

func (b *Bridge) sessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

// endSession tells a streamable HTTP server the session is over, as the
// specification asks a client to; best effort, bounded.
func (b *Bridge) endSession() {
	session := b.sessionID()
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.cfg.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set(sessionHeader, session)
	if b.authorize(req) != nil {
		return
	}
	if res, err := b.client.Do(req); err == nil {
		drain(res)
	}
}

// openLegacy opens the HTTP+SSE transport's event stream, waits for the
// endpoint event, and passes every later message on to the gateway. The
// endpoint must be on the server's own origin: the bridge's one relay
// reaches nothing else, and its bearer goes nowhere else.
func (b *Bridge) openLegacy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		return b.failed("could not form the request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if err := b.authorize(req); err != nil {
		return err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return b.failed("could not be reached: %v", scrub(err))
	}
	if err := b.refusal(res); err != nil {
		drain(res)
		return err
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		drain(res)
		return b.failed("speaks neither MCP transport: its address answered a POST with an error and a GET with %q", res.Header.Get("Content-Type"))
	}
	endpoint := make(chan string, 1)
	failed := make(chan error, 1)
	go func() {
		defer drain(res)
		got := false
		err := readEvents(res.Body, func(event string, data []byte) bool {
			if !got {
				if event != "endpoint" {
					return true
				}
				got = true
				endpoint <- string(bytes.TrimSpace(data))
				return true
			}
			if event == "" || event == "message" {
				b.emit(data, false, nil)
			}
			return true
		})
		if !got {
			failed <- b.failed("closed its event stream before naming where to send messages: %v", scrub(err))
			return
		}
		// The stream is the only way answers come back: without it the
		// server is gone, and the gateway must see a dead server.
		b.lost(b.failed("closed its event stream (%v)", scrub(err)))
	}()
	select {
	case e := <-endpoint:
		u, err := b.target.Parse(e)
		if err != nil || u.Scheme != b.target.Scheme || u.Host != b.target.Host || u.User != nil {
			return b.failed("named a message address on another origin (%q); the bridge sends nothing there", e)
		}
		b.mu.Lock()
		b.endpoint = u.String()
		b.mu.Unlock()
		return nil
	case err := <-failed:
		return err
	case <-time.After(30 * time.Second):
		return b.failed("named no message address within 30 seconds of opening its event stream")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readEvents reads a server-sent event stream, calling fn with each
// event's type ("" when unnamed) and its data lines joined, until the
// stream ends or fn returns false. An event past MaxMessage is an error.
func readEvents(r io.Reader, fn func(event string, data []byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxMessage)
	var event string
	var data []byte
	var have bool
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if have && !fn(event, data) {
				return nil
			}
			event, data, have = "", nil, false
			continue
		}
		if line[0] == ':' {
			continue // a comment, often a keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			if have {
				data = append(data, '\n')
			}
			data = append(data, value...)
			have = true
			if len(data) > MaxMessage {
				return fmt.Errorf("an event longer than %d bytes", MaxMessage)
			}
		}
	}
	if have {
		fn(event, data)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// scrub keeps a transport error's cause and drops the request address,
// which net/http repeats in every error.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// drain reads what is left of a body, bounded, so the connection can be
// reused, and closes it.
func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
}

func (b *Bridge) logf(format string, a ...any) {
	_, _ = fmt.Fprintf(b.cfg.Log, "agentry-runner bridge: "+format+"\n", a...)
}
