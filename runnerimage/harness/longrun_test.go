package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/harness/convo"
	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// shaped builds a value of a credential's shape from pieces, so this file
// holds none whole.
func shaped(parts ...string) string { return strings.Join(parts, "") }

func TestScrubTextReplacesEachSecretShapeAndNothingElse(t *testing.T) {
	letters := strings.Repeat("a1B2", 9)
	secrets := map[string]string{
		"a private key":        shaped("-----BEGIN RSA ", "PRIVATE KEY-----\nMIIEow", letters, "\n-----END RSA ", "PRIVATE KEY-----"),
		"an AWS key id":        shaped("AKIA", "ABCDEFGHIJKLMNOP"),
		"a GitHub token":       shaped("gh", "p_", letters),
		"a fine-grained token": shaped("github", "_pat_", letters),
		"a GitLab token":       shaped("gl", "pat-", letters[:24]),
		"a model key":          shaped("sk", "-ant-api03-", letters),
		"a router key":         shaped("sk", "-or-v1-", letters),
		"a live payment key":   shaped("sk", "_live_", letters),
		"a Slack token":        shaped("xo", "xb-1234567890-", letters[:20]),
		"a Google key":         shaped("AI", "za", letters[:35]),
		"a Hugging Face token": shaped("h", "f_", letters),
		"an npm token":         shaped("np", "m_", letters),
		"a signed web token":   shaped("ey", "JhbGciOiJIUzI1NiJ9", ".ey", "JzdWIiOiIxMjM0NTY3ODkwIn0", ".", letters[:20]),
	}
	for name, secret := range secrets {
		in := "before " + secret + " after"
		got := scrubText(in)
		if strings.Contains(got, secret) || got != "before "+scrubbed+" after" {
			t.Errorf("%s: %q", name, got)
		}
	}

	// A credential in a header or an address goes; what names it stays.
	token := letters[:24]
	if got := scrubText("Authorization: Bearer " + token + "\nAccept: */*"); got != "Authorization: Bearer "+scrubbed+"\nAccept: */*" {
		t.Errorf("a header: %q", got)
	}
	if got := scrubText(`{"authorization": "token ` + token + `"}`); strings.Contains(got, token) {
		t.Errorf("a header in JSON: %q", got)
	}
	if got := scrubText("postgres://app:" + "hunter2hunter2" + "@db.internal:5432/app"); got != "postgres://app:"+scrubbed+"@db.internal:5432/app" {
		t.Errorf("an address: %q", got)
	}

	for _, plain := range []string{
		"",
		"The risk-management-and-compliance-policy-2026 task-list is done.",
		"sk-short",
		"https://example.com/a/b?c=d and ssh://git@github.com/owner/repo.git",
		"commit 3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a39 on main",
		"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		"Authorization: Bearer <token>",
		"eyJ is how a base64 JSON object starts",
	} {
		if got := scrubText(plain); got != plain {
			t.Errorf("%q became %q", plain, got)
		}
	}
}

func TestScrubArgumentsKeepsWhatHoldsNoSecretByteForByte(t *testing.T) {
	clean := json.RawMessage(`{ "path": "a.txt",  "n": 12345678901234567890, "f": 1.50 }`)
	if got := scrubArguments(clean); string(got) != string(clean) {
		t.Errorf("clean arguments were rewritten: %s", got)
	}
	secret := shaped("gh", "p_", strings.Repeat("a1B2", 9))
	got := scrubArguments(json.RawMessage(`{"n":12345678901234567890,"headers":[{"k":"x","v":"` + secret + `"}],"note":"<a&b>"}`))
	var v struct {
		N       json.Number
		Headers []struct{ K, V string }
		Note    string
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("not JSON any more: %s", got)
	}
	if v.N.String() != "12345678901234567890" || v.Headers[0].V != scrubbed || v.Headers[0].K != "x" || v.Note != "<a&b>" || strings.Contains(string(got), secret) {
		t.Errorf("scrubbed arguments: %s", got)
	}
	// A member's name is scrubbed as its value is.
	if got := scrubArguments(json.RawMessage(`{"` + secret + `":1}`)); string(got) != `{"`+scrubbed+`":1}` {
		t.Errorf("a secret-shaped member name: %s", got)
	}
	if got := scrubArguments(json.RawMessage(`{"cut`)); string(got) != `{"cut` {
		t.Errorf("arguments that are not JSON: %s", got)
	}
}

