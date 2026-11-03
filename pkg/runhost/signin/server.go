// Package signin is a sign-in made for a remote runhost: Claude Code's own
// sign-in, Sign in with ChatGPT and Sign in with GitHub for a Copilot plan,
// and an OAuth connection of the runhost's own to a release's server, each
// run on the runhost while the person's browser is on the device.
//
// A connection is the runhost's own grant, never a copy of the device's:
// the consent runs on the runhost as mcpauth.Consent does on a computer,
// its redirect relayed like ChatGPT's, and the runhost keeps the refresh
// token it is issued in its connection store, whose secrets are entries of
// its encrypted store. The device opens the consent only at the
// authorization endpoint it discovered for the server itself.
//
// The sign-in runs on the server exactly as it runs on a person's computer
// (claudesignin, modelsignin), into the runhost's own sign-in store: Claude
// Code keeps its credential in its docker volume there and refreshes it
// itself; ChatGPT's and Copilot's refresh tokens are kept in the runhost's
// encrypted store. Nothing of the sign-in is kept on the device. The device
// is asked to open each address in the person's browser, and where the
// browser's answer is a redirect to a loopback callback, the device listens
// on that port of its own loopback and relays each connection, byte for
// byte and unread, over a run key session of its own to the callback on the
// runhost: into Claude Code's container through exec, or to ChatGPT's
// loopback listener on the server. GitHub's device flow needs no relay: the
// device shows the code and opens GitHub's page.
package signin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/claudesignin"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Vendors are the sign-ins a runhost takes, in the order they are offered.
var Vendors = []string{modelsignin.Claude, modelsignin.ChatGPT, modelsignin.Copilot}

// IsVendor says whether v is a sign-in a runhost takes.
func IsVendor(v string) bool {
	for _, x := range Vendors {
		if x == v {
			return true
		}
	}
	return false
}

// MaxRelayed bounds the connections relayed at once for one sign-in: a
// browser opens one or two for a callback.
const MaxRelayed = 8

// Kept is a sign-in the runhost keeps: never a token.
type Kept struct {
	Vendor string `json:"vendor"`
	Kind   string `json:"kind"`
	Label  string `json:"label,omitempty"`
	// KeptAt is when it was kept, RFC 3339.
	KeptAt string `json:"keptAt,omitempty"`
	// Broken says why the vendor no longer honours it, when it does not.
	Broken string `json:"broken,omitempty"`
}

// Removed is what removing a sign-in did.
type Removed struct {
	Vendor  string `json:"vendor"`
	Removed bool   `json:"removed"`
	// Outcome is the store's word on what the vendor was told.
	Outcome string `json:"outcome,omitempty"`
}

// Server runs sign-ins on the runhost, one at a time, for devices that
// relay the browser's answer to it.
type Server struct {
	// Home is the runhost account's runner home, whose sign-in store the
	// runhost's runs read.
	Home string
	// Keys keeps the refresh tokens: the runhost's encrypted store.
	Keys connection.Keychain
	// PolicyPath is the runhost's own policy file, which Claude Code's image
	// is prepared under.
	PolicyPath string
	// Docker is the server's daemon; nil is dockerx.New.
	Docker func() dockerx.Client
	// ChatGPT and GitHub are the vendors' flows, with only their HTTP client
	// and endpoints set; the server sets the rest.
	ChatGPT func() mcpauth.ChatGPT
	GitHub  func() mcpauth.GitHub
	// Dial reaches a loopback callback on the server; nil is net.Dialer's.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// Connections are the runhost's connection store and its store of the
	// person's own OAuth clients, which its schedules' connections are kept
	// in; nil for a runhost that keeps none.
	Connections func() (*connection.Store, *connection.Clients)
	// ConnectHTTP reaches a connection's vendor; nil is mcpauth.Guard's.
	ConnectHTTP func() *http.Client
	// CIMDURL is Agentry's client metadata document for a connection's
	// consent; "" leaves it out.
	CIMDURL string

	mu     sync.Mutex
	active *active
}

// active is the sign-in under way, and where its relays go.
type active struct {
	id     string
	vendor string
	ctx    context.Context
	// container and port are Claude Code's login container and its
	// callback; for ChatGPT, port is the flow's listener on the server's
	// loopback and container is empty.
	container string
	port      int
	slots     chan struct{}
}

func (s *Server) docker() dockerx.Client {
	if s.Docker == nil {
		return dockerx.New()
	}
	return s.Docker()
}

