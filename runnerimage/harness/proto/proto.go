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
	"strings"
)

// Version is the contract's version. A harness refuses a configuration of
// another version, and stamps its own on every event.
const Version = 2

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

	// Each block below switches on one thing the harness does on a long
	// run. A block left out is a thing it does not do.

	// Results bounds what one tool result may hold.
	Results *Results `json:"results,omitempty"`
	// Scrub removes secret-shaped values from tool arguments and results.
	Scrub *Scrub `json:"scrub,omitempty"`
	// Loop answers a repeated call with a message to the model.
	Loop *Loop `json:"loop,omitempty"`
	// LengthRecoveries is how many replies cut off at the output limit one
	// turn goes on from. Zero ends the turn at the first.
	LengthRecoveries int `json:"lengthRecoveries,omitempty"`
	// ColdClear replaces old tool results once the server's cache of the
	// conversation has expired.
	ColdClear *ColdClear `json:"coldClear,omitempty"`
	// Compaction replaces a conversation that nears the model's window, or
	// that the server refused as too long, with a summary of it.
	Compaction *Compaction `json:"compaction,omitempty"`
	// Retry sends a model request again after a failure that may pass.
	Retry *Retry `json:"retry,omitempty"`
}

// Results is the result policy.
type Results struct {
	// MaxBytes is the most text one result may hold. A longer one keeps its
	// start, cut at a line where it can be, and ends with a notice that says
	// how much was left out.
	MaxBytes int `json:"maxBytes"`
}

// Scrub is the secret scrub. Every call's arguments are scrubbed before the
// tool is given them, and every tool's result before the model is.
type Scrub struct {
	// Unchanged names the tools whose results are passed on as they are:
	// the ones that return the person's own inputs.
	Unchanged []string `json:"unchanged,omitempty"`
}

// Loop is when a repeated call is answered with a message.
type Loop struct {
	// Repeats is how many times in a row the same call, with the same
	// arguments, is made before the message.
	Repeats int `json:"repeats"`
	// CycleRepeats is how many times in a row the same sequence of two to
	// MaxPeriod calls is made before the message. Zero looks for no
	// sequence.
	CycleRepeats int `json:"cycleRepeats,omitempty"`
	MaxPeriod    int `json:"maxPeriod,omitempty"`
}

// ColdClear is the clearing of old tool results. It is done on a turn's
// first request, and only when the last reply is older than AfterSeconds:
// the server then reads the whole conversation afresh whatever it holds.
type ColdClear struct {
	// AfterSeconds is how long the server's cache of a conversation lives.
	AfterSeconds int `json:"afterSeconds"`
	// KeepTurns is how many turns before the current one keep their
	// results, and KeepBrowser how many of the newest results of
	// BrowserTools stay wherever they fall.
	KeepTurns    int      `json:"keepTurns"`
	KeepBrowser  int      `json:"keepBrowser"`
	BrowserTools []string `json:"browserTools,omitempty"`
	// MinBytes is the smallest result worth clearing: a shorter one stays
	// where it is, since its placeholder would save next to nothing and the
	// replies written with it would be left with no sign of what they rest
	// on. A result of BrowserTools goes whatever its size, because it is
	// out of date; so does one that carries an image.
	MinBytes int `json:"minBytes,omitempty"`
}

// Compaction is when and how a conversation is replaced by a summary.
type Compaction struct {
	// Threshold is the share of the model's window at which the
	// conversation is summarised, above 0 and below 1. With no window in
	// the configuration, the one a server's refusal states, or the size it
	// refused, is used once there has been a refusal.
	Threshold float64 `json:"threshold"`
	// Checkpoint is what the summary must keep, in the runner's words.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Notes names where the agent keeps its notes, as the summary's last
	// sentence says it to the agent. One line.
	Notes string `json:"notes,omitempty"`
}

