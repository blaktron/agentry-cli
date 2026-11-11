// Package harness is the runner's own model loop: it sends a conversation to
// a model, runs the tool calls the model asks for through the run's one tool
// gateway, and repeats until the model answers.
//
// The harness has no tool of its own and reaches nothing but the model and
// the gateway. It reports what it does as events (package proto) and keeps
// the conversation in an append-only log (package convo), so a process that
// is started again on the same conversation carries on from what was
// recorded.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/harness/convo"
	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
	"github.com/blaktron/agentry-cli/runnerimage/harness/mcp"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// ToolSource is where the tools come from. *mcp.Client is one.
type ToolSource interface {
	Initialize(ctx context.Context) error
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (*mcp.Result, error)
}

// Texts the model is shown for a call the harness did not run.
const (
	// notRunCut answers a call in a reply that stopped at the output limit.
	notRunCut = "Not run: the reply reached the output limit, so this call's arguments may be incomplete. Make the call again with complete arguments."
	// notRunNoAsk answers a call in a reply that did not end by asking for
	// its calls.
	notRunNoAsk = "Not run: the reply did not end by asking for its tool calls to be run."
	// notRunArguments answers a call whose arguments are not an object.
	notRunArguments = "Not run: the arguments were not a JSON object. Make the call again with arguments that match the tool's schema."
)

// Harness runs turns of one conversation.
type Harness struct {
	cfg   proto.Config
	model llm.Client
	src   ToolSource

	outMu sync.Mutex
	out   io.Writer
	seq   int64

	log   *convo.Log
	conv  string
	tools []llm.Tool
	known map[string]bool
	// toolBytes is the size of the tools as a request carries them.
	toolBytes int
	// unchanged are the tools whose results are not scrubbed.
	unchanged map[string]bool

	// Gate, when set, is asked before a turn ends on the model's answer,
	// after the turn_ending event. It returns a message to go on with, or
	// "" to end the turn.
	Gate func(ctx context.Context) (string, error)
	// Now is the clock; time.Now unless a test sets another.
	Now func() time.Time

	// bytesPerToken is what the counted requests have run at, zero before
	// the first. learnedWindow is the model's window as a server's refusal
	// stated or implied it, zero when none has. compactPast is the size the
	// conversation must pass before another summary is tried, after one
	// that could not be made.
	bytesPerToken float64
	learnedWindow int64
	compactPast   int64
}

// New makes a harness that writes its events to out.
func New(cfg proto.Config, model llm.Client, src ToolSource, out io.Writer) *Harness {
	return &Harness{cfg: cfg, model: model, src: src, out: out, Now: time.Now}
}

