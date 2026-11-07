package desktoplink

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir is a temporary folder whose sockets fit a socket's name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var blenderTools = []Tool{
	{Name: "get_scene_info", Description: "the scene", InputSchema: json.RawMessage(`{"type":"object"}`)},
	{Name: "execute_blender_code", Description: "run code", InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}}}`)},
}

// startRelay runs the relay as the CLI holds it: the binding on its stdin
// first, then the answers; its stdout read by the test. It answers each
// call it is sent with the call's tool name, as Agentry Desktop would.
func startRelay(t *testing.T, bind Bind) (sock string, sent *syncBuffer, logged *syncBuffer) {
	t.Helper()
	dir := shortDir(t)
	sock = filepath.Join(dir, "link.sock")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	sent, logged = &syncBuffer{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Relay(ctx, RelayConfig{Listen: sock, In: inR, Out: outW, Log: logged})
		_ = outW.Close()
	}()
	raw, _ := json.Marshal(Frame{Bind: &bind})
	if _, err := inW.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	if !sc.Scan() {
		t.Fatalf("the relay ended before its ready line: %v, %s", <-done, logged.String())
	}
	if f, err := ReadFrame(sc.Bytes()); err != nil || !f.Ready {
		t.Fatalf("the relay's first line is %s", sc.Bytes())
	}
	go func() {
		for sc.Scan() {
			_, _ = sent.Write(append(append([]byte(nil), sc.Bytes()...), '\n'))
			f, err := ReadFrame(sc.Bytes())
			if err != nil || !f.IsCall() {
				continue
			}
			a := TextAnswer("answered "+f.Server+"/"+f.Request.Tool, false)
			ans, _ := json.Marshal(Frame{ID: f.ID, Answer: &a})
			_, _ = inW.Write(append(ans, '\n'))
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the relay ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the relay did not stop")
		}
	})
	return sock, sent, logged
}

// The relay writes the run's binding beside its socket for the desktop
// link entries, carries a call of a bound server with the run token to the
// CLI as a frame naming the server, and writes the answer back to the
// connection that asked.
func TestTheRelayCarriesABoundServersCall(t *testing.T) {
	bind := Bind{Token: "tok-1", Servers: map[string][]Tool{"blender": blenderTools}}
	sock, sent, _ := startRelay(t, bind)
	got, err := ReadBind(filepath.Join(filepath.Dir(sock), filepath.Base(BindFile)))
	if err != nil || got.Token != "tok-1" || len(got.Servers["blender"]) != 2 {
		t.Fatalf("the binding written: %+v, %v", got, err)
	}
	a, err := Send(context.Background(), sock, Request{Token: "tok-1", Server: "blender", Tool: "get_scene_info", Arguments: json.RawMessage(`{}`)})
	if err != nil || a.IsError || a.Text() != "answered blender/get_scene_info" {
		t.Fatalf("answer %+v, %v", a, err)
	}
	if !strings.Contains(sent.String(), `"server":"blender","token":"tok-1","request":{"tool":"get_scene_info","arguments":{}}`) {
		t.Fatalf("the CLI was sent %q", sent.String())
	}
}

// Nothing but a tool call of a bound server carrying the run token reaches
// the CLI: a wrong token, an unbound server, a tool the server does not
// have, a line that is not a request and a frame the CLI side would drop
// are each refused at the socket, and the CLI sees no frame for them.
func TestTheRelayRefusesWhatIsNotABoundCall(t *testing.T) {
	bind := Bind{Token: "tok-1", Servers: map[string][]Tool{"blender": blenderTools}}
	sock, sent, _ := startRelay(t, bind)
	for name, line := range map[string]string{
		"a wrong token":        `{"token":"tok-2","server":"blender","tool":"get_scene_info"}`,
		"no token":             `{"server":"blender","tool":"get_scene_info"}`,
		"an unbound server":    `{"token":"tok-1","server":"unreal","tool":"call_tool"}`,
		"a tool it lacks":      `{"token":"tok-1","server":"blender","tool":"disable_telemetry"}`,
		"no tool":              `{"token":"tok-1","server":"blender","tool":""}`,
		"a frame, not a call":  `{"id":1,"answer":{"content":[]}}`,
		"a supervisor command": `{"cmd":"stop"}`,
		"not json":             `stop`,
	} {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte(line + "\n"))
		sc := bufio.NewScanner(c)
		if !sc.Scan() {
			t.Errorf("%s: no answer", name)
		} else if !strings.Contains(sc.Text(), `"isError":true`) || !strings.Contains(sc.Text(), "this was not one") {
			t.Errorf("%s: answered %q", name, sc.Text())
		}
		_ = c.Close()
	}
	if got := sent.String(); got != "" {
		t.Fatalf("the CLI was sent %q", got)
	}
}

// The relay starts only on a binding: a first line that is a call, an
// empty binding or a binding without a token ends it before it listens.
func TestTheRelayStartsOnABindingAlone(t *testing.T) {
	for name, first := range map[string]string{
		"a call":               `{"id":1,"server":"blender","token":"t","request":{"tool":"x"}}`,
		"a ready line":         `{"ready":true}`,
		"no token":             `{"bind":{"token":"","servers":{"blender":[]}}}`,
		"no server":            `{"bind":{"token":"t","servers":{}}}`,
		"a binding with an id": `{"id":3,"bind":{"token":"t","servers":{"blender":[]}}}`,
	} {
		sock := filepath.Join(shortDir(t), "link.sock")
		err := Relay(context.Background(), RelayConfig{Listen: sock, In: strings.NewReader(first + "\n"), Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "the binding") {
			t.Errorf("%s: %v", name, err)
		}
		if _, statErr := os.Stat(sock); statErr == nil {
			t.Errorf("%s: the relay listened", name)
		}
	}
}

// The desktop link entry lists the bound server's tools to the gateway with
// the schemas Agentry Desktop gave, forwards a call with the run token and
// returns the answer's content as the tool's result; a tool it does not
// list is refused before anything crosses, and an unbound server lists none.
func TestTheDesktopLinkServesTheBoundServersTools(t *testing.T) {
	bind := Bind{Token: "tok-1", Servers: map[string][]Tool{"blender": blenderTools}}
	sock, sent, _ := startRelay(t, bind)
	bindFile := filepath.Join(filepath.Dir(sock), filepath.Base(BindFile))
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_scene_info","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"disable_telemetry","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
	}
	var out syncBuffer
	if err := Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, io.Discard, "blender", bindFile, sock); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`"serverInfo":{"name":"agentry-desktop-blender","version":"1"}`,
		`"name":"get_scene_info"`, `"name":"execute_blender_code"`, `"inputSchema":{"type":"object","properties":{"code":{"type":"string"}}}`,
		`{"content":[{"type":"text","text":"answered blender/get_scene_info"}],"isError":false}`,
		`"unknown tool: disable_telemetry"`,
		`"method not found: resources/list"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the server's output lacks %s:\n%s", want, got)
		}
	}
	if n := strings.Count(sent.String(), `"request"`); n != 1 {
		t.Errorf("%d calls crossed, want 1: %q", n, sent.String())
	}
	if !strings.Contains(sent.String(), `"token":"tok-1"`) {
		t.Errorf("the call carried no token: %q", sent.String())
	}

	var none syncBuffer
	if err := Serve(context.Background(), strings.NewReader(lines[1]+"\n"), &none, io.Discard, "unreal", bindFile, sock); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), `"tools":[]`) {
		t.Errorf("an unbound server lists %s", none.String())
	}
	if err := Serve(context.Background(), strings.NewReader(""), io.Discard, io.Discard, "", bindFile, sock); err == nil {
		t.Error("a server with no id served")
	}
	if err := Serve(context.Background(), strings.NewReader(""), io.Discard, io.Discard, "blender", filepath.Join(shortDir(t), "none.json"), sock); err == nil {
		t.Error("a server with no binding served")
	}
}

