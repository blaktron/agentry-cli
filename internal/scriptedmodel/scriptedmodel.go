// Package scriptedmodel is a model server for tests. A test starts one on a
// local port, gives it a script of replies, points a model client at it and
// then reads back every request the client made.
//
// It answers as two wire APIs, chosen by the request's path: the Anthropic
// Messages API (a path ending in /messages) and OpenAI-style Chat Completions
// (a path ending in /chat/completions). Each request takes the next step of
// the script. A step is a reply (text, tool calls, reasoning, usage) or a
// failure a real server produces: an answer cut at the output limit, an HTTP
// error, a refusal of a conversation that is too long worded as a given
// server words it, a stream that stalls, a connection that drops.
//
// The package uses the standard library only and makes no outside
// connection.
package scriptedmodel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// API names the wire API a request used.
type API int

const (
	// Unknown is a request to a path the server does not serve.
	Unknown API = iota
	// Anthropic is the Anthropic Messages API.
	Anthropic
	// ChatCompletions is OpenAI-style Chat Completions.
	ChatCompletions
)

func (a API) String() string {
	switch a {
	case Anthropic:
		return "anthropic"
	case ChatCompletions:
		return "chat-completions"
	}
	return "unknown"
}

// Server plays a script of replies and records the requests it receives.
type Server struct {
	t    testing.TB
	http *httptest.Server
	quit chan struct{}

	mu        sync.Mutex
	steps     []Step
	served    int
	requests  []Request
	exhausted int
	closed    bool
}

// New starts a server that plays steps in order, one a request, and closes
// it when the test ends. A request that arrives after the last step is
// answered with a 500 and fails the test when it ends.
func New(t testing.TB, steps ...Step) *Server {
	t.Helper()
	s := &Server{t: t, quit: make(chan struct{}), steps: append([]Step(nil), steps...)}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.Close()
		s.mu.Lock()
		n := s.exhausted
		s.mu.Unlock()
		if n > 0 {
			t.Errorf("scriptedmodel: %d request(s) arrived after the script's last step", n)
		}
	})
	return s
}

// URL is the server's base address, with no trailing slash. A client's
// Anthropic base URL is URL() and its Chat Completions base URL is
// URL()+"/v1".
func (s *Server) URL() string { return s.http.URL }

// Script appends steps to the script.
func (s *Server) Script(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, steps...)
}

// Remaining is the number of steps no request has taken yet.
func (s *Server) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps) - s.served
}

// Requests returns every request received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Close ends every stalled stream and stops the server. It is safe to call
// more than once.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.quit)
	s.http.CloseClientConnections()
	s.http.Close()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := parseRequest(r, body)

	s.mu.Lock()
	s.requests = append(s.requests, req)
	var step Step
	have := req.API != Unknown && s.served < len(s.steps)
	if have {
		step = s.steps[s.served]
		s.served++
	} else if req.API != Unknown {
		s.exhausted++
	}
	n := len(s.requests)
	s.mu.Unlock()

	switch {
	case req.API == Unknown:
		http.Error(w, "scriptedmodel: no such path: "+r.URL.Path, http.StatusNotFound)
	case !have:
		http.Error(w, "scriptedmodel: the script has no step left", http.StatusInternalServerError)
	default:
		step.play(&exchange{w: w, r: r, req: req, n: n, quit: s.quit})
	}
}

// exchange is one request being answered.
type exchange struct {
	w    http.ResponseWriter
	r    *http.Request
	req  Request
	n    int // the request's number, from 1
	quit <-chan struct{}
}

// sse starts an event stream.
func (x *exchange) sse() {
	h := x.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	x.w.WriteHeader(http.StatusOK)
	x.flush()
}

// event writes one server-sent event. An empty name writes a data-only event.
func (x *exchange) event(name, data string) {
	var b strings.Builder
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	b.WriteString("data: " + data + "\n\n")
	_, _ = io.WriteString(x.w, b.String())
	x.flush()
}

func (x *exchange) flush() {
	if f, ok := x.w.(http.Flusher); ok {
		f.Flush()
	}
}

// json writes a whole JSON response.
func (x *exchange) json(status int, body string) {
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(status)
	_, _ = io.WriteString(x.w, body)
}

// hold blocks until the client goes away or the server closes.
func (x *exchange) hold() {
	select {
	case <-x.r.Context().Done():
	case <-x.quit:
	}
}

// drop closes the connection under the response, so the client reads an
// unexpected end of stream.
func (x *exchange) drop() {
	if hj, ok := x.w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// halves splits s in two at a rune boundary, so a stream carries it as two
// deltas and a client has to join them.
func halves(s string) (string, string) {
	r := []rune(s)
	if len(r) < 2 {
		return s, ""
	}
	return string(r[:len(r)/2]), string(r[len(r)/2:])
}
