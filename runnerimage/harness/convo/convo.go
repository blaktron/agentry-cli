// Package convo is a conversation's record: an append-only file of one JSON
// entry a line, and the messages a model request is built from, which are a
// pure function of that file.
//
// Nothing is ever rewritten. A tool call is recorded before it runs and its
// result after, so a process that died in between leaves a call with no
// result; such a call is shown to the model as failed and is never run
// again.
//
// What the model is sent can still change: a cut-off reply gives way to a
// stub, old tool results to placeholders, and the whole conversation to a
// summary of it. Each such change is an entry of its own, an edit, that says
// what the messages show from then on. The entries it covers stay in the
// file, and the same file always gives the same messages.
//
// A model's reasoning is sent back only while the conversation before it is
// the one it was written in: a server may bind a reasoning block to what
// came before it and refuse the block once that has changed. So the replies
// recorded before a clear or a compaction go back without their reasoning.
package convo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
)

// Version is the format of the file, written in its first entry.
const Version = 1

// Kinds of entry.
const (
	// KindHeader opens the file.
	KindHeader = "header"
	// KindUser is a message from the runner: a prompt.
	KindUser = "user"
	// KindAssistant is a whole reply of the model.
	KindAssistant = "assistant"
	// KindCall says a call is about to run.
	KindCall = "call"
	// KindResult is a call's result.
	KindResult = "result"
	// KindUsage is a model request's token count.
	KindUsage = "usage"
	// KindStub replaces the assistant message before it with its text: a
	// reply the output limit cut off, taken out of what the model is sent.
	KindStub = "stub"
	// KindClear replaces the results of the calls it lists with short
	// placeholders.
	KindClear = "clear"
	// KindCompaction replaces everything before it with its text, a summary
	// of the conversation, but for the entries from KeepFrom on.
	KindCompaction = "compaction"
)

// Who wrote a user entry that is not the runner's prompt.
const (
	// NoteNudge is the runtime's message after a repeated call.
	NoteNudge = "nudge"
	// NoteCut is the runtime's message after a cut-off reply.
	NoteCut = "cut"
	// NoteContinue is the runner's answer to a turn that was about to end.
	NoteContinue = "continue"
	// NoteSteer is a message the runner sent while a turn was under way.
	NoteSteer = "steer"
	// NoteStopped is the runtime's message after a turn the runner stopped.
	NoteStopped = "stopped"
)

// Entry is one line of the file. Kind says which fields are set.
type Entry struct {
	Kind string `json:"kind"`
	// At is when the entry was written.
	At string `json:"at,omitempty"`

	// Version and Conversation are set on the header.
	Version      int    `json:"version,omitempty"`
	Conversation string `json:"conversation,omitempty"`

	// Text is a user message's text, a stub's, or a compaction's summary as
	// the model is sent it.
	Text string `json:"text,omitempty"`
	// Note says a user entry is not the runner's prompt and who wrote it,
	// one of the Note constants. A prompt has none.
	Note string `json:"note,omitempty"`
	// Parts is an assistant message.
	Parts []llm.Part `json:"parts,omitempty"`
	// Finish is why the assistant's reply ended.
	Finish string `json:"finish,omitempty"`

	// CallID names the call of a call or result entry, and Result is the
	// result of a result entry.
	CallID string    `json:"callId,omitempty"`
	Result *llm.Part `json:"result,omitempty"`

	// Usage is set on a usage entry.
	Usage *llm.Usage `json:"usage,omitempty"`

	// Cleared is set on a clear entry: each result it replaces.
	Cleared []Cleared `json:"cleared,omitempty"`

	// KeepFrom, on a compaction entry, is the index in the file of the first
	// entry the summary does not replace. Zero keeps none.
	KeepFrom int `json:"keepFrom,omitempty"`
	// Reason says why a compaction or a clear was made, what a stub stands
	// for, and, on a usage entry, what a request was that was not one of
	// the conversation's own.
	Reason string `json:"reason,omitempty"`
	// Window, on a compaction entry, is the model's window in tokens as the
	// harness had learned it from the server by then, zero when it had not.
	Window int `json:"window,omitempty"`
}

// Cleared is one result a clear entry replaces: the call, and the text the
// model is sent in the result's place.
type Cleared struct {
	CallID string `json:"callId"`
	Text   string `json:"text"`
}

// Texts the model is shown for a call that has no recorded result.
const (
	// Interrupted is the result of a call that was started and whose
	// result was never recorded.
	Interrupted = "This call was interrupted before its result was recorded. It may have run in part. It was not run again."
	// NotRun is the result of a call that was never started.
	NotRun = "This call was not run."
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Log is an open conversation.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	entries []Entry
	now     func() time.Time
}

// Open opens the conversation id in dir, creating it when there is none.
// It reports whether the conversation already held a message.
func Open(dir, id string) (*Log, bool, error) {
	if !idPattern.MatchString(id) {
		return nil, false, fmt.Errorf("conversation id %q: use letters, digits, '.', '_' and '-', at most 128", id)
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("conversation %s: %w", id, err)
	}
	l := &Log{f: f, now: time.Now}
	if err := l.load(id); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("conversation %s: %w", id, err)
	}
	existed := false
	for _, e := range l.entries {
		if e.Kind == KindUser || e.Kind == KindAssistant {
			existed = true
			break
		}
	}
	return l, existed, nil
}

