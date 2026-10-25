package persistent

// Memory upkeep: an agent that keeps a memory compacts its own conversation.
// When the person has sent nothing for a while and the conversation is
// large, or when a turn has filled most of the model's window, or when the
// person asks, the supervisor sends the agent an upkeep turn: it saves what
// the conversation produced to its memory and answers with a handoff. The
// supervisor then starts a new conversation that opens with the handoff,
// quoted. A failed upkeep turn swaps nothing, and the conversation goes on
// as it was. Each compaction is a line of the segment's
// receipts/upkeep.ndjson, with the memory's fact lines counted before and
// after so a line the model lost shows.

import (
	"archive/tar"
	"bufio"
	"context"
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

// The triggers a compaction records.
const (
	UpkeepIdle   = "idle"
	UpkeepInUse  = "in-use"
	UpkeepManual = "manual"
)

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

// noteUpkeepEvent keeps what a segment's event tells upkeep. The caller
// holds mu, and a user turn carries the marks it was sent with.
func (s *Supervisor) noteUpkeepEvent(ev session.Event) {
	if !s.opts.Agent.Builtin.KeepsMemory() {
		return
	}
	switch ev.Kind {
	case session.KindUserTurn:
		if ev.Synthetic {
			return
		}
		if ev.Task == "" && !ev.Upkeep {
			s.upkeep.LastPersonTurn = ev.Time
		}
		if !ev.Upkeep {
			s.upkeep.TurnsSince++
		}
		s.upkeepDirty = true
	case session.KindUsage:
		if ev.ContextTokens > 0 {
			s.upkeep.ContextTokens = ev.ContextTokens
			if ev.ContextLimit > 0 {
				s.upkeep.ContextLimit = ev.ContextLimit
			}
			s.upkeepDirty = true
		}
	case session.KindAssistantTurn:
		if s.upkeepDirty {
			// Saved off the events' lock, which every watcher waits on.
			u := s.upkeep
			s.upkeepDirty = false
			go func() {
				s.upkeepSave.Lock()
				defer s.upkeepSave.Unlock()
				_ = saveUpkeep(s.opts.Home, s.opts.Agent.Name, u)
			}()
		}
	}
}

// upkeepTick compacts the conversation when an idle or an in-use
// compaction is due, from the heartbeat's ticker.
func (s *Supervisor) upkeepTick(ctx context.Context) {
	if !s.opts.Agent.Builtin.KeepsMemory() {
		return
	}
	trigger := s.upkeepDue()
	if trigger == "" {
		return
	}
	if !s.turn.TryLock() {
		// A turn is in flight; the next tick looks again.
		return
	}
	defer s.turn.Unlock()
	_, _ = s.compactLocked(ctx, trigger)
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
	Turn         int `json:"turn,omitempty"`
	FactsBefore  int `json:"factsBefore"`
	FactsAfter   int `json:"factsAfter"`
	HandoffBytes int `json:"handoffBytes,omitempty"`
	// HandoffCut says the handoff was longer than its bound and was cut.
	HandoffCut bool `json:"handoffCut,omitempty"`
}

// ErrNoMemory refuses a compaction of an agent that keeps no memory.
var ErrNoMemory = errors.New("keeps no memory to compact into")

// Compact compacts the conversation now, at the person's asking, whatever
// its size. It refuses while another turn is in flight.
func (s *Supervisor) Compact(ctx context.Context) (Compacted, error) {
	if !s.opts.Agent.Builtin.KeepsMemory() {
		return Compacted{}, fmt.Errorf("the %s agent %w", s.opts.Agent.Name, ErrNoMemory)
	}
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

// compactLocked runs one compaction: the upkeep turn, the handoff and the
// swap. The caller holds turn.
func (s *Supervisor) compactLocked(ctx context.Context, trigger string) (Compacted, error) {
	home, name := s.opts.Home, s.opts.Agent.Name
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
			s.mu.Lock()
			s.upkeep.RetryAfter = now.Add(upkeepRetry).UTC()
			_ = saveUpkeep(home, name, s.upkeep)
			s.mu.Unlock()
		}
		return out, nil
	}
	s.mu.Lock()
	s.upkeep.Upkeeps = append(pruneDay(s.upkeep.Upkeeps, now), now.UTC())
	s.upkeeping, s.upkeepTurn = true, true
	_ = saveUpkeep(home, name, s.upkeep)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.upkeeping, s.upkeepTurn = false, false
		s.mu.Unlock()
	}()
	s.notice("Saving to memory before a fresh conversation")
	out.FactsBefore = s.countFacts(ctx)

	events, unsubscribe := s.Subscribe()
	tctx, cancel := context.WithTimeout(ctx, upkeepTurnCap)
	err := s.sendLocked(tctx, upkeepFrame, upkeepText)
	cancel()
	answer := lastAnswer(events, func(ev session.Event) bool { return ev.Upkeep })
	unsubscribe()
	s.mu.Lock()
	out.Turn = s.usage.UserTurns
	s.mu.Unlock()
	oldConv := s.conversationID()
	handoff, bad := parseHandoff(answer)
	switch {
	case err != nil:
		out.Reason = "the upkeep turn failed: " + err.Error()
	case bad != "":
		out.Reason = bad
	}
	out.FactsAfter = s.countFacts(ctx)
	if out.Reason != "" {
		if trigger != UpkeepManual {
			s.mu.Lock()
			s.upkeep.RetryAfter = s.now().Add(upkeepRetry).UTC()
			_ = saveUpkeep(home, name, s.upkeep)
			s.mu.Unlock()
		}
		s.logUpkeep(outDir, out, oldConv, "")
		s.notice("The memory upkeep did not finish, so the conversation goes on as it was: " + out.Reason)
		return out, nil
	}
	out.HandoffBytes, out.HandoffCut = len(handoff), strings.HasSuffix(handoff, handoffCutMark)
	if err := s.swapConversation(ctx, handoff); err != nil {
		out.Reason = "the new conversation could not start: " + err.Error()
		s.logUpkeep(outDir, out, oldConv, "")
		s.notice("The memory was saved, but the fresh conversation could not start: " + err.Error())
		return out, err
	}
	out.Swapped = true
	s.mu.Lock()
	s.upkeep.LastCompaction = s.now().UTC()
	s.upkeep.TurnsSince, s.upkeep.ContextTokens = 0, 0
	_ = saveUpkeep(home, name, s.upkeep)
	newOut := s.outDir
	s.mu.Unlock()
	// The ended segment's receipts are closed with its envelope: the
	// record of the swap is the new segment's first.
	s.logUpkeep(newOut, out, oldConv, s.conversationID())
	s.notice("Compacted: what the conversation produced is in the memory, and a fresh conversation opens with the handoff")
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

