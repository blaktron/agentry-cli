package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway plays the MCP gateway's streamable HTTP: it assigns a
// session at initialize and refuses any later message without it or
// without the token, accepts the initialized notification with a 202, and
// serves tools/list in pages, as SSE when sse is set.
type fakeGateway struct {
	pages [][]string
	sse   bool
	// failList answers tools/list with a JSON-RPC error.
	failList bool

	mu      sync.Mutex
	methods []string
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	_ = json.Unmarshal(b, &m)
	g.mu.Lock()
	g.methods = append(g.methods, m.Method)
	g.mu.Unlock()
	if m.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "s-1" {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	var res any
	switch m.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s-1")
		res = map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": "2025-06-18"}}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		if g.failList {
			res = map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": "catalog unavailable"}}
			break
		}
		page := 0
		if m.Params.Cursor != "" {
			_, _ = fmt.Sscanf(m.Params.Cursor, "p%d", &page)
		}
		tools := []map[string]any{}
		for _, n := range g.pages[page] {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		result := map[string]any{"tools": tools}
		if page+1 < len(g.pages) {
			result["nextCursor"] = fmt.Sprintf("p%d", page+1)
		}
		res = map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}
	default:
		http.Error(w, "unknown", http.StatusBadRequest)
		return
	}
	out, _ := json.Marshal(res)
	if g.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// TestListToolsWalksTheCatalog: the handshake carries the gateway's
// session, every page is read, SSE and plain JSON answers both decode, and
// a name served twice is listed once.
func TestListToolsWalksTheCatalog(t *testing.T) {
	for _, sse := range []bool{false, true} {
		g := &fakeGateway{sse: sse, pages: [][]string{
			{"fs_read", "write_report", "voice_read"},
			{"create_entities", "read_graph", "write_report"},
		}}
		srv := httptest.NewServer(g)
		got, err := ListTools(srv.Client(), srv.URL, "tok", time.Now().Add(time.Minute))
		srv.Close()
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		want := []string{"fs_read", "write_report", "voice_read", "create_entities", "read_graph"}
		if !slices.Equal(got, want) {
			t.Errorf("sse=%v: got %v, want %v", sse, got, want)
		}
		if want := []string{"initialize", "notifications/initialized", "tools/list", "tools/list"}; !slices.Equal(g.methods, want) {
			t.Errorf("sse=%v: methods %v, want %v", sse, g.methods, want)
		}
	}
}

// TestListToolsFailsClosed: a refused token, a gateway error (passed on
// by its code alone: its message is a third party's words), an endless
// cursor and a missed deadline are errors, never an empty or partial list a session
// would then permit.
func TestListToolsFailsClosed(t *testing.T) {
	g := &fakeGateway{pages: [][]string{{"fs_read"}}}
	srv := httptest.NewServer(g)
	defer srv.Close()
	if _, err := ListTools(srv.Client(), srv.URL, "wrong", time.Now().Add(time.Minute)); err == nil {
		t.Error("a refused token listed tools")
	}
	g.failList = true
	if _, err := ListTools(srv.Client(), srv.URL, "tok", time.Now().Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "error -32603") || strings.Contains(err.Error(), "catalog unavailable") {
		t.Errorf("a tools/list error: %v", err)
	}
	g.failList = false
	endless := make([][]string, maxListPages+1)
	for i := range endless {
		endless[i] = []string{fmt.Sprintf("t%d", i)}
	}
	g.pages = endless
	if _, err := ListTools(srv.Client(), srv.URL, "tok", time.Now().Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "still paging") {
		t.Errorf("an endless cursor: %v", err)
	}
	g.pages = [][]string{{"fs_read"}}
	if _, err := ListTools(srv.Client(), srv.URL, "tok", time.Now().Add(-time.Second)); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Errorf("a listing past its deadline: %v", err)
	}
}

// TestSSEMessageJoinsDataLines: an SSE event's data lines make one
// message, as the SSE format says.
func TestSSEMessageJoinsDataLines(t *testing.T) {
	got := string(sseMessage([]byte("event: message\ndata: {\"a\":\ndata: 1}\n\nevent: x\ndata: no\n\n")))
	if got != "{\"a\":\n1}" {
		t.Errorf("got %q", got)
	}
}