// A call the CLI never answers ends when the relay's input ends, as an
// error the model reads rather than a hang; a frame from the CLI that is not
// an answer is dropped.
func TestTheRelayEndsWaitingCallsWhenTheCLILeaves(t *testing.T) {
	sock := filepath.Join(shortDir(t), "link.sock")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var logged syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- Relay(context.Background(), RelayConfig{Listen: sock, In: inR, Out: outW, Log: &logged})
		_ = outW.Close()
	}()
	raw, _ := json.Marshal(Frame{Bind: &Bind{Token: "t", Servers: map[string][]Tool{"blender": blenderTools}}})
	_, _ = inW.Write(append(raw, '\n'))
	sc := bufio.NewScanner(outR)
	if !sc.Scan() {
		t.Fatal("no ready line")
	}
	go func() {
		for sc.Scan() {
		}
	}()
	answered := make(chan Answer, 1)
	go func() {
		a, _ := Send(context.Background(), sock, Request{Token: "t", Server: "blender", Tool: "look"})
		answered <- a
	}()
	// The tool is not one of the server's, so the call is refused at the
	// socket; one that is, waits.
	a := <-answered
	if !a.IsError {
		t.Fatalf("a tool the server lacks was answered %+v", a)
	}
	go func() {
		a, _ := Send(context.Background(), sock, Request{Token: "t", Server: "blender", Tool: "get_scene_info"})
		answered <- a
	}()
	time.Sleep(50 * time.Millisecond)
	_, _ = inW.Write([]byte(`{"id":99,"request":{"tool":"x"}}` + "\n"))
	time.Sleep(50 * time.Millisecond)
	_ = inW.Close()
	select {
	case a := <-answered:
		if !a.IsError || !strings.Contains(a.Text(), "relay has stopped") {
			t.Fatalf("the waiting call was answered %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting call hung")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not stop")
	}
	if !strings.Contains(logged.String(), "not an answer was dropped") {
		t.Errorf("the relay logged %q", logged.String())
	}
}

