package adapter

import (
	"encoding/json"
	"testing"
)

// An input tool answers with an embedded text resource; its text stays out
// of the live event, and the event says it was left out rather than read
// as a tool that returned nothing.
func TestToolResultRedacted(t *testing.T) {
	const resource = `{"type":"resource","resource":{"uri":"agentry://inputs","mimeType":"text/plain","text":"module example.com/m\n"}}`
	const text = `{"type":"text","text":"notes.txt\t25"}`

	codex, _ := Get(CodexAdapterName)
	claude, _ := Get(ClaudeAdapterName)
	codexResult := func(content string) Event {
		evs := codex.Events([]byte(`{"type":"item.completed","item":{"id":"item_5","type":"mcp_tool_call","server":"gateway","tool":"read_input","status":"completed","result":{"content":[` + content + `],"structuredContent":null}}}`))
		if len(evs) != 1 {
			t.Fatalf("codex events = %+v", evs)
		}
		return evs[0]
	}
	claudeResult := func(content string) Event {
		evs := claude.Events([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[` + content + `]}]}}`))
		if len(evs) != 1 {
			t.Fatalf("claude events = %+v", evs)
		}
		return evs[0]
	}
	acpResult := func(content string) Event {
		var items []json.RawMessage
		if content != "" {
			items = append(items, json.RawMessage(`{"type":"content","content":`+content+`}`))
		}
		raw, _ := json.Marshal(items)
		text, redacted := acpToolContent(raw)
		return Event{Text: text, Redacted: redacted}
	}

	for name, result := range map[string]func(string) Event{"codex": codexResult, "claude": claudeResult, "acp": acpResult} {
		if ev := result(resource); ev.Text != "" || !ev.Redacted {
			t.Errorf("%s: an embedded resource = %+v, want no text and redacted", name, ev)
		}
		if ev := result(text); ev.Text != "notes.txt\t25" || ev.Redacted {
			t.Errorf("%s: a text result = %+v, want its text and not redacted", name, ev)
		}
		if ev := result(""); ev.Text != "" || ev.Redacted {
			t.Errorf("%s: an empty result = %+v, want neither text nor redacted", name, ev)
		}
	}
	if ev := codexResult(text + "," + resource); ev.Text != "notes.txt\t25" || !ev.Redacted {
		t.Errorf("codex: text and a resource = %+v, want the text kept and redacted", ev)
	}
	if evs := claude.Events([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"plain"}]}}`)); len(evs) != 1 || evs[0].Text != "plain" || evs[0].Redacted {
		t.Errorf("claude: a string result = %+v", evs)
	}
}
