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
}

// New makes a harness that writes its events to out.
func New(cfg proto.Config, model llm.Client, src ToolSource, out io.Writer) *Harness {
	return &Harness{cfg: cfg, model: model, src: src, out: out}
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
		names = append(names, t.Name)
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
	for step := 1; ; step++ {
		if h.cfg.MaxSteps > 0 && step > h.cfg.MaxSteps {
			return h.end(proto.EndMaxSteps, ""), nil
		}
		reply, err := h.model.Stream(ctx, llm.Request{
			System:    h.cfg.System,
			Messages:  h.log.Messages(),
			Tools:     h.tools,
			MaxTokens: h.cfg.Model.MaxOutputTokens,
		}, h.delta)
		if err != nil {
			return h.endModelError(err)
		}
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
			return h.end(proto.EndAnswered, answer.String()), nil
		}
		if len(calls) == 0 {
			return h.end(proto.EndAnswered, answer.String()), nil
		}
		for _, c := range calls {
			if err := h.run(ctx, c); err != nil {
				if ctx.Err() != nil {
					return h.endFailed(proto.ClassHarness, ctx.Err())
				}
				return h.endFailed(proto.ClassHarness, err)
			}
		}
	}
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
	u := reply.Usage
	if err := h.log.Append(convo.Entry{Kind: convo.KindUsage, Usage: &u}); err != nil {
		return err
	}
	h.emit(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{
		Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output,
		ContextTokens: u.Input + u.CacheRead + u.CacheWrite,
		ContextLimit:  int64(h.cfg.Model.ContextWindow),
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

// result records a call's result without reporting it as an event: the
// result of a call that was never offered to the gateway.
func (h *Harness) result(c llm.Part, r llm.Part) error {
	r.Kind, r.CallID, r.Name = llm.Result, c.CallID, c.Name
	return h.log.Append(convo.Entry{Kind: convo.KindResult, CallID: c.CallID, Result: &r})
}

// resultEvent records a call's result and reports it.
func (h *Harness) resultEvent(c llm.Part, r llm.Part, unchanged bool) error {
	if err := h.result(c, r); err != nil {
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
