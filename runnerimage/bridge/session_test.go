package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// forgetful is a streamable HTTP server that names a new session at every
// initialize and can forget the sessions it named, as a server does when
// an idle session expires or it restarts.
type forgetful struct {
	mu        sync.Mutex
	sessions  int
	live      map[string]bool
	inits     []http.Header
	initsDone []string // the session each notifications/initialized carried
	// lostCalls answers every call 404 while still starting sessions, as
	// a server that forgets a session as soon as it names it.
	lostCalls bool
}

func (f *forgetful) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = map[string]bool{}
}

func (f *forgetful) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &msg)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = map[string]bool{}
	}
	if msg.Method == "initialize" {
		f.sessions++
		s := fmt.Sprintf("sess-%d", f.sessions)
		f.live[s] = true
		f.inits = append(f.inits, r.Header.Clone())
		w.Header().Set(sessionHeader, s)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"remote","version":"1"}}}`, msg.ID)
		return
	}
	s := r.Header.Get(sessionHeader)
	if !f.live[s] {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if r.Header.Get(protocolHeader) != "2025-06-18" {
		http.Error(w, "no protocol version", http.StatusBadRequest)
		return
	}
	if msg.Method == "notifications/initialized" {
		f.initsDone = append(f.initsDone, s)
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if f.lostCalls {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, msg.ID)
}

func serveLines(t *testing.T, b *Bridge) (send func(string), stop func()) {
	t.Helper()
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- b.Serve(context.Background(), inR) }()
	return func(line string) { _, _ = fmt.Fprintln(inW, line) }, func() {
		_ = inW.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestAForgottenSessionIsRenewedAndTheCallSentAgain(t *testing.T) {
	remote := &forgetful{}
	srv := tlsServer(t, remote)
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	send, stop := serveLines(t, b)
	send(initLine)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(t, &out, `"id":2`)
	remote.forget()
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`)
	waitFor(t, &out, `"id":3`)
	stop()

	msgs := out.messages(t)
	if len(msgs) != 3 {
		t.Fatalf("the gateway should see exactly its three answers, not the bridge's own initialize: %v", msgs)
	}
	for _, id := range []float64{1, 2, 3} {
		if m := byID(t, msgs, id); m["error"] != nil {
			t.Fatalf("id %v ended in an error: %v", id, m)
		}
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.inits) != 2 {
		t.Fatalf("want the first initialize and one renewal, the server saw %d", len(remote.inits))
	}
	if h := remote.inits[1]; h.Get(sessionHeader) != "" || h.Get(protocolHeader) != "" {
		t.Fatalf("the renewal's initialize carried the old session's headers: %v", h)
	}
	if got := strings.Join(remote.initsDone, ","); got != "sess-1,sess-2" {
		t.Fatalf("each session should be told the client is initialized once: %s", got)
	}
}

func TestCallsThatMeetOneForgottenSessionShareOneRenewal(t *testing.T) {
	remote := &forgetful{}
	srv := tlsServer(t, remote)
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	send, stop := serveLines(t, b)
	send(initLine)
	waitFor(t, &out, `"id":1`)
	remote.forget()
	for id := 10; id < 18; id++ {
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"x"}}`, id))
	}
	for id := 10; id < 18; id++ {
		waitFor(t, &out, fmt.Sprintf(`"id":%d`, id))
	}
	stop()
	msgs := out.messages(t)
	for id := 10; id < 18; id++ {
		if m := byID(t, msgs, float64(id)); m["error"] != nil {
			t.Fatalf("id %d ended in an error: %v", id, m)
		}
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.inits) != 2 {
		t.Fatalf("eight calls on one lost session started %d sessions after the first", len(remote.inits)-1)
	}
}

func TestASessionForgottenAgainEndsTheCallNamingTheServer(t *testing.T) {
	remote := &forgetful{}
	srv := tlsServer(t, remote)
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	send, stop := serveLines(t, b)
	send(initLine)
	waitFor(t, &out, `"id":1`)
	remote.mu.Lock()
	remote.lostCalls = true
	remote.mu.Unlock()
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}`)
	waitFor(t, &out, `"id":2`)
	stop()
	e, _ := byID(t, out.messages(t), 2)["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.HasPrefix(msg, "the remote MCP server 127.0.0.1 ") || !strings.Contains(msg, "forgot the session again") {
		t.Fatalf("a second 404 did not end the call naming the server: %v", e)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.inits) != 2 {
		t.Fatalf("one call should start one new session, not %d", len(remote.inits)-1)
	}
}

// droppable is an HTTP+SSE server whose event streams the test can close.
// Every GET opens a stream with a message address of its own; a call to the
// method "slow" is never answered.
type droppable struct {
	mu      sync.Mutex
	streams map[string]chan string
	opened  int
	inits   int
	drop    chan struct{}
	// refuse answers this many GETs after the first 503.
	refuse int
	// initialized counts notifications/initialized per stream; active is
	// the streams still open.
	initialized map[string]int
	active      int
	// refuseInitialized refuses notifications/initialized after the first
	// stream's.
	refuseInitialized bool
}