// load reads the file's entries, drops a last line a dying process left
// half written, and writes the header of a new file.
func (l *Log) load(id string) error {
	raw, err := os.ReadFile(l.f.Name())
	if err != nil {
		return err
	}
	good := 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 256<<20)
	for sc.Scan() {
		line := sc.Bytes()
		end := good + len(line) + 1
		if end > len(raw) {
			// The last line has no newline: it was not finished.
			break
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("line %d is not an entry: %w", len(l.entries)+1, err)
		}
		l.entries = append(l.entries, e)
		good = end
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if good != len(raw) {
		if err := l.f.Truncate(int64(good)); err != nil {
			return err
		}
	}
	if _, err := l.f.Seek(int64(good), 0); err != nil {
		return err
	}
	if len(l.entries) == 0 {
		return l.append(Entry{Kind: KindHeader, Version: Version, Conversation: id})
	}
	h := l.entries[0]
	switch {
	case h.Kind != KindHeader:
		return errors.New("the file does not start with a header")
	case h.Version != Version:
		return fmt.Errorf("the file is format %d, this harness reads format %d", h.Version, Version)
	case h.Conversation != id:
		return fmt.Errorf("the file holds conversation %q", h.Conversation)
	}
	return nil
}

// Append writes one entry and makes it durable before it returns.
func (l *Log) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(e)
}

func (l *Log) append(e Entry) error {
	e.At = l.now().UTC().Format(time.RFC3339Nano)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return err
	}
	if _, err := l.f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.entries = append(l.entries, e)
	return nil
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }

// Entries returns a copy of the entries.
func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Messages is the conversation as a model request carries it. Each
// assistant message's calls are answered in the message that follows it, in
// the order the model made them; a call with no recorded result is answered
// as failed.
func (l *Log) Messages() []llm.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Project(l.entries)
}

// Len is how many entries the file holds.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Live is the index of the first entry the messages are built from: the one
// after the last compaction, or the first that compaction kept.
func Live(entries []Entry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind != KindCompaction {
			continue
		}
		if k := entries[i].KeepFrom; k > 0 && k < i {
			return k
		}
		return i + 1
	}
	return 0
}

// Project is Messages for a list of entries.
func Project(entries []Entry) []llm.Message {
	started := map[string]bool{}
	results := map[string]llm.Part{}
	cleared := map[string]string{}
	stubs := map[int]string{}
	lastAssistant := -1
	summary := -1
	// edited is the index of the last entry that changed what earlier
	// messages show. The replies before it are sent without reasoning.
	edited := -1
	for i, e := range entries {
		switch e.Kind {
		case KindCall:
			started[e.CallID] = true
		case KindResult:
			if e.Result != nil {
				if _, dup := results[e.CallID]; !dup {
					results[e.CallID] = *e.Result
				}
			}
		case KindAssistant:
			lastAssistant = i
		case KindStub:
			if lastAssistant >= 0 {
				stubs[lastAssistant] = e.Text
			}
		case KindClear:
			edited = i
			for _, c := range e.Cleared {
				cleared[c.CallID] = c.Text
			}
		case KindCompaction:
			edited = i
			summary = i
		}
	}
	answer := func(calls []llm.Part) []llm.Part {
		var out []llm.Part
		for _, c := range calls {
			r, ok := results[c.CallID]
			switch text, gone := cleared[c.CallID]; {
			case ok && gone:
				r = llm.Part{Text: text}
			case !ok:
				r = llm.Part{Kind: llm.Result, Failed: true, Text: NotRun}
				if started[c.CallID] {
					r.Text = Interrupted
				}
			}
			r.Kind, r.CallID, r.Name = llm.Result, c.CallID, c.Name
			out = append(out, r)
		}
		return out
	}

	var msgs []llm.Message
	// user adds parts to the conversation as the user's. Entries that
	// follow one another without a reply between them are one message.
	user := func(parts ...llm.Part) {
		if n := len(msgs); n > 0 && msgs[n-1].Role == llm.User {
			msgs[n-1].Parts = append(msgs[n-1].Parts, parts...)
			return
		}
		msgs = append(msgs, llm.Message{Role: llm.User, Parts: parts})
	}
	if summary >= 0 {
		user(llm.Part{Kind: llm.Text, Text: entries[summary].Text})
	}
	var open []llm.Part // the last assistant message's calls, not yet answered
	for i := Live(entries); i < len(entries); i++ {
		e := entries[i]
		switch e.Kind {
		case KindUser:
			parts := answer(open)
			open = nil
			user(append(parts, llm.Part{Kind: llm.Text, Text: e.Text})...)
		case KindAssistant:
			if len(open) > 0 {
				user(answer(open)...)
				open = nil
			}
			if stub, ok := stubs[i]; ok {
				msgs = append(msgs, llm.Message{Role: llm.Assistant, Parts: []llm.Part{{Kind: llm.Text, Text: stub}}})
				continue
			}
			var parts []llm.Part
			for _, p := range e.Parts {
				if p.Kind == llm.Reasoning && i < edited {
					continue
				}
				parts = append(parts, p)
				if p.Kind == llm.Call {
					open = append(open, p)
				}
			}
			// A reply that was reasoning alone leaves no message.
			if len(parts) > 0 {
				msgs = append(msgs, llm.Message{Role: llm.Assistant, Parts: parts})
			}
		}
	}
	if len(open) > 0 {
		user(answer(open)...)
	}
	return msgs
}