func TestNamesAndFrames(t *testing.T) {
	if ServerName("blender") != "agentry-desktop-blender" || ServerName(ComputerID) != "agentry-desktop-computer" {
		t.Error(ServerName("blender"))
	}
	if got := Command("blender"); strings.Join(got, " ") != "desktoplink --server blender" {
		t.Error(got)
	}
	call := Frame{ID: 1, Server: "blender", Token: "t", Request: &Call{Tool: "look"}}
	if !call.IsCall() {
		t.Error("a call is not a call")
	}
	for name, f := range map[string]Frame{
		"no id":        {Server: "blender", Token: "t", Request: &Call{Tool: "look"}},
		"no server":    {ID: 1, Token: "t", Request: &Call{Tool: "look"}},
		"no token":     {ID: 1, Server: "blender", Request: &Call{Tool: "look"}},
		"no tool":      {ID: 1, Server: "blender", Token: "t", Request: &Call{}},
		"an answer":    {ID: 1, Server: "blender", Token: "t", Request: &Call{Tool: "look"}, Answer: &Answer{}},
		"a ready mark": {ID: 1, Server: "blender", Token: "t", Request: &Call{Tool: "look"}, Ready: true},
		"a binding":    {ID: 1, Server: "blender", Token: "t", Request: &Call{Tool: "look"}, Bind: &Bind{}},
	} {
		if f.IsCall() {
			t.Errorf("%s is a call", name)
		}
	}
	if _, err := ReadFrame([]byte(`{"id":1,"cmd":"stop"}`)); err == nil {
		t.Error("an unknown field was read")
	}
	if a := TextAnswer("x", true); a.Text() != "x" || !a.IsError {
		t.Error(a)
	}
}
