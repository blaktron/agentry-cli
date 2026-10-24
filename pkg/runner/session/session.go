// Package session is the interactive run API: what a chat surface — the CLI's
// TUI, the desktop app — talks to. It sees a text channel and a read-only file
// drop, both recorded as receipts, and nothing that could widen the plan's
// capabilities: turns are prompts to the harness, inputs land under the staged
// /inputs directory, and the engine ends the session on the operator's budgets
// whatever the surface does.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Kind classifies session events; every kind is a line in events.ndjson.
type Kind string

const (
	// KindUserTurn is a user (or synthetic runner) message sent to the harness.
	KindUserTurn Kind = "user_turn"
	// KindAssistantText is a streamed chunk of assistant text.
	KindAssistantText Kind = "assistant_text"
	// KindAssistantTurn closes a turn: the full assistant text and stop reason.
	KindAssistantTurn Kind = "assistant_turn"
	// KindToolCall is one tool call the harness made through the gateway.
	KindToolCall Kind = "tool_call"
	// KindUsage carries the harness's token usage for a turn (or, for a
	// harness that reports only context tokens, one usage update).
	KindUsage Kind = "usage"
	// KindPermissionGranted records an in-harness permission request the
	// runner allowed — a gateway tool.
	KindPermissionGranted Kind = "permission_granted"
	// KindPermissionDenied records an in-harness permission request the
	// runner refused — a tool outside the gateway's catalog.
	KindPermissionDenied Kind = "permission_denied"
	// KindInputAdded records a file added to /inputs mid-run.
	KindInputAdded Kind = "input_added"
	// KindIntake records the non-secret intake answers the run started with.
	KindIntake Kind = "intake"
	// KindSessionEnd is the last event; Reason says why.
	KindSessionEnd Kind = "session_end"
	// KindError is a harness-reported error.
	KindError Kind = "error"
	// KindOther is anything else the harness said, kept for the record.
	KindOther Kind = "other"
	// KindNotice is the runner's word to the person about the session, which
	// a chat shows them and the harness never sees: a check of a persistent
	// agent's share folder, a change it is restarting with.
	KindNotice Kind = "notice"
)

// Event is one thing that happened in the session.
type Event struct {
	Kind Kind      `json:"kind"`
	Time time.Time `json:"time"`
	// Text is the message text (turns, chunks), the tool name (tool calls),
	// or the error text.
	Text string `json:"text,omitempty"`
	// Synthetic marks a user turn the runner composed (an input notice).
	Synthetic bool `json:"synthetic,omitempty"`
	// Task is, on a user turn a persistent agent's heartbeat sent, the id
	// of the task it was for: the turn is the agent's own, not the
	// person's, and a chat draws it as such.
	Task string `json:"task,omitempty"`
	// StopReason is the harness's reason for ending an assistant turn.
	StopReason string `json:"stopReason,omitempty"`
	// Path and Digest describe an added input (agent-side path).
	Path   string `json:"path,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Reason says why the session ended.
	Reason EndReason `json:"reason,omitempty"`
	// Token usage for usage events.
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// TotalTokens is context tokens from a harness that gives no input/output
	// split (docker-agent's usage_update; the per-update delta).
	TotalTokens int64   `json:"totalTokens,omitempty"`
	CostUSD     float64 `json:"costUsd,omitempty"`
	// Raw is the harness's own message, when there was one.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// EndReason is why a session ended.
type EndReason string

const (
	// EndFinished: the user asked for the final result and got it.
	EndFinished EndReason = "finished"
	// EndQuit: the user ended the session without asking for a result.
	EndQuit EndReason = "quit"
	// EndIdle: no user turn within the idle timeout.
	EndIdle EndReason = "idle_timeout"
	// EndWall: the session's wall-clock cap.
	EndWall EndReason = "wall_timeout"
	// EndUserTurns: the user-turn cap.
	EndUserTurns EndReason = "max_user_turns"
	// EndToolBudget: the tool-call (turn) budget.
	EndToolBudget EndReason = "tool_budget"
	// EndCostBudget: the cost budget.
	EndCostBudget EndReason = "cost_budget"
	// EndPlanLimit: the person's plan refused the model's calls for its
	// usage limit.
	EndPlanLimit EndReason = "plan_limit"
	// EndOrgPolicy: a policy of the person's organization at the model
	// vendor refused the run.
	EndOrgPolicy EndReason = "org_policy"
	// EndOutOfCredits: the model's provider refused the calls because the
	// paying account has no credits left.
	EndOutOfCredits EndReason = "out_of_credits"
	// EndHarness: the harness exited on its own.
	EndHarness EndReason = "harness_exit"
	// EndCancelled: the caller's context ended.
	EndCancelled EndReason = "cancelled"
)

// ErrEnded is returned by Send and AddInput once the session has ended.
var ErrEnded = errors.New("session has ended")

// ErrBusy is a Send refused because another turn is in flight: a session
// that more than one driver attaches to answers it rather than queue the
// turn.
var ErrBusy = errors.New("a turn is in flight; send again when its answer arrives")

// Session is one interactive run.
type Session interface {
	// Events streams everything that happens, in order; closed after End.
	Events() <-chan Event
	// Send delivers one user turn and returns when the harness ends it
	// (the assistant_turn event). One turn at a time.
	Send(ctx context.Context, text string) error
	// AddInput copies a regular host file into the run's read-only inputs
	// and tells the agent.
	AddInput(ctx context.Context, hostPath string) error
	// End finishes the session: with finish, the agent is asked for its
	// final result first (only when the release declares an output
	// schema). Receipts and the envelope are written, the sandbox torn
	// down. Safe to call after a budget ended the session.
	End(ctx context.Context, finish bool) (*receipt.Envelope, error)
	// Ended reports whether the session is over and why.
	Ended() (bool, EndReason)
}