func TestCutResultKeepsTheStartAtAWholeLine(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 6000; i++ {
		b.WriteString("line of the result, numbered ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	long := b.String()
	got, cut := cutResult(long, 2048)
	head, notice, found := strings.Cut(got, "\n[Note from the agent runtime: ")
	switch {
	case !cut || !found:
		t.Fatalf("not cut: %d bytes", len(got))
	case len(got) > 2048:
		t.Errorf("%d bytes, over the limit", len(got))
	case !strings.HasPrefix(long, head+"\n"):
		t.Errorf("the start is not the result's own, ending at a line: %q", head[len(head)-40:])
	case len(head) < 2048*5/8:
		t.Errorf("only %d bytes of the result were kept", len(head))
	case !strings.Contains(notice, "was 6018 bytes long") && !strings.Contains(notice, "bytes long and ends here, after its first"):
		t.Errorf("the notice: %s", notice)
	}

	if got, cut := cutResult("short", 2048); cut || got != "short" {
		t.Errorf("a short result: %q", got)
	}
	// One long line is cut at a character, never inside one.
	wide := strings.Repeat("é", 3000)
	got, _ = cutResult(wide, 1024)
	head, _, _ = strings.Cut(got, "\n[Note")
	if len(got) > 1024 || !strings.HasPrefix(wide, head) || len(head)%2 != 0 {
		t.Errorf("a line with no end: %d bytes, head %d", len(got), len(head))
	}
}

func TestLoopWatchSeesTheSameCallAndTheSameSequence(t *testing.T) {
	cfg := &proto.Loop{Repeats: 3, CycleRepeats: 2, MaxPeriod: 3}
	feed := func(calls ...string) (round []string, times int) {
		var w loopWatch
		for _, c := range calls {
			name, args, _ := strings.Cut(c, " ")
			w.add(name, args)
			round, times = w.repeating(cfg)
		}
		return round, times
	}
	if round, times := feed(`read {"p":1}`, `read { "p": 1 }`, `read {"p":1}`); times != 3 || strings.Join(round, ",") != "read" {
		t.Errorf("the same call three times: %v %d", round, times)
	}
	if _, times := feed(`read {"p":1}`, `read {"p":2}`, `read {"p":3}`); times != 0 {
		t.Error("three different calls were a streak")
	}
	if _, times := feed(`read {"p":1}`, `read {"p":1}`); times != 0 {
		t.Error("two calls were a streak of three")
	}
	if round, times := feed(`read {"p":1}`, `write {"p":1}`, `read {"p":1}`, `write {"p":1}`); times != 2 || strings.Join(round, ",") != "read,write" {
		t.Errorf("a sequence of two, twice: %v %d", round, times)
	}
	if round, times := feed(`a {}`, `b {}`, `c {}`, `a {}`, `b {}`, `c {}`); times != 2 || len(round) != 3 {
		t.Errorf("a sequence of three, twice: %v %d", round, times)
	}
	if _, times := feed(`a {}`, `b {}`, `a {}`, `c {}`); times != 0 {
		t.Error("a sequence that changed was a streak")
	}
	if _, times := feed(`a {}`, `b {}`, `c {}`, `d {}`, `a {}`, `b {}`, `c {}`, `d {}`); times != 0 {
		t.Error("a sequence longer than maxPeriod was looked for")
	}
	if _, times := (&loopWatch{}).repeating(nil); times != 0 {
		t.Error("no configuration looked for a streak")
	}
}

func TestTheNudgeNamesToolsAndCountsAndNeverArguments(t *testing.T) {
	one := nudgeText([]string{"fs_read"}, 5, 0)
	if !strings.HasPrefix(one, "Note from the agent runtime, not the user: you have made the same call to fs_read, with the same arguments, 5 times in a row.") {
		t.Errorf("one call: %s", one)
	}
	if strings.Contains(one, "told this") {
		t.Errorf("a first nudge speaks of an earlier one: %s", one)
	}
	if got := nudgeText([]string{"a", "b", "c"}, 3, 1); !strings.Contains(got, "the same 3 calls (a, b and c), with the same arguments, 3 times in a row") || !strings.HasSuffix(got, "You were told this once before in this conversation.") {
		t.Errorf("a sequence: %s", got)
	}
	if got := nudgeText([]string{"a"}, 5, 4); !strings.HasSuffix(got, "You were told this 4 times before in this conversation.") {
		t.Errorf("a fifth nudge: %s", got)
	}
}

func call(id, name string) llm.Part {
	return llm.Part{Kind: llm.Call, CallID: id, Name: name, Arguments: `{}`}
}

func result(id, text string) convo.Entry {
	return convo.Entry{Kind: convo.KindResult, CallID: id, Result: &llm.Part{Kind: llm.Result, CallID: id, Name: "fs_read", Text: text}}
}

func TestACompactionKeepsWhatTheModelHasNotAnswered(t *testing.T) {
	header := convo.Entry{Kind: convo.KindHeader}
	prompt := convo.Entry{Kind: convo.KindUser, Text: "The task."}
	usage := convo.Entry{Kind: convo.KindUsage, Usage: &llm.Usage{Input: 10}}
	asks := convo.Entry{Kind: convo.KindAssistant, Parts: []llm.Part{call("c1", "fs_read")}, Finish: "tool_calls"}
	answers := convo.Entry{Kind: convo.KindAssistant, Parts: []llm.Part{{Kind: llm.Text, Text: "Done."}}, Finish: "stop"}
	started := convo.Entry{Kind: convo.KindCall, CallID: "c1"}
	small, large := result("c1", "ten bytes."), result("c1", strings.Repeat("x", 5000))
	nudge := convo.Entry{Kind: convo.KindUser, Note: convo.NoteNudge, Text: "Note from the agent runtime…"}
	stub := convo.Entry{Kind: convo.KindStub, Text: "[cut]"}

	cases := []struct {
		name    string
		entries []convo.Entry
		want    int
	}{
		{"a reply's calls and their results, mid-turn", []convo.Entry{header, prompt, asks, usage, started, small}, 2},
		{"not when they are larger than the budget", []convo.Entry{header, prompt, asks, usage, started, large}, 0},
		{"the note after the results too", []convo.Entry{header, prompt, asks, usage, started, small, nudge}, 2},
		{"the note alone when the results are too large", []convo.Entry{header, prompt, asks, usage, started, large, nudge}, 6},
		{"a new prompt, and nothing of the turn that was answered", []convo.Entry{header, prompt, answers, usage, prompt}, 4},
		{"nothing when the model has answered everything", []convo.Entry{header, prompt, answers, usage}, 0},
		{"the message after a cut-off reply, and not the reply", []convo.Entry{header, prompt, asks, usage, stub, nudge}, 5},
	}
	for _, c := range cases {
		if got := keepFrom(c.entries, 1024); got != c.want {
			t.Errorf("%s: keeps from %d, want %d", c.name, got, c.want)
		}
	}
}

func TestASummaryRequestIsMadeSmallerLevelByLevel(t *testing.T) {
	big := strings.Repeat("0123456789", 1000)
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.User, Parts: []llm.Part{{Kind: llm.Text, Text: summaryOpen + "An earlier summary."}, {Kind: llm.Text, Text: "Go on."}}})
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			llm.Message{Role: llm.Assistant, Parts: []llm.Part{call("c", "fs_read")}},
			llm.Message{Role: llm.User, Parts: []llm.Part{{Kind: llm.Result, CallID: "c", Text: big, Images: []llm.Image{{MediaType: "image/png", Data: "AAAA"}}}}})
	}
	if got := summaryInput(msgs, 0); len(got) != len(msgs) || got[2].Parts[0].Text != big {
		t.Error("level 0 changed the conversation")
	}
	one := summaryInput(msgs, 1)
	if r := one[2].Parts[0]; len(one) != len(msgs) || len(r.Text) > summaryResultBytes+80 || !strings.HasPrefix(big, r.Text[:summaryResultBytes]) || len(r.Images) != 0 {
		t.Errorf("level 1: %d messages, a result of %d bytes with %d images", len(one), len(r.Text), len(r.Images))
	}
	if msgs[2].Parts[0].Text != big || len(msgs[2].Parts[0].Images) != 1 {
		t.Error("the conversation itself was changed")
	}
	for level, most := range map[int]int{2: 9, 3: 5} {
		got := summaryInput(msgs, level)
		if len(got) > most || got[0].Role != llm.User || got[1].Role != llm.Assistant {
			t.Errorf("level %d: %d messages, opening %s then %s", level, len(got), got[0].Role, got[1].Role)
			continue
		}
		// The summary the conversation opened with stays, and the request
		// says the rest was left out.
		if p := got[0].Parts; len(p) != 2 || !strings.HasPrefix(p[0].Text, summaryOpen) || p[1].Text != omitted {
			t.Errorf("level %d opens with %+v", level, p)
		}
	}
}