// Start opens the conversation, creating it when it does not exist, reads
// the gateway's tools and writes the ready event.
func (h *Harness) Start(ctx context.Context, conversation string) error {
	log, _, err := convo.Open(h.cfg.SessionDir, conversation)
	if err != nil {
		h.fail(proto.ClassHarness, err.Error())
		return err
	}
	h.log, h.conv = log, conversation

	if err := h.src.Initialize(ctx); err != nil {
		h.fail(proto.ClassGateway, err.Error())
		return err
	}
	listed, err := h.src.ListTools(ctx)
	if err != nil {
		h.fail(proto.ClassGateway, err.Error())
		return err
	}
	// The same tools in the same order on every request, whatever order
	// the gateway lists them in, so the model's server can reuse what it
	// has read.
	sort.Slice(listed, func(i, j int) bool { return listed[i].Name < listed[j].Name })
	h.known = map[string]bool{}
	names := make([]string, 0, len(listed))
	for _, t := range listed {
		if h.known[t.Name] {
			continue
		}
		h.known[t.Name] = true
		h.tools = append(h.tools, llm.Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
		h.toolBytes += len(t.Name) + len(t.Description) + len(t.InputSchema)
		names = append(names, t.Name)
	}
	h.unchanged = map[string]bool{}
	if h.cfg.Scrub != nil {
		for _, t := range h.cfg.Scrub.Unchanged {
			h.unchanged[t] = true
		}
	}
	// What an earlier process learned of the model's window.
	for _, e := range h.log.Entries() {
		if e.Kind == convo.KindCompaction && e.Window > 0 {
			h.learnedWindow = int64(e.Window)
		}
	}
	h.emit(proto.Event{Type: proto.TypeReady, Conversation: conversation, Tools: names})
	return nil
}

// Close closes the conversation's log.
func (h *Harness) Close() error {
	if h.log == nil {
		return nil
	}
	return h.log.Close()
}

// Prompt adds text to the conversation as the runner's message and runs the
// turn it starts. It returns why the turn ended, one of proto's End
// constants, having written the turn_end event. The error is non-nil only
// when the turn ended for a failure, which an error event has reported.
func (h *Harness) Prompt(ctx context.Context, text string) (string, error) {
	if err := h.log.Append(convo.Entry{Kind: convo.KindUser, Text: text}); err != nil {
		return h.endFailed(proto.ClassHarness, err)
	}
	if err := h.coldClear(); err != nil {
		return h.endFailed(proto.ClassHarness, err)
	}
	var watch loopWatch
	// recoveries counts this turn's replies taken out as cut off.
	// shortened says the request about to be sent is the one a refusal as
	// too long was answered with a summary for: a second refusal ends the
	// turn.
	recoveries, shortened := 0, false
	for step := 1; ; step++ {
		if h.cfg.MaxSteps > 0 && step > h.cfg.MaxSteps {
			return h.end(proto.EndMaxSteps, ""), nil
		}
		if !shortened && h.nearWindow() {
			// A conversation already past the window would be refused
			// whole, so its summary is asked for of the bounded copy.
			size, first := h.contextTokens(), 0
			if size >= h.window() {
				first = 1
			}
			switch err := h.compact(ctx, reasonThreshold, first); {
			case errors.Is(err, errNoSummary):
				// The conversation goes on as it is, and is not
				// tried again until it has grown by a tenth.
				h.compactPast = size + size/10
			case err != nil:
				return h.endModelError(err)
			}
		}
		msgs := h.log.Messages()
		size := len(h.cfg.System) + h.toolBytes + messagesBytes(msgs)
		reply, err := h.stream(ctx, llm.Request{
			System:    h.cfg.System,
			Messages:  msgs,
			Tools:     h.tools,
			MaxTokens: h.cfg.Model.MaxOutputTokens,
		}, h.delta)
		if err != nil {
			if !llm.IsError(err, llm.Overflow) || h.cfg.Compaction == nil || shortened {
				return h.endModelError(err)
			}
			// The server refused the conversation as too long: one
			// summary, made from a bounded copy of it, and the request
			// again. The refusal ends the turn when no summary can be had.
			h.learn(err, msgs)
			switch cerr := h.compact(ctx, reasonOverflow, 1); {
			case errors.Is(cerr, errNoSummary):
				return h.endModelError(err)
			case cerr != nil:
				return h.endModelError(cerr)
			}
			shortened = true
			step--
			continue
		}
		shortened = false
		h.calibrate(size, reply.Usage.Input+reply.Usage.CacheRead+reply.Usage.CacheWrite)
		if err := h.record(reply); err != nil {
			return h.endFailed(proto.ClassHarness, err)
		}

		var calls []llm.Part
		var answer strings.Builder
		for _, p := range reply.Parts {
			switch p.Kind {
			case llm.Call:
				calls = append(calls, p)
			case llm.Text:
				answer.WriteString(p.Text)
			}
		}

		// A reply the output limit cut off is taken out of what the model
		// is sent: a stub stands for it, the model is told what happened,
		// and the turn goes on. Its calls are not run.
		if reply.Finish == llm.FinishLength && recoveries < h.cfg.LengthRecoveries {
			recoveries++
			if err := h.recoverCut(reply, recoveries); err != nil {
				return h.endFailed(proto.ClassHarness, err)
			}
			continue
		}

		// Calls are run only when the reply ended by asking for them. A
		// reply that ended any other way is whole only up to its last
		// finished part, so its calls are answered and never run.
		if reply.Finish != llm.FinishToolCalls {
			why := notRunNoAsk
			if reply.Finish == llm.FinishLength {
				why = notRunCut
			}
			for _, c := range calls {
				if err := h.result(c, llm.Part{Failed: true, Text: why}); err != nil {
					return h.endFailed(proto.ClassHarness, err)
				}
			}
			if reply.Finish == llm.FinishLength {
				return h.end(proto.EndAnswerCut, answer.String()), nil
			}
			calls = nil
		}
		if len(calls) == 0 {
			// The model answered. The runner may have more for it.
			more, err := h.ending(ctx)
			if err != nil {
				return h.endFailed(proto.ClassHarness, err)
			}
			if more == "" {
				return h.end(proto.EndAnswered, answer.String()), nil
			}
			// The message is the runner's, and is said to be: the model
			// must not take it for the person's.
			if err := h.log.Append(convo.Entry{Kind: convo.KindUser, Text: runtimeNote + more, Note: convo.NoteContinue}); err != nil {
				return h.endFailed(proto.ClassHarness, err)
			}
			continue
		}
		var round []string
		times := 0
		for _, c := range calls {
			if err := h.run(ctx, c); err != nil {
				if ctx.Err() != nil {
					return h.endFailed(proto.ClassHarness, ctx.Err())
				}
				return h.endFailed(proto.ClassHarness, err)
			}
			watch.add(h.display(c.Name), c.Arguments)
			// Looked for after every call, so a streak inside one reply's
			// calls is seen even when a later call of the reply ends it.
			if r, n := watch.repeating(h.cfg.Loop); n > 0 {
				round, times = r, n
				watch.reset()
			}
		}
		// The message follows the results of all the reply's calls.
		if times > 0 {
			if err := h.nudge(round, times); err != nil {
				return h.endFailed(proto.ClassHarness, err)
			}
		}
	}
}

// nearWindow reports whether the conversation has reached the share of the
// model's window at which it is summarised.
func (h *Harness) nearWindow() bool {
	c, w := h.cfg.Compaction, h.window()
	if c == nil || w <= 0 {
		return false
	}
	now := h.contextTokens()
	return float64(now) >= c.Threshold*float64(w) && now > h.compactPast
}

// ending asks the runner whether the turn ends on the model's answer. With
// no one to ask it does.
func (h *Harness) ending(ctx context.Context) (string, error) {
	if h.Gate == nil {
		return "", nil
	}
	h.emit(proto.Event{Type: proto.TypeTurnEnding})
	return h.Gate(ctx)
}

// display is a tool's name as the harness repeats it to the model: the name
// of a tool the gateway listed, and "a tool" for any other, which is the
// model's own text.
func (h *Harness) display(name string) string {
	if h.known[name] {
		return name
	}
	return "a tool"
}

// recoverCut takes a reply the output limit cut off out of what the model
// is sent, and tells the model. n is how many this turn has now had.
func (h *Harness) recoverCut(reply *llm.Response, n int) error {
	kind, calls, chars := cutKind(reply)
	for i := range calls {
		calls[i] = h.display(calls[i])
	}
	if err := h.log.Append(convo.Entry{Kind: convo.KindStub, Text: cutStub(kind, calls, chars), Reason: kind}); err != nil {
		return err
	}
	last := n >= h.cfg.LengthRecoveries
	if err := h.log.Append(convo.Entry{Kind: convo.KindUser, Note: convo.NoteCut, Text: cutInstruction(kind, calls, chars, last)}); err != nil {
		return err
	}
	h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnLengthRecovery, Reason: kind, Count: n,
		Text: fmt.Sprintf("A reply cut off at the output limit (%s) was taken out of the conversation and the model was told why (%d of %d).", kind, n, h.cfg.LengthRecoveries)})
	return nil
}

