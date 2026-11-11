package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/harness/convo"
	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// How large a conversation is. The server's own count of a request is the
// truth; only what has been added since that request is estimated, from its
// bytes.
const (
	// bytesPerToken is the estimate until a request has been counted: text
	// in most languages and most code runs to between three and four bytes
	// a token.
	bytesPerToken = 3.5
	// imageBytes stands for one image, whose cost in tokens does not follow
	// its size in bytes.
	imageBytes = 7000
	// partBytes stands for what a part costs beside its text.
	partBytes = 16
)

// messagesBytes is the size of what msgs carry.
func messagesBytes(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			n += partSize(p)
		}
	}
	return n
}

func partSize(p llm.Part) int {
	return partBytes + len(p.Text) + len(p.Name) + len(p.Arguments) + len(p.Images)*imageBytes
}

// entrySize is the size of what one entry adds to the messages.
func entrySize(e convo.Entry) int {
	switch e.Kind {
	case convo.KindUser:
		return partBytes + len(e.Text)
	case convo.KindResult:
		if e.Result != nil {
			return partSize(*e.Result)
		}
	case convo.KindAssistant:
		n := 0
		for _, p := range e.Parts {
			n += partSize(p)
		}
		return n
	}
	return 0
}

// contextTokens estimates the size, in tokens, of the request the
// conversation would now make: the last counted request and its reply, plus
// what has been added since at the rate the counted requests have run at.
// With no counted request since the last summary, the whole of it is
// estimated.
func (h *Harness) contextTokens() int64 {
	entries := h.log.Entries()
	// The last counted request, unless a summary has replaced the
	// conversation it measured.
	last := -1
	for i := len(entries) - 1; i >= 0 && entries[i].Kind != convo.KindCompaction; i-- {
		// A summary request measures the conversation and the summary
		// both, so it is no measure of the conversation.
		if entries[i].Kind == convo.KindUsage && entries[i].Usage != nil && entries[i].Reason == "" {
			last = i
			break
		}
	}
	if last < 0 {
		size := len(h.cfg.System) + h.toolBytes + messagesBytes(convo.Project(entries))
		return int64(float64(size) / h.rate())
	}
	u := entries[last].Usage
	tail := 0
	for _, e := range entries[last+1:] {
		if e.Kind != convo.KindAssistant {
			tail += entrySize(e)
		}
	}
	return u.Input + u.CacheRead + u.CacheWrite + u.Output + int64(float64(tail)/h.rate())
}

// rate is bytes a token, as the counted requests have run at, held between
// two and six so one odd count cannot throw every later estimate.
func (h *Harness) rate() float64 {
	if h.bytesPerToken <= 0 {
		return bytesPerToken
	}
	return min(max(h.bytesPerToken, 2), 6)
}

// calibrate notes that a request of size bytes was counted as tokens.
func (h *Harness) calibrate(bytes int, tokens int64) {
	if bytes > 4096 && tokens > 0 {
		h.bytesPerToken = float64(bytes) / float64(tokens)
	}
}

// window is the model's window in tokens: the configuration's, or the one a
// server's refusal has taught the harness when the configuration gives none
// or a larger one (the server is the one that refuses); zero when neither
// is known.
func (h *Harness) window() int64 {
	w := int64(h.cfg.Model.ContextWindow)
	if h.learnedWindow > 0 && (w <= 0 || h.learnedWindow < w) {
		return h.learnedWindow
	}
	return w
}

// Why a compaction is made (convo.Entry.Reason).
const (
	reasonThreshold = proto.CompactionThreshold
	reasonOverflow  = proto.CompactionOverflow
)

// The summary request, and the message a summary reaches the model in.
const (
	// summaryAsk is the instruction a summary request ends with.
	summaryAsk = runtimeNote + "this conversation is about to be replaced by a summary of it, " +
		"because it is close to the most the model can read at once. " +
		"Write that summary now, as your whole reply, and call no tool. " +
		"Whoever carries on will have the summary and the tools and nothing else of this conversation, " +
		"so the summary must hold everything needed to go on from here. " +
		"If the conversation opens with an earlier summary, carry over what still matters from it."
	// summaryShort is added when an earlier attempt ran to the output limit.
	summaryShort = " Keep it short: the last summary was cut off at the output limit and could not be used."
	// summaryOpen and summaryClose stand before and after a stored summary.
	summaryOpen = runtimeNote + "the conversation up to this point was replaced by this summary of it, " +
		"to stay inside what the model can read at once.\n\n"
	summaryClose = "\n\nCarry on with the task from where the summary says you are."
	// omitted opens a summary request that leaves out the oldest part of
	// the conversation.
	omitted = "[The earlier part of the conversation is left out of this request.]"
	// summaryResultBytes is how much of each tool result a bounded summary
	// request keeps.
	summaryResultBytes = 2048
	// summaryLevels is how many times a summary request is made smaller
	// before the harness gives up.
	summaryLevels = 3
)

