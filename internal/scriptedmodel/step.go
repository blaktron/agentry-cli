package scriptedmodel

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Usage is the token count a reply reports.
type Usage struct {
	// Input is the input the server read fresh, not from its cache.
	Input int
	// CacheRead is the input the server read from its cache.
	CacheRead int
	// CacheWrite is the input the server wrote to its cache. Chat
	// Completions has no field for it.
	CacheWrite int
	// Output is the output, reasoning included.
	Output int
	// Reasoning is the part of Output that was reasoning. The Anthropic
	// API has no field for it.
	Reasoning int
}

// Call is one tool call. Arguments is the JSON text of its arguments.
type Call struct {
	ID        string
	Name      string
	Arguments string
}

type stepKind int

const (
	kindReply stepKind = iota
	kindStatus
	kindRaw
	kindOverflow
	kindStall
	kindDrop
)

// Step is one scripted answer to one request.
type Step struct {
	kind stepKind

	// A reply.
	text      string
	reasoning string
	calls     []Call
	usage     Usage
	cut       bool // the answer ended at the output limit

	// An HTTP answer.
	status      int
	message     string
	contentType string
	header      map[string]string

	// A refusal of a conversation that is too long.
	dialect   Dialect
	window    int
	requested int
}

// Text is a reply of text alone, which ends the model's turn.
func Text(text string) Step { return Step{kind: kindReply, text: text} }

// ToolCalls is a reply that asks for the calls.
func ToolCalls(calls ...Call) Step {
	return Step{kind: kindReply, calls: append([]Call(nil), calls...)}
}

// Cut is a reply of text that stopped at the output limit.
func Cut(text string) Step { return Step{kind: kindReply, text: text, cut: true} }

// CutToolCall is a reply that stopped at the output limit in the middle of a
// tool call's arguments. partialArguments is what arrived of them, which is
// not valid JSON.
func CutToolCall(name, partialArguments string) Step {
	return Step{kind: kindReply, calls: []Call{{Name: name, Arguments: partialArguments}}, cut: true}
}

// WithText puts text before a reply's tool calls.
func (s Step) WithText(text string) Step { s.text = text; return s }

// WithReasoning puts reasoning before a reply's text and calls.
func (s Step) WithReasoning(text string) Step { s.reasoning = text; return s }

// WithUsage sets the token count a reply reports.
func (s Step) WithUsage(u Usage) Step { s.usage = u; return s }

// WithHeader adds a response header to an HTTP answer.
func (s Step) WithHeader(name, value string) Step {
	h := make(map[string]string, len(s.header)+1)
	for k, v := range s.header {
		h[k] = v
	}
	h[name] = value
	s.header = h
	return s
}

// Status is an HTTP error with message in the error body of the API the
// request used.
func Status(code int, message string) Step {
	return Step{kind: kindStatus, status: code, message: message}
}

// RateLimited is a 429 that says when to try again.
func RateLimited(retryAfter time.Duration) Step {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	return Status(http.StatusTooManyRequests, "Rate limit reached. Please try again later.").
		WithHeader("Retry-After", strconv.Itoa(secs))
}

// Raw is an HTTP answer with exactly this status, content type and body.
func Raw(code int, contentType, body string) Step {
	return Step{kind: kindRaw, status: code, contentType: contentType, message: body}
}

// Stall starts a stream, sends text, and then sends nothing more until the
// client goes away.
func Stall(text string) Step { return Step{kind: kindStall, text: text} }

// Drop starts a stream, sends text, and then closes the connection.
func Drop(text string) Step { return Step{kind: kindDrop, text: text} }

// Repeat is step n times.
func Repeat(n int, step Step) []Step {
	out := make([]Step, n)
	for i := range out {
		out[i] = step
	}
	return out
}

// Dialect names a server whose refusal of a conversation that is too long
// the script can play.
type Dialect int

const (
	// OpenAI refuses with a 400 and the code context_length_exceeded.
	OpenAI Dialect = iota + 1
	// VLLM refuses with a 400 in OpenAI's words and no code.
	VLLM
	// LlamaCpp refuses with a 400 of type exceed_context_size_error that
	// states its window as n_ctx.
	LlamaCpp
	// LMStudio answers 200 and sends the refusal as an error event in the
	// stream.
	LMStudio
	// Ollama refuses with a 400 that states by how much, not the window.
	Ollama
	// AnthropicAPI refuses with a 400 invalid_request_error.
	AnthropicAPI
)

// Overflow is the refusal of a conversation that is too long, as dialect
// words it, from a server whose window is window tokens. Each dialect answers
// in its own server's shape whatever API the request used.
func Overflow(dialect Dialect, window int) Step {
	return Step{kind: kindOverflow, dialect: dialect, window: window, requested: window + window/4 + 1}
}

func (s Step) play(x *exchange) {
	for k, v := range s.header {
		x.w.Header().Set(k, v)
	}
	switch s.kind {
	case kindStatus:
		if x.req.API == Anthropic {
			x.json(s.status, anthropicError(s.status, s.message))
		} else {
			x.json(s.status, chatError(s.status, s.message))
		}
	case kindRaw:
		x.w.Header().Set("Content-Type", s.contentType)
		x.w.WriteHeader(s.status)
		_, _ = x.w.Write([]byte(s.message))
	case kindOverflow:
		s.playOverflow(x)
	default:
		if x.req.API == Anthropic {
			s.playAnthropic(x)
		} else {
			s.playChat(x)
		}
	}
}

// The refusals below carry each server's own words. The OpenAI, vLLM and
// llama.cpp bodies and the Anthropic and Ollama messages are the ones those
// servers send. LM Studio's is the wording and the event shape its users
// report; it was not captured from a running LM Studio.
func (s Step) playOverflow(x *exchange) {
	switch s.dialect {
	case OpenAI:
		x.json(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":"This model's maximum context length is %d tokens. However, your messages resulted in %d tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, s.window, s.requested))
	case VLLM:
		x.json(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":"This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, 4096 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","param":null,"code":400}}`, s.window, s.requested, s.requested-4096))
	case LlamaCpp:
		x.json(http.StatusBadRequest, fmt.Sprintf(`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":%d,"n_ctx":%d}}`, s.requested, s.window))
	case LMStudio:
		x.sse()
		x.event("error", fmt.Sprintf(`{"error":{"message":"The number of tokens to keep from the initial prompt is greater than the context length (n_keep: %d >= n_ctx: %d). Try to load the model with a larger context length, or provide a shorter input"}}`, s.requested, s.window))
	case Ollama:
		x.json(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":"prompt too long; exceeded max context length by %d tokens","type":"invalid_request_error","param":null,"code":null}}`, s.requested-s.window))
	case AnthropicAPI:
		x.json(http.StatusBadRequest, fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: %d tokens > %d maximum"}}`, s.requested, s.window))
	default:
		x.json(http.StatusInternalServerError, `{"error":{"message":"scriptedmodel: unknown dialect"}}`)
	}
}