func (d *droppable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/mcp" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.URL.Path == "/mcp" && r.Method == http.MethodGet:
		d.mu.Lock()
		if d.opened > 0 && d.refuse > 0 {
			d.refuse--
			d.mu.Unlock()
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		d.opened++
		key := fmt.Sprint(d.opened)
		ch := make(chan string, 8)
		d.streams[key] = ch
		drop := d.drop
		d.active++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.active--
			d.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /messages?stream=%s\n\n", key)
		w.(http.Flusher).Flush()
		for {
			select {
			case msg := <-ch:
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				w.(http.Flusher).Flush()
			case <-drop:
				return
			case <-r.Context().Done():
				return
			}
		}
	case r.URL.Path == "/messages" && r.Method == http.MethodPost:
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		stream := r.URL.Query().Get("stream")
		d.mu.Lock()
		ch := d.streams[stream]
		if msg.Method == "initialize" {
			d.inits++
		}
		refuse := msg.Method == "notifications/initialized" && d.refuseInitialized && stream != "1"
		if msg.Method == "notifications/initialized" && !refuse {
			if d.initialized == nil {
				d.initialized = map[string]int{}
			}
			d.initialized[stream]++
		}
		d.mu.Unlock()
		if ch == nil {
			http.Error(w, "unknown stream", http.StatusNotFound)
			return
		}
		if refuse {
			http.Error(w, "not now", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if len(msg.ID) != 0 && msg.Method != "slow" {
			ch <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05"}}`, msg.ID)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestAClosedEventStreamEndsWaitingCallsAndTheNextCallOpensAnother(t *testing.T) {
	remote := &droppable{streams: map[string]chan string{}, drop: make(chan struct{})}
	srv := tlsServer(t, remote)
	var out, log lockedBuffer
	b := newBridge(t, srv, "", &out, &log)
	send, stop := serveLines(t, b)
	send(initLine)
	waitFor(t, &out, `"id":1`)
	send(`{"jsonrpc":"2.0","id":2,"method":"slow"}`)
	// The stream closes only once the call is out and waiting on it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		n := len(b.pending)
		b.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow call never went out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	remote.mu.Lock()
	close(remote.drop)
	remote.drop = make(chan struct{})
	remote.mu.Unlock()
	waitFor(t, &out, `"id":2`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	waitFor(t, &out, `"id":3`)
	stop()

	msgs := out.messages(t)
	e, _ := byID(t, msgs, 2)["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "closed its event stream before answering") {
		t.Fatalf("the call waiting on the closed stream was not ended: %v", msgs)
	}
	if m := byID(t, msgs, 3); m["error"] != nil {
		t.Fatalf("the call after the stream closed failed: %v", m)
	}
	if len(msgs) != 3 {
		t.Fatalf("the gateway should see exactly its three answers: %v", msgs)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.opened != 2 || remote.inits != 2 {
		t.Fatalf("want a second stream and an initialize on it; streams %d, initializes %d", remote.opened, remote.inits)
	}
	if remote.initialized["2"] != 1 {
		t.Fatalf("the second stream was not told the client is initialized: %v", remote.initialized)
	}
}

func TestAStreamThatCannotBeOpenedAgainEndsOneCallNotTheBridge(t *testing.T) {
	remote := &droppable{streams: map[string]chan string{}, drop: make(chan struct{}), refuse: 1}
	srv := tlsServer(t, remote)
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	send, stop := serveLines(t, b)
	send(initLine)
	waitFor(t, &out, `"id":1`)
	remote.mu.Lock()
	close(remote.drop)
	remote.drop = make(chan struct{})
	remote.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		closed := b.endpoint == ""
		b.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bridge never saw its stream close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(t, &out, `"id":2`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	waitFor(t, &out, `"id":3`)
	stop()
	msgs := out.messages(t)
	e, _ := byID(t, msgs, 2)["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "answered 503") {
		t.Fatalf("the call that met a refused stream should end naming the refusal: %v", msgs)
	}
	if m := byID(t, msgs, 3); m["error"] != nil {
		t.Fatalf("the bridge did not recover once the stream opened again: %v", m)
	}
}

func TestAStreamNoSessionStartedOnIsClosedAgain(t *testing.T) {
	remote := &droppable{streams: map[string]chan string{}, drop: make(chan struct{}), refuseInitialized: true}
	srv := tlsServer(t, remote)
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	send, stop := serveLines(t, b)
	send(initLine)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	waitFor(t, &out, `"id":1`)
	remote.mu.Lock()
	close(remote.drop)
	remote.drop = make(chan struct{})
	remote.mu.Unlock()
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.endpoint == ""
	})
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(t, &out, `"id":2`)
	e, _ := byID(t, out.messages(t), 2)["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "answered 400") {
		t.Fatalf("a refused notifications/initialized should end the call naming the refusal: %v", e)
	}
	eventually(t, func() bool {
		remote.mu.Lock()
		defer remote.mu.Unlock()
		return remote.active == 0 && remote.opened == 2
	})
	stop()
}

func TestARenewalTheServerNeverAnswersEndsTheCall(t *testing.T) {
	remote := &forgetful{}
	hang := make(chan struct{})
	defer close(hang)
	var mu sync.Mutex
	inits := 0
	srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"initialize"`) {
			mu.Lock()
			inits++
			n := inits
			mu.Unlock()
			if n > 1 {
				// An event stream that never carries the answer.
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-hang:
				case <-r.Context().Done():
				}
				return
			}
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		remote.ServeHTTP(w, r)
	}))
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	b.handshake = 200 * time.Millisecond
	send, stop := serveLines(t, b)
	send(initLine)
	waitFor(t, &out, `"id":1`)
	remote.forget()
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}`)
	waitFor(t, &out, `"id":2`)
	stop()
	e, _ := byID(t, out.messages(t), 2)["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.HasPrefix(msg, "the remote MCP server 127.0.0.1 ") {
		t.Fatalf("a renewal that was never answered should end the call naming the server: %v", e)
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tlsServer is a test server whose cleanup cuts the connections first, so
// a stream the bridge failed to close fails the test instead of hanging
// the server's Close.
func tlsServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}