// usageSummary marks the token count of a summary request in the record.
const usageSummary = "summary"

// errNoSummary says no usable summary could be had.
var errNoSummary = errors.New("no summary of the conversation could be made")

// compact replaces the conversation with a summary of it. The summary is
// asked for with the conversation as it stands and the instruction last, so
// the server reads what it has already cached; a request the server refuses
// as too long, or whose reply is cut off or empty, is made again smaller, at
// most summaryLevels times. first is the level to start at: 0 the whole
// conversation, 1 with every tool result bounded.
//
// It returns errNoSummary when no level gave a summary or there is nothing
// for one to replace, and the model's own error when a request failed
// otherwise.
func (h *Harness) compact(ctx context.Context, reason string, first int) error {
	entries := h.log.Entries()
	live := convo.Project(entries)
	keep := keepFrom(entries, h.keepBudget())
	before := h.contextTokens()
	// A summary stands for the replies it replaces. With none before what
	// is kept there is nothing for one to stand for, and asking would only
	// add to the conversation.
	replaces := false
	end := keep
	if end == 0 {
		end = len(entries)
	}
	for _, e := range entries[convo.Live(entries):end] {
		replaces = replaces || e.Kind == convo.KindAssistant
	}
	if !replaces {
		return errNoSummary
	}

	var summary string
	short := false
	for level := first; level <= summaryLevels && summary == ""; level++ {
		ask := summaryAsk
		if c := h.cfg.Compaction; c != nil && c.Checkpoint != "" {
			ask += " " + c.Checkpoint
		}
		if short {
			ask += summaryShort
		}
		msgs := summaryInput(live, level)
		msgs = appendUser(msgs, llm.Part{Kind: llm.Text, Text: ask})
		reply, err := h.stream(ctx, llm.Request{
			System: h.cfg.System, Messages: msgs, Tools: h.tools, MaxTokens: h.cfg.Model.MaxOutputTokens,
		}, func(llm.Delta) {})
		if err != nil {
			if llm.IsError(err, llm.Overflow) {
				h.learn(err, msgs)
				continue
			}
			return err
		}
		// A summary request is a request like any other: counted and paid.
		if err := h.usage(reply.Usage, usageSummary); err != nil {
			return err
		}
		var text strings.Builder
		for _, p := range reply.Parts {
			if p.Kind == llm.Text {
				text.WriteString(p.Text)
			}
		}
		// A summary that ran to the output limit stops mid-thought, and
		// the conversation it stands for would be lost with it.
		if reply.Finish == llm.FinishLength {
			short = true
			continue
		}
		summary = strings.TrimSpace(text.String())
	}
	if summary == "" {
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCompactionFailed, Reason: reason,
			Text: "The conversation could not be summarised, so it was left as it was."})
		return errNoSummary
	}

	stored := summaryOpen + summary
	if c := h.cfg.Compaction; c != nil && c.Notes != "" {
		stored += "\n\nYour notes are in " + c.Notes + ": read that file before anything else, then go on from where it says you are, and keep it up to date."
	}
	stored += summaryClose
	if err := h.log.Append(convo.Entry{Kind: convo.KindCompaction, Text: stored, KeepFrom: keep, Reason: reason, Window: int(h.learnedWindow)}); err != nil {
		return err
	}
	after := h.contextTokens()
	// What a summary leaves may itself be near the window. Another summary
	// of it would gain nothing, so none is made until it has grown.
	h.compactPast = after + after/10
	h.emit(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{
		ContextTokens: after, ContextLimit: h.window(), Compacted: true, CostUSD: h.cost(),
	}})
	h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCompaction, Reason: reason,
		Text: fmt.Sprintf("The conversation was replaced by a summary of it (about %d tokens before, about %d after).", before, after)})
	return nil
}

// learn takes what a refusal says of the model's window: the window it
// states, else, where none is known at all, the size of the request it
// refused, which the window is no larger than.
func (h *Harness) learn(err error, refused []llm.Message) {
	var me *llm.Error
	if !errors.As(err, &me) {
		return
	}
	if me.Window > 0 {
		h.learnedWindow = me.Window
		return
	}
	// A refusal that states no window still says the request was past it.
	size := len(h.cfg.System) + h.toolBytes + messagesBytes(refused)
	if implied := max(int64(float64(size)/h.rate()), 1); h.window() == 0 || implied < h.window() {
		h.learnedWindow = implied
	}
}

// keepBudget is the most, in bytes, a compaction keeps of the conversation
// beside its summary: an eighth of the window, or a small fixed size when
// the window is not known.
func (h *Harness) keepBudget() int {
	if w := h.window(); w > 0 {
		return int(float64(w) * h.rate() / 8)
	}
	return 16 << 10
}

