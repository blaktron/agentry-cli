package convo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
)

func call(id, name string) llm.Part {
	return llm.Part{Kind: llm.Call, CallID: id, Name: name, Arguments: `{}`}
}

func result(id, text string) *llm.Part {
	return &llm.Part{Kind: llm.Result, CallID: id, Text: text}
}

// shape writes a message list as one line a message, for comparison.
func shape(msgs []llm.Message) string {
	var lines []string
	for _, m := range msgs {
		var parts []string
		for _, p := range m.Parts {
			s := string(p.Kind)
			switch p.Kind {
			case llm.Call:
				s += ":" + p.CallID
			case llm.Result:
				s += ":" + p.CallID + "=" + p.Text
				if p.Failed {
					s += "!"
				}
			default:
				s += ":" + p.Text
			}
			parts = append(parts, s)
		}
		lines = append(lines, string(m.Role)+" "+strings.Join(parts, " | "))
	}
	return strings.Join(lines, "\n")
}

func TestProject(t *testing.T) {
	entries := []Entry{
		{Kind: KindHeader, Version: Version, Conversation: "c"},
		{Kind: KindUser, Text: "task"},
		{Kind: KindAssistant, Parts: []llm.Part{{Kind: llm.Text, Text: "reading"}, call("a", "fs_read"), call("b", "fs_list")}},
		{Kind: KindUsage, Usage: &llm.Usage{Input: 1}},
		{Kind: KindCall, CallID: "a"},
		// Results are recorded as they arrive; the model is answered in
		// the order it asked.
		{Kind: KindCall, CallID: "b"},
		{Kind: KindResult, CallID: "b", Result: result("b", "two files")},
		{Kind: KindResult, CallID: "a", Result: result("a", "hello")},
		{Kind: KindAssistant, Parts: []llm.Part{call("c", "run_command"), call("d", "fs_read")}},
		{Kind: KindCall, CallID: "c"},
		// The process died here: c was started, d never was.
		{Kind: KindUser, Text: "carry on"},
		{Kind: KindAssistant, Parts: []llm.Part{{Kind: llm.Text, Text: "done"}}},
	}
	want := strings.Join([]string{
		"user text:task",
		"assistant text:reading | call:a | call:b",
		"user result:a=hello | result:b=two files",
		"assistant call:c | call:d",
		"user result:c=" + Interrupted + "! | result:d=" + NotRun + "! | text:carry on",
		"assistant text:done",
	}, "\n")
	if got := shape(Project(entries)); got != want {
		t.Errorf("messages:\n%s\nwant:\n%s", got, want)
	}

	// Calls still open at the end of the record are answered too, so the
	// conversation is always one a server accepts.
	open := entries[:9]
	if got := shape(Project(open)); !strings.HasSuffix(got, "user result:c="+NotRun+"! | result:d="+NotRun+"!") {
		t.Errorf("open calls at the end:\n%s", got)
	}
}

func TestProjectNamesAResultByItsCall(t *testing.T) {
	msgs := Project([]Entry{
		{Kind: KindUser, Text: "task"},
		{Kind: KindAssistant, Parts: []llm.Part{call("a", "fs_read")}},
		{Kind: KindResult, CallID: "a", Result: &llm.Part{Text: "hello"}},
		// A second result for a call never replaces the first.
		{Kind: KindResult, CallID: "a", Result: &llm.Part{Text: "other"}},
	})
	r := msgs[2].Parts[0]
	if r.Kind != llm.Result || r.CallID != "a" || r.Name != "fs_read" || r.Text != "hello" {
		t.Errorf("result part: %+v", r)
	}
}

func TestALogIsCarriedOnAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	l, existed, err := Open(dir, "run-1")
	if err != nil || existed {
		t.Fatalf("open: existed %v, err %v", existed, err)
	}
	for _, e := range []Entry{
		{Kind: KindUser, Text: "task"},
		{Kind: KindAssistant, Parts: []llm.Part{call("a", "fs_read")}},
		{Kind: KindCall, CallID: "a"},
	} {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	before := shape(l.Messages())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// A process that died while writing leaves half a line.
	path := filepath.Join(dir, "run-1.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"result","callId":"a","result":{"kind":"res`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	l, existed, err = Open(dir, "run-1")
	if err != nil || !existed {
		t.Fatalf("reopen: existed %v, err %v", existed, err)
	}
	defer func() { _ = l.Close() }()
	if got := shape(l.Messages()); got != before {
		t.Errorf("after reopening:\n%s\nwant:\n%s", got, before)
	}
	if err := l.Append(Entry{Kind: KindUser, Text: "carry on"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5 (the half line gone, the new entry after):\n%s", len(lines), raw)
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %d is not an entry: %q", i+1, line)
		}
		if e.At == "" {
			t.Errorf("line %d has no time", i+1)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode is %v, want 0600", info.Mode().Perm())
	}
}

func TestOpenRefuses(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", ".", "..", "../x", "a/b", "-x", strings.Repeat("a", 129), "a b"} {
		if _, _, err := Open(dir, id); err == nil {
			t.Errorf("conversation id %q was accepted", id)
		}
	}

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("newer", `{"kind":"header","version":2,"conversation":"newer"}`+"\n")
	write("other", `{"kind":"header","version":1,"conversation":"someone-else"}`+"\n")
	write("headless", `{"kind":"user","text":"task"}`+"\n")
	write("broken", `{"kind":"header","version":1,"conversation":"broken"}`+"\nnot json\n"+`{"kind":"user","text":"x"}`+"\n")
	for _, id := range []string{"newer", "other", "headless", "broken"} {
		if _, _, err := Open(dir, id); err == nil {
			t.Errorf("the file %s was accepted", id)
		}
	}
}