// nudge tells the model, after the results of a repeated call, to reach its
// goal another way. The turn goes on.
func (h *Harness) nudge(round []string, times int) error {
	earlier := 0
	for _, e := range h.log.Entries() {
		if e.Kind == convo.KindUser && e.Note == convo.NoteNudge {
			earlier++
		}
	}
	if err := h.log.Append(convo.Entry{Kind: convo.KindUser, Note: convo.NoteNudge, Text: nudgeText(round, times, earlier)}); err != nil {
		return err
	}
	h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnLoopNudge, Tool: round[0], Count: times,
		Text: fmt.Sprintf("%d identical rounds of calls (%s) in a row; the model was told to reach its goal another way.", times, joinNames(round))})
	return nil
}

// delta passes a piece of a streaming reply on as an event.
func (h *Harness) delta(d llm.Delta) {
	switch d.Kind {
	case llm.Text:
		h.emit(proto.Event{Type: proto.TypeText, Text: d.Text})
	case llm.Reasoning:
		h.emit(proto.Event{Type: proto.TypeReasoning, Text: d.Text})
	}
}

// record writes a whole reply and its token count to the log and reports
// the count.
func (h *Harness) record(reply *llm.Response) error {
	if err := h.log.Append(convo.Entry{Kind: convo.KindAssistant, Parts: reply.Parts, Finish: string(reply.Finish)}); err != nil {
		return err
	}
	return h.usage(reply.Usage, "")
}