// keepFrom is the index of the first entry a compaction keeps: what the
// model has not answered yet. That is the messages to it that close the
// conversation (a prompt, a note of the runtime's), and before them the
// last reply's calls with their results, when those fit in budget bytes.
// Zero keeps nothing.
func keepFrom(entries []convo.Entry, budget int) int {
	live := convo.Live(entries)
	first := len(entries)
	i := len(entries) - 1
closing:
	for ; i >= live; i-- {
		switch entries[i].Kind {
		case convo.KindUser:
			first = i
		case convo.KindAssistant, convo.KindResult, convo.KindCall:
			break closing
		}
	}
	// The last reply, when it made calls and was not taken out as cut off.
	for j := i; j >= live; j-- {
		if entries[j].Kind != convo.KindAssistant {
			continue
		}
		calls, size := false, 0
		for _, p := range entries[j].Parts {
			calls = calls || p.Kind == llm.Call
		}
		for _, e := range entries[j:first] {
			size += entrySize(e)
			calls = calls && e.Kind != convo.KindStub
		}
		if calls && size <= budget {
			first = j
		}
		break
	}
	if first >= len(entries) {
		return 0
	}
	return first
}

// summaryInput is the conversation as a summary request at level carries
// it. Level 0 is all of it. From level 1 every tool result is held to
// summaryResultBytes and carries no image, and no reply carries its
// reasoning: the conversation it was written in is no longer the one sent.
// Level 2 keeps the newest half of the messages and level 3 the newest
// quarter, after the summary the conversation opens with, if it opens with
// one.
func summaryInput(msgs []llm.Message, level int) []llm.Message {
	if level <= 0 {
		return msgs
	}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		parts := make([]llm.Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			if p.Kind == llm.Reasoning {
				continue
			}
			if p.Kind == llm.Result {
				p.Images = nil
				if len(p.Text) > summaryResultBytes {
					cut := summaryResultBytes
					for cut > 0 && !utf8.RuneStart(p.Text[cut]) {
						cut--
					}
					p.Text = p.Text[:cut] + "\n[The rest of this result is left out of this request.]"
				}
			}
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			out = append(out, llm.Message{Role: m.Role, Parts: parts})
		}
	}
	if level == 1 {
		return out
	}
	from := len(out) - len(out)/(1<<(level-1))
	// A message of results answers the reply before it, so the part that is
	// kept starts at a reply.
	for from < len(out) && out[from].Role != llm.Assistant {
		from++
	}
	if from <= 1 || from >= len(out) {
		return out
	}
	head := llm.Message{Role: llm.User, Parts: []llm.Part{{Kind: llm.Text, Text: omitted}}}
	if first := out[0]; first.Role == llm.User && len(first.Parts) > 0 && strings.HasPrefix(first.Parts[0].Text, summaryOpen) {
		head.Parts = []llm.Part{first.Parts[0], head.Parts[0]}
	}
	return append([]llm.Message{head}, out[from:]...)
}

// appendUser adds part to msgs as the user's, in the last message when that
// is the user's already.
func appendUser(msgs []llm.Message, part llm.Part) []llm.Message {
	out := append([]llm.Message(nil), msgs...)
	if n := len(out); n > 0 && out[n-1].Role == llm.User {
		out[n-1] = llm.Message{Role: llm.User, Parts: append(append([]llm.Part(nil), out[n-1].Parts...), part)}
		return out
	}
	return append(out, llm.Message{Role: llm.User, Parts: []llm.Part{part}})
}

// clearedText is the placeholder a cleared result gives way to. It names the
// tool and the size and repeats nothing of the call or its result.
func clearedText(tool string, bytes int) string {
	return fmt.Sprintf("[cleared: this result of %s was %d bytes; make the call again to read it]", tool, bytes)
}

