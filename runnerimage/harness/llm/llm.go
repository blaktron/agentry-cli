// Package llm is the harness's view of a model: one request in, a stream of
// pieces and one whole reply out, whatever wire API carries them.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Role is who a message is from.
type Role string

// Roles.
const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Kind is what a part of a message is.
type Kind string

// Kinds of part.
const (
	// Text is text: the person's, or the model's answer.
	Text Kind = "text"
	// Reasoning is the model's reasoning. Meta holds what its server needs
	// to take it back, and it is sent back unchanged.
	Reasoning Kind = "reasoning"
	// Call is a tool call the model made.
	Call Kind = "call"
	// Result is a tool call's result.
	Result Kind = "result"
)

// Part is one piece of a message.
type Part struct {
	Kind Kind   `json:"kind"`
	Text string `json:"text,omitempty"`

	// CallID names a call, and the call a result answers. Name is the
	// tool's name and Arguments its arguments as the model wrote them.
	// Arguments is text, not parsed JSON: a reply cut at the output limit
	// leaves a call's arguments unfinished.
	CallID    string `json:"callId,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// Failed marks a result that is an error.
	Failed bool `json:"failed,omitempty"`
	// Images are the images a result carries.
	Images []Image `json:"images,omitempty"`

	// Meta is the server's own data for this part (a reasoning block's
	// signature, for one). Only the client that produced it reads it.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// Image is an image in a tool result, base64-encoded.
type Image struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

// Message is one message of a conversation.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// Tool is a tool the model may call.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema of the tool's arguments.
	Schema json.RawMessage
}

// Request is one model request.
type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Finish is why a reply ended.
type Finish string

// Why a reply ended.
const (
	// FinishStop: the model finished its answer.
	FinishStop Finish = "stop"
	// FinishToolCalls: the model asked for its calls to be run.
	FinishToolCalls Finish = "tool_calls"
	// FinishLength: the reply stopped at the output limit.
	FinishLength Finish = "length"
	// FinishOther: the server ended the reply for a reason of its own.
	FinishOther Finish = "other"
)

// Usage is the token count of one request. Input, CacheRead and CacheWrite
// do not overlap.
type Usage struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
}

// Response is a whole reply.
type Response struct {
	// Parts is the reply in order: reasoning, text and calls.
	Parts  []Part
	Finish Finish
	Usage  Usage
}

// Delta is a piece of a reply as it streams: Text or Reasoning.
type Delta struct {
	Kind Kind
	Text string
}

// Client reaches one model.
type Client interface {
	// Stream sends a request and calls onDelta for each piece of text or
	// reasoning as it arrives. It returns the whole reply, or an *Error.
	// A reply cut off by a failure is not returned.
	Stream(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error)
}

// Class is the kind of failure a model request met.
type Class string

// Classes of failure. Their names are the harness protocol's error classes.
const (
	Overflow   Class = "overflow"
	Capacity   Class = "capacity"
	RateLimit  Class = "rate_limit"
	Credits    Class = "credits"
	Auth       Class = "auth"
	BadRequest Class = "request"
	Transport  Class = "transport"
	Server     Class = "server"
)

// Error is a failed model request.
type Error struct {
	Class Class
	// Status is the HTTP status, zero when the failure had none.
	Status int
	// Message is the server's own words, or the transport's.
	Message string
	// RetryAfter is the wait the server asked for, zero when it gave none.
	RetryAfter time.Duration
	// Window is the model's window in tokens, when the server stated it in
	// refusing a conversation as too long; zero when it stated none.
	Window int64
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("model request failed (%s, HTTP %d): %s", e.Class, e.Status, e.Message)
	}
	return fmt.Sprintf("model request failed (%s): %s", e.Class, e.Message)
}