// usage records one model request's token count and reports it. what is ""
// for a request of the conversation, and says what else the request was.
func (h *Harness) usage(u llm.Usage, what string) error {
	if err := h.log.Append(convo.Entry{Kind: convo.KindUsage, Usage: &u, Reason: what}); err != nil {
		return err
	}
	h.emit(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{
		Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output,
		ContextTokens: u.Input + u.CacheRead + u.CacheWrite,
		ContextLimit:  h.window(),
		CostUSD:       h.cost(),
	}})
	return nil
}

// cost is what the conversation's recorded requests have cost at the
// configuration's prices, zero when it gave none. It is counted from the
// record, so it is the same after the harness is started again.
func (h *Harness) cost() float64 {
	p := h.cfg.Model.Prices
	if p == nil {
		return 0
	}
	var total float64
	for _, e := range h.log.Entries() {
		if e.Kind != convo.KindUsage || e.Usage == nil {
			continue
		}
		u := e.Usage
		total += float64(u.Input)*p.Input + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite + float64(u.Output)*p.Output
	}
	return total / 1e6
}

// run runs one call through the gateway. The call is recorded before it is
// sent and its result after, so a call is never sent twice. The error is
// non-nil only when the log could not be written or the turn was cancelled.
func (h *Harness) run(ctx context.Context, c llm.Part) error {
	args := json.RawMessage(c.Arguments)
	if strings.TrimSpace(c.Arguments) == "" {
		args = json.RawMessage(`{}`)
	}
	if h.cfg.Scrub != nil {
		// What the tool is given, and what the event shows, holds no
		// secret-shaped value. The model's own message keeps its words.
		args = scrubArguments(args)
	}
	h.emit(proto.Event{Type: proto.TypeToolCall, CallID: c.CallID, Tool: c.Name, Arguments: validJSON(args)})

	var obj map[string]json.RawMessage
	switch {
	case !h.known[c.Name]:
		return h.resultEvent(c, llm.Part{Failed: true, Text: fmt.Sprintf("Not run: there is no tool named %q.", c.Name)}, false)
	case json.Unmarshal(args, &obj) != nil || obj == nil:
		return h.resultEvent(c, llm.Part{Failed: true, Text: notRunArguments}, false)
	}

	if err := h.log.Append(convo.Entry{Kind: convo.KindCall, CallID: c.CallID}); err != nil {
		return err
	}
	res, err := h.src.CallTool(ctx, c.Name, args)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The gateway's refusal, or a failure to reach it, is the call's
		// result: the model can read why and choose what to do.
		return h.resultEvent(c, llm.Part{Failed: true, Text: err.Error()}, false)
	}
	part, unchanged := resultPart(res)
	return h.resultEvent(c, part, unchanged)
}

