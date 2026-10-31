package applink

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"agent_stop","arguments":{"agent":"assistant"}}}`,
	)
	if len(replies) != 4 {
		t.Fatalf("%d replies", len(replies))
	}
	tools := replies[1]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, Tools(SetAssistant)) {
		t.Fatalf("the Assistant's set: %v", names)
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
// resets an agent; the Assistant's set is self_configure, its task tools and
// recall, each taking its own fields and nothing else.
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
	own := []string{SelfConfigure, TaskAdd, TaskList, TaskUpdate, TaskRemove, Recall}
	meetings := []string{MeetingArm, MeetingList, MeetingCancel}
	if got := Tools(SetAssistant); !slices.Equal(got, slices.Concat(own, Tools(SetMain), meetings)) {
		t.Errorf("the Assistant's set is %v", got)
	}
	// Each of the Assistant's own tools takes its own name and personality,
	// or a task's fields, and nothing else.
	takes := map[string][]string{
		SelfConfigure: {"name", "personality"},
		TaskAdd:       {"cron", "instruction", "name", "zone"},
		TaskList:      nil,
		TaskUpdate:    {"cron", "instruction", "name", "paused", "task", "zone"},
		TaskRemove:    {"task"},
		Recall:        {"from", "limit", "query", "to"},
	}
	for _, tool := range assistantTools {
		props := tool.Schema["properties"].(map[string]any)
		got := slices.Sorted(maps.Keys(props))
		if !slices.Equal(got, takes[tool.Name]) || tool.Schema["additionalProperties"] != false {
			t.Errorf("%s takes %v", tool.Name, got)
		}
	}
}

// TestTheAssistantHasEveryHelperTool: the Assistant's set holds each of
// Agentry Helper's tools with the same description and schema, so it can do
// whatever the Helper can, and no tool is listed twice.
func TestTheAssistantHasEveryHelperTool(t *testing.T) {
	byName := map[string]Tool{}
	for _, tool := range sets[SetAssistant] {
		if _, dup := byName[tool.Name]; dup {
			t.Errorf("the Assistant's set lists %s twice", tool.Name)
		}
		byName[tool.Name] = tool
	}
	if len(sets[SetMain]) == 0 {
		t.Fatal("Agentry Helper's set is empty")
	}
	for _, want := range sets[SetMain] {
		got, ok := byName[want.Name]
		if !ok {
			t.Errorf("the Assistant's set lacks %s", want.Name)
			continue
		}
		if got.Description != want.Description || !reflect.DeepEqual(got.Schema, want.Schema) {
			t.Errorf("the Assistant's %s differs from Agentry Helper's", want.Name)
		}
	}
}

// TestAnUnknownSetIsRefused: the role refuses a set it does not know.
func TestAnUnknownSetIsRefused(t *testing.T) {
	if err := Serve(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, "coder", "/nowhere"); err == nil {
		t.Fatal("an unknown set was served")
	}
}

// TestARunAndAScheduleTakeAnswersByQuestionID: quickagent_run and
// schedule_add take a QuickAgent's answers as an object of strings keyed by
// question id, and quickagent_show says it shows how to run the QuickAgent,
// so an agent can read the questions and answer them.
func TestARunAndAScheduleTakeAnswersByQuestionID(t *testing.T) {
	byName := map[string]Tool{}
	for _, tool := range sets[SetMain] {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"quickagent_run", "schedule_add"} {
		props, _ := byName[name].Schema["properties"].(map[string]any)
		a, ok := props["answers"].(map[string]any)
		if !ok {
			t.Fatalf("%s takes no answers: %v", name, byName[name].Schema)
		}
		if a["type"] != "object" || !reflect.DeepEqual(a["additionalProperties"], map[string]any{"type": "string"}) {
			t.Errorf("%s's answers are not an object of strings: %v", name, a)
		}
		if req, _ := byName[name].Schema["required"].([]string); slices.Contains(req, "answers") {
			t.Errorf("%s requires answers, which a QuickAgent without questions has none of", name)
		}
	}
	show := byName["quickagent_show"].Description
	for _, want := range []string{"how to run it", "the questions it asks", "the files it takes", "the keys it needs", "guide"} {
		if !strings.Contains(show, want) {
			t.Errorf("quickagent_show's description lacks %q: %s", want, show)
		}
	}
	if run := byName["quickagent_run"].Description; !strings.Contains(run, "answers to the questions quickagent_show lists") {
		t.Errorf("quickagent_run's description does not say where its answers come from: %s", run)
	}
}
