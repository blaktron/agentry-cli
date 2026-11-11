// Package proto is the contract between the runner and its own harness: the
// configuration the runner renders for a run, and the events the harness
// writes back, one JSON object a line.
//
// Both sides compile this package, so a field one side writes is a field
// the other reads. Version moves when a field changes meaning or goes away.
package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Version is the contract's version. A harness refuses a configuration of
// another version, and stamps its own on every event.
const Version = 1

// Config is everything the harness is told about a run. It names where
// secrets are found and never holds one.
type Config struct {
	Version int   `json:"version"`
	Model   Model `json:"model"`
	// System is the system prompt.
	System  string  `json:"system"`
	Gateway Gateway `json:"gateway"`
	// SessionDir is the folder that holds one log file a conversation.
	SessionDir string `json:"sessionDir"`
	// MaxSteps ends a turn after this many model requests. Zero is no
	// limit of the harness's own; the runner's budgets still apply.
	MaxSteps int `json:"maxSteps,omitempty"`
}

// Wire APIs a model is reached by.
const (
	// APIAnthropic is the Anthropic Messages API.
	APIAnthropic = "anthropic"
	// APIChat is OpenAI-style Chat Completions, as OpenAI-compatible
	// servers speak it.
	APIChat = "chat-completions"
	// APIResponses is the OpenAI Responses API.
	APIResponses = "openai-responses"
	// APIGemini is the Gemini API's generateContent.
	APIGemini = "gemini"
)

// Model says how the model is reached.
type Model struct {
	// API is the wire API, one of the API constants.
	API string `json:"api"`
	// Name is the model's id as its server knows it.
	Name string `json:"name"`
	// BaseURL is the server's address with no trailing slash. Empty is
	// the API's own.
	BaseURL string `json:"baseUrl,omitempty"`
	// KeyEnv names the environment variable that holds the key. Empty is
	// a server that takes none, which only Chat Completions allows.
	KeyEnv string `json:"keyEnv,omitempty"`
	// MaxOutputTokens is the most output asked of one request. Zero leaves
	// the limit to the server, which the Anthropic API does not allow.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// ContextWindow is the model's window in tokens, zero when unknown.
	ContextWindow int `json:"contextWindow,omitempty"`
	// Sampling is sent with every request; a field left out is the
	// server's own default. Chat Completions only.
	Sampling *Sampling `json:"sampling,omitempty"`
	// ExtraBody is a JSON object whose members are added to every
	// request's body as they are (a server's own switches, such as its
	// chat template's arguments). Chat Completions only.
	ExtraBody json.RawMessage `json:"extraBody,omitempty"`
	// ReplayReasoning sends the model's earlier reasoning back with each
	// assistant message, for a model whose template expects it. Chat
	// Completions only; the Anthropic API always takes its own back.
	ReplayReasoning bool `json:"replayReasoning,omitempty"`
	// Prices is what the model's provider charges, when the runner knows.
	// With it the harness reports what the conversation has cost.
	Prices *Prices `json:"prices,omitempty"`
}

// Prices is a model's prices in US dollars a million tokens.
type Prices struct {
	// Input is input the server reads fresh, CacheRead input it reads
	// from its cache, CacheWrite input it writes to it.
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// Output is output, reasoning included.
	Output float64 `json:"output"`
}