// parseHandoff takes the handoff from an upkeep turn's answer: the text from
// its first handoff line on, at most maxHandoff bytes. The reason is set when
// the answer holds none.
func parseHandoff(answer string) (string, string) {
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
	if start < 0 {
		if answer == "" {
			return "", "the upkeep turn gave no answer"
		}
		return "", "the upkeep turn's answer was not a handoff"
	}
	h := strings.TrimSpace(answer[start:])
	if len(h) > maxHandoff {
		h = strings.ToValidUTF8(h[:maxHandoff], "") + handoffCutMark
	}
	return h, ""
}

// handoffCarried is the text a compacted conversation's first turn starts
// with. The handoff is quoted, so it cannot close this frame and open one
// that passes for Agentry's; what it says is still the agent's own words
// from the last conversation, which the agent may act on.
func handoffCarried(h string) string {
	return "(Agentry compacted your conversation: what it produced is in your memory, and this is the handoff you wrote, quoted. " +
		"Read memory/index.md and the pages it names before you answer from what came before. The handoff: " + strconv.Quote(h) + ")\n\n"
}

// swapConversation ends the running segment and starts the next in a new
// conversation that opens with the handoff. The caller holds turn.
func (s *Supervisor) swapConversation(ctx context.Context, handoff string) error {
	if err := s.updateConfig(func(c *Config) error {
		c.Conversation = nil
		return nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.handoff = handoff
	s.mu.Unlock()
	return s.nextSegmentFor(ctx, "Starting a fresh conversation that opens with the handoff")
}

// factCountWait bounds each fact count, so the counts before and after an
// upkeep turn keep a waiting message within its bound.
const factCountWait = 20 * time.Second

// factLine is a fact line of the memory's schema, current or superseded.
var factLine = regexp.MustCompile(`^~?F[0-9]+ `)

// countFacts counts the fact lines of the memory's pages, read by a one-shot
// as the export reads them; -1 when it could not be read.
func (s *Supervisor) countFacts(ctx context.Context) int {
	if s.opts.CountFacts != nil {
		return s.opts.CountFacts(ctx)
	}
	if s.opts.Docker == nil || s.opts.Policy == nil {
		return -1
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
	n, err := countFactLines(pr)
	_ = pr.CloseWithError(err)
	if runErr := <-done; err == nil {
		err = runErr
	}
	if err != nil {
		return -1
	}
	return n
}

// countFactLines counts the fact lines of the pages in a tar of the memory;
// the notes copied in are sources, not pages, and are left out.
func countFactLines(r io.Reader) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(name, "pages/") || strings.HasPrefix(name, "pages/notes/") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if h.Size > maxCountedPage {
			return 0, fmt.Errorf("the page %s is %d bytes, too large to count", name, h.Size)
		}
		sc := bufio.NewScanner(tr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if factLine.Match(sc.Bytes()) {
				n++
			}
		}
		if err := sc.Err(); err != nil {
			// A count that skipped part of a page would understate what
			// the memory holds: none is better.
			return 0, fmt.Errorf("count the facts of %s: %w", name, err)
		}
	}
}

// maxCountedPage bounds a page the fact count reads.
const maxCountedPage = 16 << 20

// upkeepRecord is one line of receipts/upkeep.ndjson.
type upkeepRecord struct {
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
	b, err := json.Marshal(upkeepRecord{Time: s.now().UTC(), Trigger: c.Trigger, Swapped: c.Swapped, Reason: c.Reason,
		ContextTokens: c.ContextTokens, OldConversation: oldConv, NewConversation: newConv, Turn: c.Turn,
		FactsBefore: c.FactsBefore, FactsAfter: c.FactsAfter, HandoffBytes: c.HandoffBytes, HandoffCut: c.HandoffCut})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, upkeepLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the segment's own receipts
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	_, _ = w.Write(append(b, '\n'))
	_ = w.Flush()
}
