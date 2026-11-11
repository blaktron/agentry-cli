// Package convo is a conversation's record: an append-only file of one JSON
// entry a line, and the messages a model request is built from, which are a
// pure function of that file.
//
// Nothing is ever rewritten. A tool call is recorded before it runs and its
// result after, so a process that died in between leaves a call with no
// result; such a call is shown to the model as failed and is never run
// again.
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
)

// Entry is one line of the file. Kind says which fields are set.
type Entry struct {
	Kind string `json:"kind"`
	// At is when the entry was written.
	At string `json:"at,omitempty"`

	// Version and Conversation are set on the header.
	Version      int    `json:"version,omitempty"`
	Conversation string `json:"conversation,omitempty"`

	// Text is a user message's text.
	Text string `json:"text,omitempty"`
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

// Project is Messages for a list of entries.
func Project(entries []Entry) []llm.Message {
	started := map[string]bool{}
	results := map[string]llm.Part{}
	for _, e := range entries {
		switch e.Kind {
		case KindCall:
			started[e.CallID] = true
		case KindResult:
			if e.Result != nil {
				if _, dup := results[e.CallID]; !dup {
					results[e.CallID] = *e.Result
				}
			}
		}
	}
	answer := func(calls []llm.Part) []llm.Part {
		var out []llm.Part
		for _, c := range calls {
			r, ok := results[c.CallID]
			if !ok {
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
	var open []llm.Part // the last assistant message's calls, not yet answered
	for _, e := range entries {
		switch e.Kind {
		case KindUser:
			parts := answer(open)
			open = nil
			parts = append(parts, llm.Part{Kind: llm.Text, Text: e.Text})
			msgs = append(msgs, llm.Message{Role: llm.User, Parts: parts})
		case KindAssistant:
			if len(open) > 0 {
				msgs = append(msgs, llm.Message{Role: llm.User, Parts: answer(open)})
				open = nil
			}
			msgs = append(msgs, llm.Message{Role: llm.Assistant, Parts: append([]llm.Part(nil), e.Parts...)})
			for _, p := range e.Parts {
				if p.Kind == llm.Call {
					open = append(open, p)
				}
			}
		}
	}
	if len(open) > 0 {
		msgs = append(msgs, llm.Message{Role: llm.User, Parts: answer(open)})
	}
	return msgs
}