// Sampling is the sampling a model is asked to use.
type Sampling struct {
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"topP,omitempty"`
	TopK              *int64   `json:"topK,omitempty"`
	MinP              *float64 `json:"minP,omitempty"`
	PresencePenalty   *float64 `json:"presencePenalty,omitempty"`
	RepetitionPenalty *float64 `json:"repetitionPenalty,omitempty"`
}

// Gateway is the one source of tools: an MCP server over streamable HTTP.
type Gateway struct {
	URL string `json:"url"`
	// TokenEnv names the environment variable that holds the bearer.
	TokenEnv string `json:"tokenEnv"`
}

// DecodeConfig reads a configuration strictly: an unknown field, a second
// document or another version is an error.
func DecodeConfig(r io.Reader) (Config, error) {
	var c Config
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("harness configuration: %w", err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("harness configuration: more than one document")
	}
	if c.Version != Version {
		return Config{}, fmt.Errorf("harness configuration: version %d, this harness reads version %d", c.Version, Version)
	}
	switch c.Model.API {
	case APIAnthropic, APIResponses, APIGemini:
		switch {
		case c.Model.KeyEnv == "":
			return Config{}, fmt.Errorf("harness configuration: model.keyEnv is empty")
		case c.Model.Sampling != nil || len(c.Model.ExtraBody) > 0 || c.Model.ReplayReasoning:
			return Config{}, fmt.Errorf("harness configuration: model.sampling, model.extraBody and model.replayReasoning are for model.api %q", APIChat)
		}
	case APIChat:
		if c.Model.BaseURL == "" {
			return Config{}, fmt.Errorf("harness configuration: model.baseUrl is empty, and model.api %q has no address of its own", APIChat)
		}
		if len(c.Model.ExtraBody) > 0 {
			var obj map[string]json.RawMessage
			if json.Unmarshal(c.Model.ExtraBody, &obj) != nil || obj == nil {
				return Config{}, fmt.Errorf("harness configuration: model.extraBody is not a JSON object")
			}
		}
	default:
		return Config{}, fmt.Errorf("harness configuration: model.api %q is not one this harness speaks", c.Model.API)
	}
	switch {
	case c.Model.Prices != nil && (c.Model.Prices.Input < 0 || c.Model.Prices.CacheRead < 0 || c.Model.Prices.CacheWrite < 0 || c.Model.Prices.Output < 0):
		return Config{}, fmt.Errorf("harness configuration: model.prices holds a negative price")
	case c.Model.Name == "":
		return Config{}, fmt.Errorf("harness configuration: model.name is empty")
	case c.Model.MaxOutputTokens < 0 || c.Model.MaxOutputTokens == 0 && c.Model.API == APIAnthropic:
		return Config{}, fmt.Errorf("harness configuration: model.maxOutputTokens must be positive")
	case c.Gateway.URL == "":
		return Config{}, fmt.Errorf("harness configuration: gateway.url is empty")
	case c.Gateway.TokenEnv == "":
		return Config{}, fmt.Errorf("harness configuration: gateway.tokenEnv is empty")
	case c.SessionDir == "":
		return Config{}, fmt.Errorf("harness configuration: sessionDir is empty")
	}
	return c, nil
}

// Encode renders a configuration as the bytes the harness reads. The same
// configuration always renders to the same bytes.
func (c Config) Encode() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Event types.
const (
	// TypeReady is the first event: the harness has read its
	// configuration and the gateway's tools.
	TypeReady = "ready"
	// TypeText is a piece of the model's answer.
	TypeText = "text"
	// TypeReasoning is a piece of the model's reasoning.
	TypeReasoning = "reasoning"
	// TypeToolCall says the harness is about to run a call.
	TypeToolCall = "tool_call"
	// TypeToolResult is a call's result.
	TypeToolResult = "tool_result"
	// TypeUsage is one model request's token count.
	TypeUsage = "usage"
	// TypeWarning is something the harness did that the runner counts.
	TypeWarning = "warning"
	// TypeError is a failure, with its class.
	TypeError = "error"
	// TypeTurnEnd closes a prompt. Every prompt gets exactly one.
	TypeTurnEnd = "turn_end"
)

// Why a turn ended.
const (
	// EndAnswered: the model answered with no call left to run.
	EndAnswered = "answered"
	// EndAnswerCut: the model's answer stopped at the output limit.
	EndAnswerCut = "answer_cut"
	// EndError: a failure ended the turn; an error event precedes it.
	EndError = "error"
	// EndMaxSteps: the configuration's step limit was reached.
	EndMaxSteps = "max_steps"
)

// Classes of error.
const (
	// ClassOverflow: the server refused the conversation as too long.
	ClassOverflow = "overflow"
	// ClassCapacity: the server is overloaded or unavailable.
	ClassCapacity = "capacity"
	// ClassRateLimit: the server asked the client to slow down.
	ClassRateLimit = "rate_limit"
	// ClassCredits: the account that pays has nothing left.
	ClassCredits = "credits"
	// ClassAuth: the server refused the key.
	ClassAuth = "auth"
	// ClassRequest: the server refused the request as malformed.
	ClassRequest = "request"
	// ClassTransport: the connection failed or the stream broke.
	ClassTransport = "transport"
	// ClassServer: any other failure of the model's server.
	ClassServer = "server"
	// ClassGateway: the tool gateway could not be reached or refused.
	ClassGateway = "gateway"
	// ClassHarness: a fault of the harness's own.
	ClassHarness = "harness"
)

// Event is one line the harness writes. Type says which fields are set.
type Event struct {
	// V is the contract's version.
	V int `json:"v"`
	// Seq numbers the events of one process from 1, with no gap.
	Seq int64 `json:"seq"`
	// Type is one of the Type constants.
	Type string `json:"type"`

	// Text is a piece of the answer (text) or of the reasoning
	// (reasoning), a tool's output (tool_result), the words of a warning
	// or an error, or the whole final answer (turn_end).
	Text string `json:"text,omitempty"`

	// CallID, Tool and Arguments describe a call (tool_call); CallID and
	// Tool name the call a result answers (tool_result).
	CallID    string          `json:"callId,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Failed marks a result whose tool reported an error, or that the
	// harness wrote because the call was not run.
	Failed bool `json:"failed,omitempty"`
	// Unchanged marks a result that held content passed to the model as
	// the tool sent it and left out of Text.
	Unchanged bool `json:"unchanged,omitempty"`

	// Usage is set on a usage event.
	Usage *Usage `json:"usage,omitempty"`

	// Code names a warning.
	Code string `json:"code,omitempty"`
	// Class is an error's class, one of the Class constants. Status is
	// the HTTP status the server answered with, zero when there was none.
	// RetryAfterSeconds is the wait the server asked for.
	Class             string `json:"class,omitempty"`
	Status            int    `json:"status,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`

	// Reason says why a turn ended, one of the End constants.
	Reason string `json:"reason,omitempty"`
	// Conversation is the id of the conversation (ready, turn_end).
	Conversation string `json:"conversation,omitempty"`
	// Tools is the names of the tools the model is offered (ready).
	Tools []string `json:"tools,omitempty"`
}

// Usage is the token count of one model request.
type Usage struct {
	// Input is the input the server read fresh, CacheRead what it read
	// from its cache, CacheWrite what it wrote to it. The three do not
	// overlap; their sum is the size of the request.
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	// Output is the output, reasoning included.
	Output int64 `json:"output"`
	// ContextTokens is the size of the conversation as of this request:
	// its input, cached or not. ContextLimit is the model's window, zero
	// when the configuration gave none.
	ContextTokens int64 `json:"contextTokens"`
	ContextLimit  int64 `json:"contextLimit,omitempty"`
	// CostUSD is what the whole conversation has cost so far, this request
	// included, at the configuration's prices; absent when it gave none.
	// It is a running total of the conversation, not of the process: a
	// harness started again on a conversation goes on from what the
	// earlier requests cost.
	CostUSD float64 `json:"costUsd,omitempty"`
}