func (s *Server) store() *modelsignin.Store { return modelsignin.Open(s.Home, s.Keys) }

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Start runs vendor's sign-in for the device on the other end of in and fw,
// and answers with what it kept. app is GitHub's app for Copilot ("" for
// the GitHub App). The device's leaving (in ends) ends the sign-in.
func (s *Server) Start(ctx context.Context, vendor, app string, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if !IsVendor(vendor) {
		return engine.ExitUsage, fmt.Errorf("a runhost signs in to %s, not %q", strings.Join(Vendors, ", "), vendor)
	}
	id, err := newID()
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := &active{id: id, vendor: vendor, ctx: ctx, slots: make(chan struct{}, MaxRelayed)}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		return engine.ExitUsage, errors.New("another sign-in is under way on this runhost; try again when it has finished")
	}
	s.active = a
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
	}()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()
	var rec *modelsignin.Record
	switch vendor {
	case modelsignin.Claude:
		rec, err = s.claude(ctx, a, fw)
	case modelsignin.ChatGPT:
		rec, err = s.chatgpt(ctx, a, fw)
	case modelsignin.Copilot:
		rec, err = s.copilot(ctx, app, fw)
	}
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return 1, errors.New("nothing kept: the device left before the sign-in finished")
		}
		return exitOf(err), err
	}
	return 0, fw.Result(keptOf(rec))
}

// exitOf is err's exit class, 1 when it has none.
func exitOf(err error) int {
	var c *engine.ClassifiedError
	if errors.As(err, &c) {
		return c.Code
	}
	return 1
}

func keptOf(r *modelsignin.Record) Kept {
	return Kept{Vendor: r.Vendor, Kind: r.Kind, Label: r.Label, KeptAt: r.CreatedAt, Broken: r.Broken}
}

// lines writes what it is given to the device as narration, a line a frame.
type lines struct {
	fw  *runhost.FrameWriter
	buf []byte
}

func (l *lines) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		if t := strings.TrimRight(string(l.buf[:i]), "\r"); strings.TrimSpace(t) != "" {
			_ = l.fw.Out("%s", t)
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 {
		l.buf = nil
	}
	return len(p), nil
}

// stepSentence is a Claude sign-in's step as the device is told it: the
// image is the runhost's.
func stepSentence(step claudesignin.Step, detail string) string {
	switch step {
	case claudesignin.StepBuild:
		if detail != "" {
			return "Building the Claude Code " + detail + " image on the runhost — the download and build take a few minutes…"
		}
		return "Building the Claude Code image on the runhost — the download and build take a few minutes…"
	case claudesignin.StepImage:
		return "Checking the Claude Code image on the runhost…"
	}
	return step.Sentence(detail)
}

func (s *Server) claude(ctx context.Context, a *active, fw *runhost.FrameWriter) (*modelsignin.Record, error) {
	pol, _, err := policy.Load(s.PolicyPath)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the runhost's policy: %w", err))
	}
	si := claudesignin.SignIn{Home: s.Home, Policy: pol, PolicyPath: s.PolicyPath, Docker: s.Docker, Keys: s.Keys,
		Relay: func(_ context.Context, container string, port int) (func(), error) {
			s.mu.Lock()
			a.container, a.port = container, port
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				a.container, a.port = "", 0
				s.mu.Unlock()
			}, nil
		}}
	out := &lines{fw: fw}
	p := claudesignin.Person{
		Out: out,
		Open: func(u string) {
			s.mu.Lock()
			port := a.port
			s.mu.Unlock()
			_ = fw.Frame(openFrame(u, runhost.Open{Relay: a.id, Port: port}))
		},
		Step: func(step claudesignin.Step, detail string) { _ = fw.Out("%s", stepSentence(step, detail)) },
	}
	return si.Run(ctx, p, false)
}

func (s *Server) chatgpt(ctx context.Context, a *active, fw *runhost.FrameWriter) (*modelsignin.Record, error) {
	var flow mcpauth.ChatGPT
	if s.ChatGPT != nil {
		flow = s.ChatGPT()
	}
	flow.Say = func(t string) { _ = fw.Out("%s", t) }
	flow.Present = func(u string) {
		port, ok := loopbackRedirect(u, mcpauth.ChatGPTCallbackPath)
		if !ok {
			_ = fw.Out("ChatGPT's address names no callback on the runhost's loopback; nothing can be relayed")
			return
		}
		s.mu.Lock()
		a.port = port
		s.mu.Unlock()
		_ = fw.Frame(openFrame(u, runhost.Open{Relay: a.id, Port: port}))
	}
	got, err := s.store().SignInChatGPT(ctx, flow, false)
	if err != nil {
		return nil, err
	}
	return got.Record, nil
}

func (s *Server) copilot(ctx context.Context, app string, fw *runhost.FrameWriter) (*modelsignin.Record, error) {
	if app == "" {
		app = mcpauth.GitHubApp
	}
	var flow mcpauth.GitHub
	if s.GitHub != nil {
		flow = s.GitHub()
	}
	flow.Present = func(page, code string) {
		_ = fw.Frame(openFrame(page, runhost.Open{Code: code}))
	}
	got, err := s.store().SignInCopilot(ctx, flow, app)
	if err != nil {
		return nil, err
	}
	return got.Record, nil
}

func openFrame(u string, o runhost.Open) runhost.Frame {
	b, _ := json.Marshal(o)
	return runhost.Frame{Type: runhost.FrameOpen, Text: u, Result: b}
}