// coldClear replaces old tool results with placeholders when the server's
// cache of the conversation has expired: the last reply is older than the
// configuration's lifetime, so the next request is read afresh whatever it
// holds. It is called before a turn's first request and never inside a
// turn, where it would throw away a cache that is still warm.
//
// The results of the newest KeepTurns turns before this one stay, and so do
// the newest KeepBrowser results of the browser's tools; every other result
// of a browser tool goes, wherever it falls.
func (h *Harness) coldClear() error {
	cfg := h.cfg.ColdClear
	if cfg == nil {
		return nil
	}
	entries := h.log.Entries()
	live := convo.Live(entries)

	// When the last reply ended.
	var last time.Time
	for i := len(entries) - 1; i >= 0 && last.IsZero(); i-- {
		if entries[i].Kind == convo.KindUsage {
			last, _ = time.Parse(time.RFC3339Nano, entries[i].At)
		}
	}
	idle := h.Now().Sub(last)
	if last.IsZero() || idle <= time.Duration(cfg.AfterSeconds)*time.Second {
		return nil
	}

	browser := map[string]bool{}
	for _, t := range cfg.BrowserTools {
		browser[t] = true
	}
	gone := map[string]bool{}
	for _, e := range entries {
		if e.Kind == convo.KindClear {
			for _, c := range e.Cleared {
				gone[c.CallID] = true
			}
		}
	}
	// Newest first, counting the prompts passed on the way back: a result
	// with one prompt after it belongs to the turn before the current one,
	// which that prompt opened.
	var cleared []convo.Cleared
	prompts, browserSeen := 0, 0
	for i := len(entries) - 1; i >= live; i-- {
		e := entries[i]
		if e.Kind == convo.KindUser && e.Note == "" {
			prompts++
		}
		if e.Kind != convo.KindResult || e.Result == nil || gone[e.CallID] {
			continue
		}
		r := e.Result
		old := prompts > cfg.KeepTurns
		if browser[r.Name] {
			browserSeen++
			old = browserSeen > cfg.KeepBrowser
		}
		// A result no longer than its placeholder is left alone.
		text := clearedText(h.display(r.Name), len(r.Text))
		if !old || (len(r.Text) <= len(text) && len(r.Images) == 0) {
			continue
		}
		cleared = append(cleared, convo.Cleared{CallID: e.CallID, Text: text})
	}
	if len(cleared) == 0 {
		return nil
	}
	// Oldest first, as the conversation has them.
	for i, j := 0, len(cleared)-1; i < j; i, j = i+1, j-1 {
		cleared[i], cleared[j] = cleared[j], cleared[i]
	}
	if err := h.log.Append(convo.Entry{Kind: convo.KindClear, Cleared: cleared, Reason: "cold cache"}); err != nil {
		return err
	}
	h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnColdClear, Count: len(cleared), Reason: "cold cache",
		Text: fmt.Sprintf("%d old tool results were replaced by placeholders: the last reply was %s ago, past the %s the model's server keeps a conversation cached.",
			len(cleared), idle.Round(time.Second), time.Duration(cfg.AfterSeconds)*time.Second)})
	return nil
}

// What stands in for a reply the output limit cut off, and what the model is
// told after it. Both name the tools and the length, never the text that
// was cut.

// cutKind is what the output limit cut: a call, an answer's text, or
// reasoning with no reply yet.
func cutKind(reply *llm.Response) (kind string, calls []string, chars int) {
	for _, p := range reply.Parts {
		switch p.Kind {
		case llm.Call:
			calls = append(calls, p.Name)
		case llm.Text:
			chars += utf8.RuneCountInString(p.Text)
		}
	}
	switch {
	case len(calls) > 0:
		return proto.CutToolCall, calls, chars
	case chars > 0:
		return proto.CutAnswer, nil, chars
	}
	return proto.CutReasoning, nil, 0
}

// cutStub is the assistant message kept in place of the cut-off reply.
func cutStub(kind string, calls []string, chars int) string {
	switch kind {
	case proto.CutToolCall:
		return fmt.Sprintf("[This answer reached the output limit while calling %s. "+
			"The call was not run, and its arguments were not kept.]", joinNames(calls))
	case proto.CutAnswer:
		return fmt.Sprintf("[This answer was cut off at the output limit after %d characters. "+
			"It was taken out of the conversation.]", chars)
	}
	return "[This answer reached the output limit while still reasoning, before any reply. " +
		"Its reasoning was taken out of the conversation.]"
}

// cutInstruction is the message the model gets after the stub. last says no
// further cut-off reply will be recovered from.
func cutInstruction(kind string, calls []string, chars int, last bool) string {
	var text string
	switch kind {
	case proto.CutToolCall:
		text = fmt.Sprintf(runtimeNote+"your last answer reached the output limit "+
			"while calling %s, so the arguments may be cut and the call was not run. "+
			"Call it again with complete arguments. If they are long, split the work into smaller calls.", joinNames(calls))
	case proto.CutAnswer:
		text = fmt.Sprintf(runtimeNote+"your last answer ran past the output limit "+
			"and was cut off after %d characters, so it was taken out of the conversation. "+
			"It may have been repeating itself; do not write it again. "+
			"Take one concrete step now with a tool call, or give a short final answer.", chars)
	default:
		text = runtimeNote + "your last answer spent the whole output limit " +
			"reasoning and gave no reply, so its reasoning was taken out of the conversation. " +
			"Do not reason it all through again. " +
			"Take one concrete step now with a tool call, or give a short final answer."
	}
	if last {
		text += " Another answer cut off at the output limit will end the run."
	}
	return text
}
