package persistent

// Upkeep: every persistent agent compacts its own conversation. When the
// person has sent nothing for a while and the conversation is large, or when
// a turn has filled most of the model's window, or when the person asks, the
// supervisor sends the agent an upkeep turn: an agent that keeps a memory
// saves what the conversation produced to it first, and every agent answers
// with a handoff. The supervisor then starts a new conversation that opens
// with the handoff, quoted. A failed upkeep turn swaps nothing, and the
// conversation goes on as it was. Each compaction is a line of the segment's
// receipts/upkeep.ndjson, with the memory's fact lines counted before and
// after, where there is a memory, so a line the model lost shows.

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// The rules a compaction runs by.
const (
	// upkeepIdle is how long the person has sent nothing before an idle
	// conversation is compacted.
	upkeepIdle = 2 * time.Hour
	// upkeepIdleFloor is the smallest conversation an idle compaction
	// takes: below it the conversation goes on natively.
	upkeepIdleFloor = 100_000
	// upkeepInUseShare is the share of the model's window a turn fills
	// before the conversation is compacted while in use.
	upkeepInUseShare = 0.75
	// maxUpkeepsPerDay bounds the upkeep turns in any 24 hours.
	maxUpkeepsPerDay = 12
	// upkeepTurnCap bounds an upkeep turn; a person's message waits for it
	// at most this long.
	upkeepTurnCap = 5 * time.Minute
	// maxHandoff bounds the handoff a new conversation opens with.
	maxHandoff = 4096
	// upkeepRetry is how long an idle or in-use compaction that failed
	// waits before it is tried again, so a failing one does not spend the
	// day's upkeep turns a minute apart.
	upkeepRetry = time.Hour
)

// The triggers a compaction records, and a consolidation's.
const (
	UpkeepIdle   = "idle"
	UpkeepInUse  = "in-use"
	UpkeepManual = "manual"
	// UpkeepCold compacts before a person's message that comes after the
	// harness's prompt cache has expired, when the history is re-sent
	// uncached anyway.
	UpkeepCold = "cold"
	// UpkeepConsolidate is the daily tidy of the memory, which swaps
	// nothing.
	UpkeepConsolidate = "consolidate"
)

// consolidateEvery is how long a consolidation waits after the last.
const consolidateEvery = 24 * time.Hour

// keptForCompaction is how many of a day's upkeep turns a consolidation
// leaves to compactions, so a failing tidy cannot spend them.
const keptForCompaction = 4

// beginUpkeep marks upkeep as running, so a person's message waits for it
// and is told why, until the returned func ends it.
func (s *Supervisor) beginUpkeep(wait string) func() {
	s.mu.Lock()
	s.upkeeping++
	outer := s.upkeepWait
	s.upkeepWait = wait
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.upkeeping--
		s.upkeepWait = outer
		s.mu.Unlock()
	}
}

// upkeepSent settles an upkeep turn's mark: the pump takes it from the
// turn's own event, which may reach it after the turn returned, and a turn
// that failed may have sent none, so its mark is dropped.
func (s *Supervisor) upkeepSent(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.upkeepTurn = false
	s.mu.Unlock()
}

// upkeepFile, in the agent's state folder, keeps what upkeep decides by
// across segments and restarts.
const upkeepFile = "upkeep.json"

// upkeepLog is the segment's record of its compactions.
const upkeepLog = "upkeep.ndjson"

// upkeepState is upkeep.json.
type upkeepState struct {
	Version int `json:"version"`
	// LastPersonTurn is when the person last sent a turn; a task's or an
	// upkeep turn is not theirs.
	LastPersonTurn time.Time `json:"lastPersonTurn,omitzero"`
	// LastResponse is when the agent last answered a turn of any kind: the
	// harness's prompt cache is warm until its lifetime after it.
	LastResponse time.Time `json:"lastResponse,omitzero"`
	// ContextTokens and ContextLimit are the conversation's size after its
	// latest turn and the model's window, as the harness said them.
	ContextTokens int64 `json:"contextTokens,omitempty"`
	ContextLimit  int64 `json:"contextLimit,omitempty"`
	// TurnsSince counts the turns of any kind since the conversation began
	// or was last compacted: an idle conversation with none is left alone.
	TurnsSince int `json:"turnsSince,omitempty"`
	// LastCompaction is when a compaction last swapped the conversation.
	LastCompaction time.Time `json:"lastCompaction,omitzero"`
	// Upkeeps are the upkeep turns sent, for the daily cap.
	Upkeeps []time.Time `json:"upkeeps,omitempty"`
	// RetryAfter holds an idle or in-use compaction back after one failed.
	RetryAfter time.Time `json:"retryAfter,omitzero"`
	// Handoff is the handoff a compaction's new conversation opens with,
	// kept until a turn has carried it to the agent.
	Handoff string `json:"handoff,omitempty"`
	// LastConsolidation is when the memory was last consolidated.
	LastConsolidation time.Time `json:"lastConsolidation,omitzero"`
	// MemoryDigest is the memory's digest after the last consolidation.
	MemoryDigest string `json:"memoryDigest,omitempty"`
	// MemoryWrites counts the turns, the agent's or an upkeep's, that may
	// have written the memory since: with none, nothing is looked at.
	MemoryWrites int `json:"memoryWrites,omitempty"`
	// ConsolidateAfter holds a consolidation back after one could not run.
	ConsolidateAfter time.Time `json:"consolidateAfter,omitzero"`
}