// resultPart turns a tool's result into what the model is sent. Text items
// are joined; an embedded resource's text is passed on whole after them;
// images stay images. unchanged reports that a resource was passed on.
func resultPart(res *mcp.Result) (part llm.Part, unchanged bool) {
	var text []string
	for _, c := range res.Content {
		switch {
		case c.Type == "text":
			text = append(text, c.Text)
		case c.Type == "image" && c.Data != "":
			part.Images = append(part.Images, llm.Image{MediaType: c.MimeType, Data: c.Data})
		case c.Type == "resource" && c.Resource != nil && c.Resource.Text != "":
			text = append(text, c.Resource.Text)
			unchanged = true
		}
	}
	part.Text = strings.Join(text, "\n")
	part.Failed = res.IsError
	return part, unchanged
}

// shape is a result as the model is sent it: scrubbed, but for a tool whose
// results are passed on as they are, and held to the result policy's size.
func (h *Harness) shape(c llm.Part, r llm.Part) llm.Part {
	r.Kind, r.CallID, r.Name = llm.Result, c.CallID, c.Name
	if h.cfg.Scrub != nil && !h.unchanged[c.Name] {
		r.Text = scrubText(r.Text)
	}
	if h.cfg.Results != nil {
		r.Text, _ = cutResult(r.Text, h.cfg.Results.MaxBytes)
	}
	return r
}

// result records a call's result without reporting it as an event: the
// result of a call that was never offered to the gateway. The record holds
// what the model is sent.
func (h *Harness) result(c llm.Part, r llm.Part) error {
	r = h.shape(c, r)
	return h.log.Append(convo.Entry{Kind: convo.KindResult, CallID: c.CallID, Result: &r})
}

// resultEvent records a call's result and reports it.
func (h *Harness) resultEvent(c llm.Part, r llm.Part, unchanged bool) error {
	r = h.shape(c, r)
	if err := h.log.Append(convo.Entry{Kind: convo.KindResult, CallID: c.CallID, Result: &r}); err != nil {
		return err
	}
	ev := proto.Event{Type: proto.TypeToolResult, CallID: c.CallID, Tool: c.Name, Failed: r.Failed, Unchanged: unchanged}
	if !unchanged {
		ev.Text = r.Text
	}
	h.emit(ev)
	return nil
}

func (h *Harness) end(reason, text string) string {
	h.emit(proto.Event{Type: proto.TypeTurnEnd, Reason: reason, Text: text, Conversation: h.conv})
	return reason
}

func (h *Harness) fail(class, text string) {
	h.emit(proto.Event{Type: proto.TypeError, Class: class, Text: text})
}

func (h *Harness) endFailed(class string, err error) (string, error) {
	h.fail(class, err.Error())
	return h.end(proto.EndError, ""), err
}

// endModelError reports a failed model request with the class its client
// gave it and the server's own words.
func (h *Harness) endModelError(err error) (string, error) {
	var me *llm.Error
	if !errors.As(err, &me) {
		return h.endFailed(proto.ClassHarness, err)
	}
	h.emit(proto.Event{
		Type: proto.TypeError, Class: string(me.Class), Status: me.Status, Text: me.Message,
		RetryAfterSeconds: int(me.RetryAfter.Seconds()),
	})
	return h.end(proto.EndError, ""), err
}

// emit writes one event as a line.
func (h *Harness) emit(ev proto.Event) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	h.seq++
	ev.V, ev.Seq = proto.Version, h.seq
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = h.out.Write(append(line, '\n'))
}

// validJSON is raw when it is JSON, and raw as a JSON string otherwise, so
// an event always carries arguments a reader can parse.
func validJSON(raw json.RawMessage) json.RawMessage {
	if json.Valid(raw) {
		return raw
	}
	s, _ := json.Marshal(string(raw))
	return s
}