// Retry is the retry policy of a model request.
type Retry struct {
	// Attempts is how many times a failed request is sent again.
	Attempts int `json:"attempts"`
	// BaseSeconds is the wait before the first of them; each later wait is
	// twice the one before, and none is longer than MaxSeconds. A wait the
	// server asks for is kept instead when it is no longer than MaxSeconds;
	// a longer one ends the retries.
	BaseSeconds int `json:"baseSeconds"`
	MaxSeconds  int `json:"maxSeconds"`
	// IdleSeconds ends a request whose reply has sent nothing for this
	// long, as a failure of the connection. Zero sets no limit.
	IdleSeconds int `json:"idleSeconds,omitempty"`
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
	case c.MaxSteps < 0 || c.LengthRecoveries < 0:
		return Config{}, fmt.Errorf("harness configuration: maxSteps and lengthRecoveries must not be negative")
	case c.Results != nil && c.Results.MaxBytes < MinResultBytes:
		return Config{}, fmt.Errorf("harness configuration: results.maxBytes must be at least %d", MinResultBytes)
	case c.Loop != nil && (c.Loop.Repeats < 2 || c.Loop.CycleRepeats < 0 || c.Loop.CycleRepeats == 1 || c.Loop.MaxPeriod < 0 || c.Loop.CycleRepeats > 0 && c.Loop.MaxPeriod < 2):
		return Config{}, fmt.Errorf("harness configuration: loop.repeats and loop.cycleRepeats count at least 2 repeats, and loop.maxPeriod at least 2 calls")
	case c.ColdClear != nil && (c.ColdClear.AfterSeconds <= 0 || c.ColdClear.KeepTurns < 0 || c.ColdClear.KeepBrowser < 0 || c.ColdClear.MinBytes < 0):
		return Config{}, fmt.Errorf("harness configuration: coldClear.afterSeconds must be positive, and keepTurns, keepBrowser and minBytes not negative")
	case c.Compaction != nil && (c.Compaction.Threshold <= 0 || c.Compaction.Threshold >= 1):
		return Config{}, fmt.Errorf("harness configuration: compaction.threshold must be above 0 and below 1")
	case c.Compaction != nil && strings.ContainsAny(c.Compaction.Notes, "\r\n"):
		return Config{}, fmt.Errorf("harness configuration: compaction.notes must be one line")
	case c.Retry != nil && (c.Retry.Attempts < 0 || c.Retry.BaseSeconds < 0 || c.Retry.MaxSeconds < c.Retry.BaseSeconds || c.Retry.IdleSeconds < 0):
		return Config{}, fmt.Errorf("harness configuration: retry holds a negative figure, or maxSeconds below baseSeconds")
	}
	return c, nil
}

// MinResultBytes is the least results.maxBytes may be: room for the notice
// and a useful start of the result.
const MinResultBytes = 1024

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
	// TypeTurnEnding says the model answered with no call left to run, and
	// asks the runner whether the turn ends there. The harness waits for
	// the runner's accept or continue command.
	TypeTurnEnding = "turn_ending"
	// TypeTurnEnd closes a prompt. Every prompt gets exactly one.
	TypeTurnEnd = "turn_end"
	// TypePermission asks the runner whether a call may run, in a session.
	// CallID and Tool name the call. The harness waits for the runner's
	// permit or deny command.
	TypePermission = "permission"
	// TypeSteered says messages the runner sent while a turn was under way,
	// or between turns, are now part of the conversation. Count is how many.
	TypeSteered = "steered"
)

// Codes of warning.
const (
	// WarnLoopNudge: the model repeated a call and was told to reach its
	// goal another way. Tool names the call, Count how often it repeated.
	WarnLoopNudge = "loop_nudge"
	// WarnLengthRecovery: a reply cut off at the output limit was taken out
	// of what the model is sent, and the model was told. Reason is what was
	// cut, one of the Cut constants.
	WarnLengthRecovery = "length_recovery"
	// WarnColdClear: old tool results were replaced by placeholders. Count
	// is how many.
	WarnColdClear = "cold_clear"
	// WarnCompaction: the conversation was replaced by a summary. Reason is
	// why, one of the Compaction constants.
	WarnCompaction = "compaction"
	// WarnCompactionFailed: a summary could not be made, and the
	// conversation was left as it was. Reason is why one was wanted.
	WarnCompactionFailed = "compaction_failed"
	// WarnRetry: a model request failed and is sent again. Class is the
	// failure's, Count the attempt.
	WarnRetry = "retry"
	// WarnCommandRefused: a command of the runner's could not be taken as
	// things stood (a prompt while a turn is under way, an answer to nothing
	// asked). Reason is the command's type. The session goes on.
	WarnCommandRefused = "command_refused"
)

// What the output limit cut off (a length_recovery warning's reason).
const (
	CutAnswer    = "answer"
	CutReasoning = "reasoning"
	CutToolCall  = "tool call"
)