func loadUpkeep(home, name string) upkeepState {
	var u upkeepState
	b, err := os.ReadFile(filepath.Join(Dir(home, name), upkeepFile)) //nolint:gosec // the runner home's own file
	if err == nil {
		_ = json.Unmarshal(b, &u)
	}
	u.Version = 1
	return u
}

// saveUpkeep writes upkeep.json through one fixed temporary file, so two
// writes must never run at once: a supervisor writes only through
// changeUpkeep.
func saveUpkeep(home, name string, u upkeepState) error {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(Dir(home, name), upkeepFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// upkeepsToday is how many upkeep turns were sent in the 24 hours before now.
func (u upkeepState) upkeepsToday(now time.Time) int {
	n := 0
	for _, t := range u.Upkeeps {
		if now.Sub(t) < 24*time.Hour {
			n++
		}
	}
	return n
}

// inUseDue says the conversation's latest turn filled the share of the
// model's window an in-use compaction waits for.
func (u upkeepState) inUseDue(now time.Time) bool {
	return !now.Before(u.RetryAfter) && u.ContextLimit > 0 && float64(u.ContextTokens) >= upkeepInUseShare*float64(u.ContextLimit)
}

// idleDue says an idle compaction is due at now.
func (u upkeepState) idleDue(now time.Time) bool {
	return !now.Before(u.RetryAfter) && u.TurnsSince > 0 && !u.LastPersonTurn.IsZero() && now.Sub(u.LastPersonTurn) >= upkeepIdle &&
		u.ContextTokens >= upkeepIdleFloor
}

// coldDue says a cold-cache compaction is due at now, for a harness whose
// prompt cache lives after: the last answer is older than that, the
// conversation is at least the floor, and a turn has passed since it was last
// compacted.
func (u upkeepState) coldDue(now time.Time, after time.Duration) bool {
	return !now.Before(u.RetryAfter) && u.TurnsSince > 0 && !u.LastResponse.IsZero() && now.Sub(u.LastResponse) > after &&
		u.ContextTokens >= adapter.ColdCompactionFloor
}

// consolidationDue says the memory's daily consolidation is due at now: the
// person has sent nothing for upkeepIdle, a day has passed since the last,
// and a turn since may have changed the memory.
func (u upkeepState) consolidationDue(now time.Time) bool {
	return u.MemoryWrites > 0 && !now.Before(u.ConsolidateAfter) && !u.LastPersonTurn.IsZero() &&
		now.Sub(u.LastPersonTurn) >= upkeepIdle && (u.LastConsolidation.IsZero() || now.Sub(u.LastConsolidation) >= consolidateEvery)
}

// noteUpkeepEvent keeps what a segment's event tells upkeep. The caller
// holds mu, and a user turn carries the marks it was sent with.
func (s *Supervisor) noteUpkeepEvent(ev session.Event) {
	switch ev.Kind {
	case session.KindUserTurn:
		if ev.Synthetic {
			return
		}
		if ev.Task == "" && !ev.Upkeep && ev.Time.After(s.upkeep.LastPersonTurn) {
			s.upkeep.LastPersonTurn = ev.Time
		}
		if !ev.Upkeep {
			s.upkeep.TurnsSince++
			if s.keepsMemory() {
				s.upkeep.MemoryWrites++
			}
		}
		s.upkeepDirty = true
	case session.KindUsage:
		if ev.ContextTokens > 0 || ev.ContextCompacted {
			// After the harness's own compaction the size it reports is
			// the new one, and an unknown one is taken as zero: the size
			// before it would compact a conversation already made small.
			s.upkeep.ContextTokens = ev.ContextTokens
			if ev.ContextLimit > 0 {
				s.upkeep.ContextLimit = ev.ContextLimit
			}
			s.upkeepDirty = true
		}
	case session.KindAssistantTurn:
		// The answer is when the harness's prompt cache was last warm, and
		// is saved with whatever else changed since the last save: off the
		// events' lock, which every watcher waits on.
		s.upkeep.LastResponse, s.upkeepDirty = s.now().UTC(), false
		go func() { _ = s.changeUpkeep(nil) }()
	}
}

// upkeepTick, from the heartbeat's ticker, compacts the conversation when an
// idle or an in-use compaction is due, then consolidates the memory when
// that is due: after an idle compaction, its turn comes first.
func (s *Supervisor) upkeepTick(ctx context.Context) {
	trigger := s.upkeepDue()
	if trigger == "" && !s.consolidationDue() {
		return
	}
	if !s.turn.TryLock() {
		// A turn is in flight; the next tick looks again.
		return
	}
	defer s.turn.Unlock()
	// A message that comes between the compaction and the consolidation
	// waits as for either.
	defer s.beginUpkeep(s.upkeepWords().wait)()
	if trigger != "" {
		_, _ = s.compactLocked(ctx, trigger)
	}
	if s.consolidationDue() {
		s.consolidateLocked(ctx)
	}
}

// consolidationDue says the memory's consolidation is due now; an agent that
// keeps no memory has none.
func (s *Supervisor) consolidationDue() bool {
	if !s.keepsMemory() {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upkeep.consolidationDue(now)
}

// consolidateFrame opens the consolidation's upkeep turn.
const consolidateFrame = "(Agentry: memory upkeep. Follow your memory-consolidate skill now: tidy your memory so it stays short to read and right, " +
	"without losing a current fact, log it, and answer in one line with what you changed. The conversation goes on after it. " +
	"This is not a message from the person.)\n\n"

// consolidateText is the consolidation's message.
const consolidateText = "Upkeep: consolidate your memory (memory-consolidate)."

// consolidateLocked runs the memory's daily consolidation: an upkeep turn
// in the conversation as it is, which swaps nothing. A memory unchanged
// since the last run is left alone, and a person's message waiting puts it
// off to a later idle. The caller holds turn.
func (s *Supervisor) consolidateLocked(ctx context.Context) {
	now := s.now()
	defer s.beginUpkeep("Tidying the memory first; your message follows")()
	s.mu.Lock()
	u := s.upkeep
	outDir := s.outDir
	waiting := s.waiting > 0
	s.mu.Unlock()
	if waiting {
		return
	}
	out := Compacted{Trigger: UpkeepConsolidate}
	holdBack := func(reason string) {
		out.Reason = reason
		_ = s.changeUpkeep(func(st *upkeepState) { st.ConsolidateAfter = s.now().Add(upkeepRetry).UTC() })
		s.logUpkeep(outDir, out, "", "")
	}
	if u.upkeepsToday(now) >= maxUpkeepsPerDay-keptForCompaction {
		holdBack(fmt.Sprintf("%d upkeep turns in the last day, and the last %d of the %d are kept for compactions", u.upkeepsToday(now), keptForCompaction, maxUpkeepsPerDay))
		return
	}
	before, digest := s.memoryStat(ctx)
	if digest != "" && digest == u.MemoryDigest {
		// Nothing was written since the last run: nothing to tidy.
		out.Reason, out.FactsBefore, out.FactsAfter = "the memory has not changed since it was last consolidated", before, before
		_ = s.changeUpkeep(func(st *upkeepState) { st.MemoryWrites = 0 })
		s.logUpkeep(outDir, out, "", "")
		return
	}
	_ = s.changeUpkeep(func(st *upkeepState) {
		st.Upkeeps = append(pruneDay(st.Upkeeps, now), now.UTC())
		s.upkeepTurn, s.upkeepMark = true, consolidateText
	})
	s.notice("Tidying the memory")
	out.FactsBefore = before
	tctx, cancel := context.WithTimeout(ctx, upkeepTurnCap)
	err := s.sendLocked(tctx, consolidateFrame, consolidateText)
	cancel()
	s.upkeepSent(err)
	s.mu.Lock()
	out.Turn = s.usage.UserTurns
	s.mu.Unlock()
	after, afterDigest := s.memoryStat(ctx)
	out.FactsAfter = after
	if err != nil {
		holdBack("the upkeep turn failed: " + err.Error())
		s.notice("The memory's tidy did not finish: " + out.Reason)
		return
	}
	_ = s.changeUpkeep(func(st *upkeepState) {
		st.LastConsolidation = s.now().UTC()
		st.MemoryDigest, st.MemoryWrites = afterDigest, 0
	})
	s.logUpkeep(outDir, out, "", "")
	s.notice(fmt.Sprintf("Tidied the memory: %d fact lines before, %d after", before, after))
}

// Compacted is what a compaction did, as the owner socket answers it.
type Compacted struct {
	Trigger string `json:"trigger"`
	// Swapped says a new conversation opened with the handoff.
	Swapped bool `json:"swapped"`
	// Reason is why nothing was swapped.
	Reason        string `json:"reason,omitempty"`
	ContextTokens int64  `json:"contextTokens,omitempty"`
	// Turn is the upkeep turn's number among the segment's user turns.
	Turn int `json:"turn,omitempty"`
	// FactsBefore and FactsAfter count the memory's fact lines around the
	// upkeep turn: -1 when they were not counted, because the memory could
	// not be read or the agent keeps none.
	FactsBefore  int `json:"factsBefore"`
	FactsAfter   int `json:"factsAfter"`
	HandoffBytes int `json:"handoffBytes,omitempty"`
	// HandoffCut says the handoff was longer than its bound and was cut.
	HandoffCut bool `json:"handoffCut,omitempty"`
	// HandoffForm is the handoff's form (HandoffKeyed, HandoffProse).
	HandoffForm string `json:"handoffForm,omitempty"`
	// Swap is how the new conversation started (SwapInSegment,
	// SwapSegment), and SwapMillis how long that took.
	Swap       string `json:"swap,omitempty"`
	SwapMillis int64  `json:"swapMillis,omitempty"`
}

// Compact compacts the conversation now, at the person's asking, whatever
// its size. It refuses while another turn is in flight.
func (s *Supervisor) Compact(ctx context.Context) (Compacted, error) {
	if !s.turn.TryLock() {
		return Compacted{}, ErrBusy
	}
	defer s.turn.Unlock()
	return s.compactLocked(ctx, UpkeepManual)
}

// upkeepFrame opens the upkeep turn: what it is and what it is for.
const upkeepFrame = "(Agentry: memory upkeep. This conversation is about to be compacted: you will continue in a new conversation " +
	"that opens with only your handoff and your memory. Follow your memory-compact skill: save everything of lasting use from " +
	"this conversation to your memory, write the episode page, log it, and answer with the handoff alone. This is not a message " +
	"from the person.)\n\n"

// upkeepText is the upkeep turn's message.
const upkeepText = "Upkeep: compact this conversation (memory-compact)."

// upkeepFrameNoMemory opens the upkeep turn of an agent that keeps no memory:
// the handoff is all that crosses into the new conversation, so it says what
// a useful one holds.
const upkeepFrameNoMemory = "(Agentry: upkeep. This conversation is about to be compacted: you will continue in a new conversation " +
	"that opens with only your handoff. Answer with the handoff alone, in these lines: owed: what you still owe the person; " +
	"ctx: what you need to go on, and for work on files the files you were working on and the last command you ran with its " +
	"outcome; tried: what was tried and how it went; open: what is still open. Keep it short and exact. This is not a message " +
	"from the person.)\n\n"

// upkeepTextNoMemory is the upkeep turn's message for an agent that keeps no
// memory.
const upkeepTextNoMemory = "Upkeep: compact this conversation."

// keepsMemory says the agent keeps a memory to save its conversation to.
func (s *Supervisor) keepsMemory() bool { return s.opts.Agent.Builtin.KeepsMemory() }

// upkeepWords are what a compaction's upkeep turn says to the agent and what
// the person is told while it runs.
type upkeepWords struct {
	// frame and text open the upkeep turn and are its message.
	frame, text string
	// wait is told to a message that waits for it; start and done are the
	// notices before and after it.
	wait, start, done string
}

var (
	// memoryUpkeep is an agent that keeps a memory saving to it first.
	memoryUpkeep = upkeepWords{
		frame: upkeepFrame, text: upkeepText,
		wait:  "Saving to memory first; your message follows",
		start: "Saving to memory before a fresh conversation",
		done:  "Compacted: what the conversation produced is in the memory, and a fresh conversation opens with the handoff",
	}
	// handoffUpkeep is an agent that keeps no memory writing its handoff.
	handoffUpkeep = upkeepWords{
		frame: upkeepFrameNoMemory, text: upkeepTextNoMemory,
		wait:  "Compacting the conversation first; your message follows",
		start: "Writing a handoff before a fresh conversation",
		done:  "Compacted: a fresh conversation opens with the handoff",
	}
)

// upkeepWords are this agent's.
func (s *Supervisor) upkeepWords() upkeepWords {
	if s.keepsMemory() {
		return memoryUpkeep
	}
	return handoffUpkeep
}

// compactLocked runs one compaction: the upkeep turn, the handoff and the
// swap. The caller holds turn.
func (s *Supervisor) compactLocked(ctx context.Context, trigger string) (Compacted, error) {
	now := s.now()
	s.mu.Lock()
	u := s.upkeep
	outDir := s.outDir
	s.mu.Unlock()
	out := Compacted{Trigger: trigger, ContextTokens: u.ContextTokens}
	if u.upkeepsToday(now) >= maxUpkeepsPerDay {
		out.Reason = fmt.Sprintf("%d upkeep turns in the last day, the most there may be", maxUpkeepsPerDay)
		s.logUpkeep(outDir, out, "", "")
		if trigger != UpkeepManual {
			// An automatic trigger that cannot run waits rather than asking
			// again every minute or before every message.
			_ = s.changeUpkeep(func(st *upkeepState) { st.RetryAfter = now.Add(upkeepRetry).UTC() })
		}
		return out, nil
	}
	memory, words := s.keepsMemory(), s.upkeepWords()
	_ = s.changeUpkeep(func(st *upkeepState) {
		st.Upkeeps = append(pruneDay(st.Upkeeps, now), now.UTC())
		s.upkeepTurn, s.upkeepMark = true, words.text
	})
	defer s.beginUpkeep(words.wait)()
	s.notice(words.start)
	var digestBefore string
	out.FactsBefore, out.FactsAfter = -1, -1
	if memory {
		out.FactsBefore, digestBefore = s.memoryStat(ctx)
	}

	events, unsubscribe := s.Subscribe()
	tctx, cancel := context.WithTimeout(ctx, upkeepTurnCap)
	err := s.sendLocked(tctx, words.frame, words.text)
	cancel()
	s.upkeepSent(err)
	answer := lastAnswer(events, func(ev session.Event) bool { return ev.Upkeep })
	unsubscribe()
	s.mu.Lock()
	out.Turn = s.usage.UserTurns
	if memory {
		// The upkeep turn wrote the memory, or may have.
		s.upkeep.MemoryWrites++
	}
	s.mu.Unlock()
	oldConv := s.conversationID()
	handoff, form, bad := parseHandoff(answer)
	var digestAfter string
	if memory {
		out.FactsAfter, digestAfter = s.memoryStat(ctx)
	}
	switch {
	case err != nil:
		out.Reason = "the upkeep turn failed: " + err.Error()
	case bad != "":
		out.Reason = bad
	case form == HandoffProse && digestBefore != "" && digestBefore == digestAfter:
		// A keyed handoff says the agent followed memory-compact; prose
		// does not, and with the memory unchanged nothing shows the
		// conversation was saved before it is left behind. An agent that
		// keeps no memory has no digest: its handoff is all there is to
		// save, and prose is taken as it is.
		out.Reason = "the upkeep turn's answer was not a handoff, and the memory did not change"
	}
	if out.Reason != "" {
		if trigger != UpkeepManual {
			_ = s.changeUpkeep(func(st *upkeepState) { st.RetryAfter = s.now().Add(upkeepRetry).UTC() })
		}
		s.logUpkeep(outDir, out, oldConv, "")
		s.notice("The upkeep did not finish, so the conversation goes on as it was: " + out.Reason)
		return out, nil
	}
	out.HandoffBytes, out.HandoffCut, out.HandoffForm = len(handoff), strings.HasSuffix(handoff, handoffCutMark), form
	swap, took, err := s.swapConversation(ctx, handoff)
	out.Swap, out.SwapMillis = swap, took.Milliseconds()
	var unstored *unstoredError
	switch {
	case errors.As(err, &unstored):
		// The agent is in the new conversation; only the record of it for
		// the next start failed.
		out.Reason = err.Error()
		s.notice("A fresh conversation started, but " + err.Error())
	case err != nil:
		out.Reason = "the new conversation could not start: " + err.Error()
		s.logUpkeep(outDir, out, oldConv, "")
		s.notice("The handoff was written, but the fresh conversation could not start: " + err.Error())
		return out, err
	}
	out.Swapped = true
	var newOut string
	_ = s.changeUpkeep(func(st *upkeepState) {
		st.LastCompaction = s.now().UTC()
		st.TurnsSince, st.ContextTokens = 0, 0
		newOut = s.outDir
	})
	// After a segment restart the ended segment's receipts are closed
	// with its envelope, and the record of the swap is the new segment's
	// first.
	s.logUpkeep(newOut, out, oldConv, s.conversationID())
	s.notice(words.done)
	return out, nil
}

// coldDue says a cold-cache compaction is due before the person's next
// message: on a harness the supervisor cannot clear old tool results on
// (every one but docker-agent, whose harness clears them itself), once its
// prompt cache has expired.
func (s *Supervisor) coldDue() bool {
	if s.adapter == adapter.DefaultName {
		return false
	}
	provider := ""
	if s.opts.Policy != nil {
		provider = s.opts.Policy.Model.Provider
	}
	after := adapter.ColdAfter(s.adapter, provider)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upkeep.coldDue(now, after)
}

// coldWords are what the person is told while a cold-cache compaction the
// harness runs itself is under way.
var coldWords = upkeepWords{
	wait:  "Compacting the conversation after the pause; your message follows",
	start: "Compacting the conversation after the pause, in the running harness",
	done:  "Compacted: the harness summarised the conversation in place",
}

// coldCompactLocked runs a cold-cache compaction: the running harness
// compacts the conversation in place where it can (codex, Copilot CLI), and
// otherwise the upkeep turn and a new conversation do. A harness compaction
// that fails waits an hour, as a failed automatic upkeep does, rather than
// falling back in the same wait: the person's message waits for one upkeep's
// bound at most. The caller holds turn.
func (s *Supervisor) coldCompactLocked(ctx context.Context) (Compacted, error) {
	s.mu.Lock()
	sess, u, outDir := s.sess, s.upkeep, s.outDir
	s.mu.Unlock()
	c, ok := sess.(engine.ConversationCompactor)
	now := s.now()
	if !ok || !c.CanCompact() || u.upkeepsToday(now) >= maxUpkeepsPerDay {
		// compactLocked keeps the daily cap and the hour's wait itself.
		return s.compactLocked(ctx, UpkeepCold)
	}
	out := Compacted{Trigger: UpkeepCold, ContextTokens: u.ContextTokens, FactsBefore: -1, FactsAfter: -1, Swap: SwapInPlace}
	defer s.beginUpkeep(coldWords.wait)()
	s.notice(coldWords.start)
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, upkeepTurnCap)
	err := c.CompactConversation(tctx)
	cancel()
	out.SwapMillis = time.Since(start).Milliseconds()
	conv := s.conversationID()
	_ = s.changeUpkeep(func(st *upkeepState) {
		st.Upkeeps = append(pruneDay(st.Upkeeps, now), now.UTC())
		if err != nil {
			st.RetryAfter = s.now().Add(upkeepRetry).UTC()
			return
		}
		st.LastCompaction = s.now().UTC()
		st.TurnsSince = 0
	})
	if err != nil {
		out.Reason = "the harness's own compaction failed: " + err.Error()
		s.logUpkeep(outDir, out, conv, conv)
		s.notice("The harness could not compact the conversation, so it goes on as it was: " + err.Error())
		return out, nil
	}
	s.logUpkeep(outDir, out, conv, conv)
	s.notice(coldWords.done)
	return out, nil
}

func pruneDay(ts []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range ts {
		if now.Sub(t) < 24*time.Hour {
			out = append(out, t)
		}
	}
	return out
}

// conversationID is the harness's id for the conversation the running
// segment is in.
func (s *Supervisor) conversationID() string {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	if sess == nil {
		return ""
	}
	id, _, _ := sess.Conversation()
	return id
}

// handoffCutMark ends a handoff cut at its bound.
const handoffCutMark = " [cut]"

// handoffKeys are the lines a handoff is made of.
var handoffKeys = []string{"owed:", "ctx:", "tried:", "open:", "pages:"}

// The forms of a handoff, as its record says them.
const (
	// HandoffKeyed: the answer held the handoff's key lines, and the
	// handoff is the text from the first of them on.
	HandoffKeyed = "keyed"
	// HandoffProse: the answer held none, and the handoff is the whole
	// answer. Some models write the handoff as a paragraph; it still says
	// what the conversation left open.
	HandoffProse = "prose"
)

// parseHandoff takes the handoff from an upkeep turn's answer: the text from
// its first handoff line on, or the whole answer when it has none, at most
// maxHandoff bytes, and which form it was in. The reason is set when there is
// no answer.
func parseHandoff(answer string) (handoff, form, reason string) {
	answer = strings.TrimSpace(answer)
	start := -1
	for _, k := range handoffKeys {
		for i := 0; ; {
			j := strings.Index(answer[i:], k)
			if j < 0 {
				break
			}
			at := i + j
			if at == 0 || answer[at-1] == '\n' {
				if start < 0 || at < start {
					start = at
				}
				break
			}
			i = at + len(k)
		}
	}
	if answer == "" {
		return "", "", "the upkeep turn gave no answer"
	}
	form = HandoffProse
	if start >= 0 {
		answer, form = answer[start:], HandoffKeyed
	}
	h := strings.TrimSpace(answer)
	if len(h) > maxHandoff {
		h = strings.ToValidUTF8(h[:maxHandoff], "") + handoffCutMark
	}
	return h, form, ""
}

// handoffCarried is the text a compacted conversation's first turn starts
// with; memory says the agent keeps one, which then holds what the last
// conversation produced. The handoff is quoted, so it cannot close this frame
// and open one that passes for Agentry's; what it says is still the agent's
// own words from the last conversation, which the agent may act on.
func handoffCarried(h string, memory bool) string {
	saved := ","
	if memory {
		saved = ": what it produced is in your memory,"
	}
	return "(Agentry compacted your conversation" + saved + " and this is the handoff you wrote, quoted. " +
		"The handoff: " + strconv.Quote(h) + ")\n\n"
}

// How a compaction swapped the conversation, as its record says it.
const (
	// SwapInSegment: the running harness started the new conversation.
	SwapInSegment = "in-segment"
	// SwapSegment: the segment ended and the next started in a new
	// conversation.
	SwapSegment = "segment"
	// SwapInPlace: the running harness compacted the conversation in
	// place, which stays the same conversation (the record's Swapped is
	// false).
	SwapInPlace = "in-place"
)

// compactionReason is why a compacted conversation is not the one before.
const compactionReason = "a compaction: the new conversation opens with the agent's handoff"

// swapConversation moves the agent to a new conversation that opens with the
// handoff: inside the running segment where its harness can start one, and
// otherwise by ending the segment and starting the next with no
// conversation to continue. It says which, and how long that swap took (a
// fallback's restart alone). An *unstoredError says the in-segment swap
// happened and only its record for the next start failed. The caller holds
// turn.
func (s *Supervisor) swapConversation(ctx context.Context, handoff string) (string, time.Duration, error) {
	start := time.Now()
	err := s.swapInSegment(ctx, handoff)
	var unstored *unstoredError
	switch {
	case err == nil, errors.As(err, &unstored):
		return SwapInSegment, time.Since(start), err
	case !errors.Is(err, engine.ErrNoNewConversation):
		// The harness could not start one: the segment restart can.
		s.notice("The running agent could not start a fresh conversation, so it restarts in one: " + err.Error())
	}
	start = time.Now()
	if err := s.setPendingHandoff(handoff); err != nil {
		return SwapSegment, 0, err
	}
	if err := s.updateConfig(func(c *Config) error {
		c.Conversation = nil
		return nil
	}); err != nil {
		return SwapSegment, 0, err
	}
	err = s.nextSegmentFor(ctx, "Starting a fresh conversation that opens with the handoff")
	return SwapSegment, time.Since(start), err
}

// newConversationWait bounds the running harness's start of a new
// conversation, which the person's waiting message waits for.
const newConversationWait = time.Minute

// unstoredError is an in-segment swap whose new conversation started and
// could not be recorded as the agent's for its next start.
type unstoredError struct{ err error }

func (e *unstoredError) Error() string {
	return "it could not be recorded for the next start: " + e.err.Error()
}

func (e *unstoredError) Unwrap() error { return e.err }

// swapInSegment starts the new conversation in the running segment's
// harness, records it as the agent's conversation, and has the next turn
// open with the handoff. engine.ErrNoNewConversation when the harness
// cannot; any other error before the harness started one leaves the
// conversation as it was, and one after is an *unstoredError.
func (s *Supervisor) swapInSegment(ctx context.Context, handoff string) error {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	r, ok := sess.(engine.ConversationRenewer)
	if !ok {
		return engine.ErrNoNewConversation
	}
	// The handoff is kept before the harness moves: once it has, the new
	// conversation must open with it, even after a stop.
	if err := s.setPendingHandoff(handoff); err != nil {
		return err
	}
	nctx, cancel := context.WithTimeout(ctx, newConversationWait)
	id, err := r.NewConversation(nctx, compactionReason)
	cancel()
	if err != nil {
		_ = s.setPendingHandoff("")
		return err
	}
	s.mu.Lock()
	s.carry, s.carryHandoff, s.persona, s.resumed, s.resumeReason = handoffCarried(handoff, s.keepsMemory()), handoff, "", false, compactionReason
	s.memoryFrameDue = true
	segment, runID := s.segment, s.runID
	s.mu.Unlock()
	resumed := false
	_ = appendEntry(s.opts.Home, s.opts.Agent.Name, Entry{Segment: segment, Run: runID, Resumed: &resumed,
		Event: session.Event{Kind: KindConversationStart, Time: time.Now().UTC(), Text: compactionReason}})
	if err := s.updateConfig(func(c *Config) error {
		c.Conversation = &Conversation{Adapter: s.adapter, ID: id}
		// A new conversation has been given nothing yet.
		c.PersonaGiven = ""
		return nil
	}); err != nil {
		return &unstoredError{err: err}
	}
	return nil
}

// setPendingHandoff keeps the handoff a new conversation is to open with in
// upkeep.json ("" takes it off).
func (s *Supervisor) setPendingHandoff(h string) error {
	return s.changeUpkeep(func(st *upkeepState) { st.Handoff = h })
}

// changeUpkeep applies change (nil: none) to the upkeep state and saves it.
// Every write of upkeep.json goes through here: the state is copied while
// upkeepSave is held, so the writes land in the order their copies were taken
// and the last one on disk carries every change made before it. change runs
// under mu, so it may set other fields mu guards, and must not take mu or
// upkeepSave itself; upkeepSave is always taken before mu.
func (s *Supervisor) changeUpkeep(change func(st *upkeepState)) error {
	s.upkeepSave.Lock()
	defer s.upkeepSave.Unlock()
	s.mu.Lock()
	if change != nil {
		change(&s.upkeep)
	}
	u := s.upkeep
	s.mu.Unlock()
	return saveUpkeep(s.opts.Home, s.opts.Agent.Name, u)
}

// handoffSent settles the handoff a turn of sess carried: delivered, it is
// no longer pending; not, the segment's next turn carries it again.
func (s *Supervisor) handoffSent(sess engine.PersistentSession, h string, delivered bool) {
	if delivered {
		_ = s.changeUpkeep(func(st *upkeepState) {
			if st.Handoff == h {
				st.Handoff = ""
			}
		})
		return
	}
	s.mu.Lock()
	if s.sess == sess && s.carryHandoff == "" && s.upkeep.Handoff == h {
		s.carry, s.carryHandoff = handoffCarried(h, s.keepsMemory())+s.carry, h
	}
	s.mu.Unlock()
}

// factCountWait bounds each fact count, so the counts before and after an
// upkeep turn keep a waiting message within its bound.
const factCountWait = 20 * time.Second

// factLine is a fact line of the memory's schema, current or superseded.
var factLine = regexp.MustCompile(`^~?F[0-9]+ `)

// memoryStat is the count of the memory's fact lines and a digest of every
// file it holds, read by a one-shot as the export reads them: -1 and "" when
// it could not be read.
func (s *Supervisor) memoryStat(ctx context.Context) (int, string) {
	if s.opts.CountFacts != nil {
		digest := ""
		if s.opts.MemoryDigest != nil {
			digest = s.opts.MemoryDigest(ctx)
		}
		return s.opts.CountFacts(ctx), digest
	}
	if s.opts.Docker == nil || s.opts.Policy == nil {
		return -1, ""
	}
	volume := MemoryVolumeIn(s.opts.Home, s.opts.Agent.Name)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	cctx, cancel := context.WithTimeout(ctx, factCountWait)
	defer cancel()
	go func() {
		image := s.opts.Policy.RunnerImageTag(runnerimage.ContentVersion())
		err := engine.ExportVolume(cctx, s.opts.Docker, image, volume, s.opts.Policy.Docker.Runtime, s.opts.Policy.Docker.Limits, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	n, digest, err := memoryStatOf(pr)
	_ = pr.CloseWithError(err)
	if runErr := <-done; err == nil {
		err = runErr
	}
	if err != nil {
		return -1, ""
	}
	return n, digest
}

// countFactLines counts the fact lines of the pages in a tar of the memory.
func countFactLines(r io.Reader) (int, error) {
	n, _, err := memoryStatOf(r)
	return n, err
}

// memoryStatOf reads a tar of the memory: the fact lines of its pages,
// where the notes copied in are sources, not pages, and are left out; and a
// digest of every regular file's name and bytes, which changes whenever the
// memory does.
func memoryStatOf(r io.Reader) (int, string, error) {
	tr := tar.NewReader(r)
	sum := sha256.New()
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
		}
		if err != nil {
			return 0, "", err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg {
			continue
		}
		page := strings.HasPrefix(name, "pages/") && !strings.HasPrefix(name, "pages/notes/") && strings.HasSuffix(name, ".md")
		_, _ = fmt.Fprintf(sum, "%s\x00%d\x00", name, h.Size)
		switch {
		case page && h.Size > maxCountedPage:
			return 0, "", fmt.Errorf("the page %s is %d bytes, too large to count", name, h.Size)
		case h.Size > maxCountedPage:
			// Too large to read for the digest: its name and size stand
			// for it.
			continue
		}
		body := io.TeeReader(tr, sum)
		if !page {
			if _, err := io.Copy(io.Discard, body); err != nil {
				return 0, "", err
			}
			continue
		}
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if factLine.Match(sc.Bytes()) {
				n++
			}
		}
		if err := sc.Err(); err != nil {
			// A count that skipped part of a page would understate what
			// the memory holds: none is better.
			return 0, "", fmt.Errorf("count the facts of %s: %w", name, err)
		}
	}
}

// maxCountedPage bounds a file the memory's count and digest read.
const maxCountedPage = 16 << 20

// UpkeepRecord is one line of receipts/upkeep.ndjson: a compaction, or a
// consolidation (Trigger UpkeepConsolidate), whether it ran or why not.
type UpkeepRecord struct {
	Time            time.Time `json:"time"`
	Trigger         string    `json:"trigger"`
	Swapped         bool      `json:"swapped"`
	Reason          string    `json:"reason,omitempty"`
	ContextTokens   int64     `json:"contextTokens,omitempty"`
	OldConversation string    `json:"oldConversation,omitempty"`
	NewConversation string    `json:"newConversation,omitempty"`
	Turn            int       `json:"turn,omitempty"`
	FactsBefore     int       `json:"factsBefore"`
	FactsAfter      int       `json:"factsAfter"`
	HandoffBytes    int       `json:"handoffBytes,omitempty"`
	HandoffCut      bool      `json:"handoffCut,omitempty"`
	HandoffForm     string    `json:"handoffForm,omitempty"`
	Swap            string    `json:"swap,omitempty"`
	SwapMillis      int64     `json:"swapMillis,omitempty"`
}

// logUpkeep appends a compaction's line to the receipts of the segment it
// ran in.
func (s *Supervisor) logUpkeep(outDir string, c Compacted, oldConv, newConv string) {
	if outDir == "" {
		return
	}
	dir := filepath.Join(outDir, "receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(UpkeepRecord{Time: s.now().UTC(), Trigger: c.Trigger, Swapped: c.Swapped, Reason: c.Reason,
		ContextTokens: c.ContextTokens, OldConversation: oldConv, NewConversation: newConv, Turn: c.Turn,
		FactsBefore: c.FactsBefore, FactsAfter: c.FactsAfter, HandoffBytes: c.HandoffBytes, HandoffCut: c.HandoffCut,
		HandoffForm: c.HandoffForm, Swap: c.Swap, SwapMillis: c.SwapMillis})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, upkeepLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the segment's own receipts
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	_, _ = w.Write(append(b, '\n'))
	_ = w.Flush()
}

// memoryQuiet is how long the person has sent nothing before their next turn
// is taken for a new topic and opens with the memory frame.
const memoryQuiet = 30 * time.Minute

// memoryFrame reminds an agent that keeps a memory to read it: Agentry cannot
// tell when the topic changes, only when the conversation is new or the
// person comes back after a while.
const memoryFrame = "(Agentry: this may be a new topic. Read memory/index.md and the pages it names before you answer.)\n\n"

// memoryFrameLocked is the memory frame when the turn about to be sent is a
// new conversation's first, or the person's first after memoryQuiet with
// none; person says the turn is theirs. The caller holds mu.
func (s *Supervisor) memoryFrameLocked(person bool) string {
	if !s.keepsMemory() {
		return ""
	}
	last := s.upkeep.LastPersonTurn
	if s.memoryFrameDue || s.carryHandoff != "" || (person && !last.IsZero() && s.now().Sub(last) >= memoryQuiet) {
		return memoryFrame
	}
	return ""
}

// LastUpkeep is the agent's newest compaction record and its newest
// consolidation record, across its segments' receipts; nil where there is
// none. What cannot be read (a file, a line) is skipped: the records are a
// report, and a damaged one hides nothing else.
func LastUpkeep(home, name string) (compaction, consolidation *UpkeepRecord) {
	dir := filepath.Join(Dir(home, name), segmentsDir)
	segments, _ := os.ReadDir(dir)
	for _, seg := range segments {
		if !seg.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, seg.Name(), "receipts", upkeepLog)) //nolint:gosec // the agent's own receipts
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r UpkeepRecord
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Time.IsZero() {
				continue
			}
			last := &compaction
			if r.Trigger == UpkeepConsolidate {
				last = &consolidation
			}
			if *last == nil || !r.Time.Before((*last).Time) {
				*last = &r
			}
		}
		_ = f.Close()
	}
	return compaction, consolidation
}