// loopbackRedirect is the port of an authorization address whose
// redirect_uri is http://127.0.0.1:<port><path> or localhost's.
func loopbackRedirect(raw, path string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	r, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil || r.Scheme != "http" || (r.Hostname() != "127.0.0.1" && r.Hostname() != "localhost") || r.Path != path || r.User != nil {
		return 0, false
	}
	p, err := strconv.Atoi(r.Port())
	if err != nil || p < 1024 || p > 65535 {
		return 0, false
	}
	return p, true
}

// Relay carries one browser connection, in from the device and out in data
// frames, to the callback of the sign-in id names. It returns when either
// side closes, or when the sign-in ends.
func (s *Server) Relay(ctx context.Context, id string, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	s.mu.Lock()
	a := s.active
	var container string
	var port int
	if a != nil && a.id == id {
		container, port = a.container, a.port
	}
	s.mu.Unlock()
	if a == nil || a.id != id || port == 0 {
		return engine.ExitUsage, errors.New("no sign-in on this runhost is waiting for that answer")
	}
	select {
	case a.slots <- struct{}{}:
	default:
		return 1, errors.New("too many connections at once for one sign-in")
	}
	defer func() { <-a.slots }()
	rctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	out := dataWriter{fw: fw}
	if a.vendor == modelsignin.Claude {
		ex, ok := s.docker().(dockerx.Execer)
		if !ok {
			return 1, errors.New("this runhost's docker client cannot relay into a container")
		}
		if err := claudesignin.Forward(rctx, ex, container, port, in, out); err != nil && rctx.Err() == nil {
			return 1, err
		}
		return 0, nil
	}
	dial := s.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp4", addr)
		}
	}
	dctx, dcancel := context.WithTimeout(rctx, 10*time.Second)
	conn, err := dial(dctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	dcancel()
	if err != nil {
		return 1, fmt.Errorf("reach the sign-in's callback on the runhost: %w", err)
	}
	defer func() { _ = conn.Close() }()
	go func() {
		<-rctx.Done()
		_ = conn.Close()
	}()
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(out, conn)
	return 0, nil
}

// dataWriter writes what it is given as data frames.
type dataWriter struct{ fw *runhost.FrameWriter }

func (d dataWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), runhost.DataChunk)
		if err := d.fw.Frame(runhost.Frame{Type: runhost.FrameData, Data: append([]byte(nil), p[:c]...)}); err != nil {
			return n, err
		}
		n, p = n+c, p[c:]
	}
	return n, nil
}

// List is the sign-ins the runhost keeps.
func (s *Server) List() ([]Kept, error) {
	recs, err := s.store().List()
	if err != nil {
		return nil, err
	}
	var out []Kept
	for _, r := range recs {
		if IsVendor(r.Vendor) {
			out = append(out, keptOf(r))
		}
	}
	return out, nil
}

// Remove ends vendor's sign-in on the runhost: Claude Code's own logout and
// its volume deleted, or the refresh token revoked at the vendor where it
// can be and deleted from the store.
func (s *Server) Remove(ctx context.Context, vendor string, out io.Writer) (Removed, error) {
	if !IsVendor(vendor) {
		return Removed{}, fmt.Errorf("a runhost keeps sign-ins for %s, not %q", strings.Join(Vendors, ", "), vendor)
	}
	st := s.store()
	if _, err := st.Get(vendor); errors.Is(err, modelsignin.ErrNone) {
		return Removed{Vendor: vendor}, nil
	} else if err != nil {
		return Removed{}, err
	}
	if vendor == modelsignin.Claude {
		pol, _, err := policy.Load(s.PolicyPath)
		if err != nil {
			return Removed{}, err
		}
		st.EndClaude = claudesignin.SignIn{Home: s.Home, Policy: pol, PolicyPath: s.PolicyPath, Docker: s.Docker}.End(out)
	}
	outcome, err := st.Remove(ctx, vendor, "removed from the runhost by a paired device")
	if err != nil {
		return Removed{}, err
	}
	return Removed{Vendor: vendor, Removed: true, Outcome: outcome}, nil
}

// HasSignIn says whether the runhost keeps a working sign-in of kind, and
// if not, why.
func (s *Server) HasSignIn(kind string) error {
	vendor := modelsignin.VendorOf(kind)
	if !IsVendor(vendor) {
		return fmt.Errorf("a runhost does not sign in for %q", kind)
	}
	r, err := s.store().Get(vendor)
	if errors.Is(err, modelsignin.ErrNone) {
		return fmt.Errorf("this runhost keeps no %s sign-in", vendor)
	}
	if err != nil {
		return err
	}
	if r.Broken != "" {
		return fmt.Errorf("this runhost's %s sign-in no longer works (%s)", vendor, r.Broken)
	}
	if vendor == modelsignin.Claude && r.ConfigDir == "" {
		return errors.New("this runhost keeps no sign-in for the Claude Code image")
	}
	return nil
}