func TestWhatStandsForACutOffReplyNeverRepeatsIt(t *testing.T) {
	reply := &llm.Response{Finish: llm.FinishLength, Parts: []llm.Part{
		{Kind: llm.Reasoning, Text: "secret-marker reasoning"},
		{Kind: llm.Text, Text: "secret-marker text"},
	}}
	kind, calls, chars := cutKind(reply)
	if kind != proto.CutAnswer || len(calls) != 0 || chars != len("secret-marker text") {
		t.Errorf("an answer: %s %v %d", kind, calls, chars)
	}
	reply.Parts = append(reply.Parts, llm.Part{Kind: llm.Call, Name: "fs_write", Arguments: `{"path":"secret-marker`})
	if kind, calls, _ = cutKind(reply); kind != proto.CutToolCall || strings.Join(calls, ",") != "fs_write" {
		t.Errorf("a call: %s %v", kind, calls)
	}
	if kind, _, _ := cutKind(&llm.Response{Parts: reply.Parts[:1]}); kind != proto.CutReasoning {
		t.Errorf("reasoning alone: %s", kind)
	}
	for _, kind := range []string{proto.CutAnswer, proto.CutToolCall, proto.CutReasoning} {
		for _, last := range []bool{false, true} {
			stub, tell := cutStub(kind, []string{"fs_write"}, 18), cutInstruction(kind, []string{"fs_write"}, 18, last)
			if strings.Contains(stub+tell, "secret-marker") || !strings.HasPrefix(tell, runtimeNote) || !strings.HasPrefix(stub, "[") {
				t.Errorf("%s: %s / %s", kind, stub, tell)
			}
			if strings.HasSuffix(tell, "will end the run.") != last {
				t.Errorf("%s, last %v: %s", kind, last, tell)
			}
		}
	}
}