// Why a conversation was summarised (a compaction warning's reason).
const (
	// CompactionThreshold: it reached the configured share of the window.
	CompactionThreshold = "threshold"
	// CompactionOverflow: the server refused it as too long.
	CompactionOverflow = "overflow"
	// CompactionRequest: the runner asked for it with a compact command.
	CompactionRequest = "request"
)

// Commands the runner writes to the harness, one JSON object a line.
const (
	// CmdPrompt starts a turn with Text as the runner's message.
	CmdPrompt = "prompt"
	// CmdAccept answers turn_ending: the turn ends.
	CmdAccept = "accept"
	// CmdContinue answers turn_ending: the turn goes on, with Text as a
	// message to the model.
	CmdContinue = "continue"

	// The commands below are a session's: the serve mode reads them, and
	// the one-shot mode takes none of them.

	// CmdOpen opens the conversation named Conversation, creating it when
	// there is none, and is answered with a ready event or an error. It is
	// taken between turns only. A session's first command is an open.
	CmdOpen = "open"
	// CmdSteer adds Text to the conversation as the runner's message while
	// a turn is under way: after the results of the calls being run, or
	// when the model next answers with no call. Between turns it is kept
	// and goes ahead of the next prompt.
	CmdSteer = "steer"
	// CmdInterrupt stops the turn under way. Every call left open is given
	// a result that says so, and the turn ends as interrupted. With no turn
	// under way it does nothing.
	CmdInterrupt = "interrupt"
	// CmdPermit and CmdDeny answer a permission event for the call CallID
	// names. A denied call is not run: the denial is its result, and the
	// turn goes on.
	CmdPermit = "permit"
	CmdDeny   = "deny"
	// CmdCompact replaces the open conversation with a summary of it,
	// between turns. It is answered with a compaction warning whose reason
	// is request, or, when no summary was made and the conversation is as
	// it was, with a compaction_failed warning of that reason. An interrupt
	// gives it up.
	CmdCompact = "compact"
)

// Command is one line the runner writes.
type Command struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Conversation is the id of the conversation to open (open).
	Conversation string `json:"conversation,omitempty"`
	// CallID names the call a permit or a deny answers.
	CallID string `json:"callId,omitempty"`
}

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
	// EndInterrupted: the runner stopped the turn. Text is what the model
	// had written of the reply it was stopped in.
	EndInterrupted = "interrupted"
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
	// Tool name the call a result answers (tool_result) or the call the
	// runner is asked about (permission).
	CallID    string          `json:"callId,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Failed marks a result whose tool reported an error, or that the
	// harness wrote because the call was not run.
	Failed bool `json:"failed,omitempty"`
	// Unchanged marks a result that held content passed to the model as
	// the tool sent it and left out of Text.
	Unchanged bool `json:"unchanged,omitempty"`

	// Usage is set on a usage event. On the ready event of a conversation
	// that already held a message it says where the conversation stands:
	// its size, the model's window and what it has cost.
	Usage *Usage `json:"usage,omitempty"`

	// Code names a warning, one of the Warn constants. Count is the figure
	// the code's comment gives it, or how many messages a steered event
	// counts.
	Code  string `json:"code,omitempty"`
	Count int    `json:"count,omitempty"`
	// Class is an error's class, one of the Class constants. Status is
	// the HTTP status the server answered with, zero when there was none.
	// RetryAfterSeconds is the wait the server asked for.
	Class             string `json:"class,omitempty"`
	Status            int    `json:"status,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`

	// Reason says why a turn ended, one of the End constants, or what a
	// warning's code says it is.
	Reason string `json:"reason,omitempty"`
	// Conversation is the id of the conversation (ready, turn_end).
	Conversation string `json:"conversation,omitempty"`
	// Continued says the conversation already held a message when it was
	// opened (ready).
	Continued bool `json:"continued,omitempty"`
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
	// Compacted marks the request that wrote a summary the conversation was
	// then replaced by. ContextTokens is then an estimate of the
	// conversation's size after it.
	Compacted bool `json:"compacted,omitempty"`
	// CostUSD is what the whole conversation has cost so far, this request
	// included, at the configuration's prices; absent when it gave none.
	// It is a running total of the conversation, not of the process: a
	// harness started again on a conversation goes on from what the
	// earlier requests cost.
	CostUSD float64 `json:"costUsd,omitempty"`
}
