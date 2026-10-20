package applink

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSupervisor answers each request on a socket with the tool's name and
// its arguments, and records what it was sent.
func fakeSupervisor(t *testing.T) (string, *[]Request) {
	t.Helper()
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "app.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var got []Request
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := ReadRequest(c)
			if err == nil {
				got = append(got, req)
				b, _ := json.Marshal(Answer{Text: "answered " + req.Tool + " " + string(req.Arguments)})
				_, _ = c.Write(append(b, '\n'))
			}
			_ = c.Close()
		}
	}()
	return sock, &got
}

func serve(t *testing.T, set, sock string, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &bytes.Buffer{}, set, sock); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%v in %q", err, l)
		}
		replies = append(replies, r)
	}
	return replies
}

// TestTheAppLinkListsItsSetAndForwardsEachCall: tools/list is the set's
// tools alone, a call to one is forwarded over the socket with its
// arguments and its answer returned, and a tool outside the set is refused
// without reaching the socket.
func TestTheAppLinkListsItsSetAndForwardsEachCall(t *testing.T) {
	sock, got := fakeSupervisor(t)
	replies := serve(t, SetAssistant, sock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"self_configure","arguments":{"name":"Sandra"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"quickagent_run","arguments":{"ref":"x/y"}}}`,
	)
	if len(replies) != 4 {
		t.Fatalf("%d replies", len(replies))
	}
	tools := replies[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != SelfConfigure {
		t.Fatalf("the Assistant's set: %v", tools)
	}
	text := replies[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != `answered self_configure {"name":"Sandra"}` {
		t.Errorf("the forwarded answer: %v", text)
	}
	if replies[3]["error"] == nil {
		t.Error("a tool outside the set was called")
	}
	if len(*got) != 1 || (*got)[0].Tool != SelfConfigure {
		t.Errorf("the socket was sent %+v", *got)
	}
}

// TestAppLinkHasNoGrantTool: no tool of any set changes what an agent is
// given, signs in, publishes, touches a credential, or starts, stops or
// resets an agent; the Assistant's set is self_configure alone.
func TestAppLinkHasNoGrantTool(t *testing.T) {
	// Each word of a tool's name is held to the words of what no tool may do.
	forbidden := map[string]bool{"grant": true, "roster": true, "switch": true, "change": true, "configure": true, "config": true,
		"enable": true, "disable": true, "reset": true, "stop": true, "start": true, "agent": true, "agents": true, "connect": true,
		"login": true, "signin": true, "publish": true, "secret": true, "key": true, "token": true, "credential": true, "policy": true,
		"unsafe": true, "set": true, "write": true, "delete": true}
	for _, set := range Sets() {
		for _, name := range Tools(set) {
			if name == SelfConfigure && set == SetAssistant {
				continue
			}
			for _, w := range strings.Split(name, "_") {
				if forbidden[w] {
					t.Errorf("the %s set's tool %s reads as one that may %s", set, name, w)
				}
			}
		}
	}
	if got := Tools(SetAssistant); len(got) != 1 || got[0] != SelfConfigure {
		t.Errorf("the Assistant's set is %v", got)
	}
	for _, tool := range sets[SetAssistant] {
		props := tool.Schema["properties"].(map[string]any)
		if len(props) != 2 || props["name"] == nil || props["personality"] == nil || tool.Schema["additionalProperties"] != false {
			t.Errorf("self_configure takes %v", props)
		}
	}
}

// TestAnUnknownSetIsRefused: the role refuses a set it does not know.
func TestAnUnknownSetIsRefused(t *testing.T) {
	if err := Serve(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, "coder", "/nowhere"); err == nil {
		t.Fatal("an unknown set was served")
	}
}
