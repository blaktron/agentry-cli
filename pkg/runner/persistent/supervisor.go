package persistent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// ErrBusy refuses a turn sent while another is in flight.
var ErrBusy = errors.New("a turn is in flight; send again when its answer arrives")

// ErrDesktopOnly refuses an agent that runs only in Agentry Desktop.
var ErrDesktopOnly = errors.New("runs only in Agentry Desktop")

// Options start a supervisor.
type Options struct {
	Home  string
	Agent Agent
	// Client is who supervises: ClientCLI or ClientDesktop.
	Client     string
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	// Build is the client's build identity.
	Build  string
	Docker dockerx.Client
	// Stderr gets the segment's progress (image builds, sweeps).
	Stderr io.Writer
	// Packs opens the knowledge packs the roster has on, for each segment;
	// nil refuses a roster with one on.
	Packs Packs
	// StartSegment starts a segment; nil starts it with localrun. Tests
	// put a session of their own here.
	StartSegment func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error)
}

// Supervisor owns one running agent: its lock, its segment, its socket.
type Supervisor struct {
	opts    Options
	owner   *Owner
	since   time.Time
	adapter string
	model   string

	turn sync.Mutex // one turn at a time; TryLock refuses a second as busy
	// seg is held while a segment starts, ends or is swapped for the next,
	// so a stop never meets a segment half made.
	seg sync.Mutex

	mu        sync.Mutex
	sess      engine.PersistentSession
	runID     string
	outDir    string
	segment   int
	applied   Applied
	grants    string // grantsKey of the config the segment started with
	packsDone func()
	ending    *atomic.Bool // set for the segment being ended to start the next
	// segEnded says the segment in sess has ended, with segEnv and segErr
	// its end: a stop after a next segment failed to start does not end it
	// or count it twice.
	segEnded     bool
	segEnv       *receipt.Envelope
	segErr       error
	pumpDone     chan struct{}
	carry        string // the last exchange, for the first turn of a conversation that did not continue
	persona      string // personaKey of what the conversation has been given
	resumed      bool
	resumeReason string
	busy         bool
	usage        receipt.Usage
	watchers     map[int]chan session.Event
	nextWatcher  int
	ln           net.Listener
	stopped      bool // set under mu once Stop has let the watchers go

	done     chan struct{}
	stopOnce sync.Once
	env      *receipt.Envelope
	endErr   error
}

// Start takes the agent's owner lock and starts a segment: the conversation
// the last segment was in is continued where the harness can, and otherwise
// the new conversation's first turn carries the last exchange of the
// transcript, which the envelope records. It refuses an agent whose roster
// turns on what the person has not agreed to on this computer
// (ConsentError). It serves the owner socket until Stop, or until the
// segment ends by itself.
func Start(ctx context.Context, opts Options) (*Supervisor, error) {
	a := opts.Agent
	if a.DesktopOnly && opts.Client != ClientDesktop {
		return nil, fmt.Errorf("%s %w", a.Title, ErrDesktopOnly)
	}
	if !a.Builtin.Persistent() {
		return nil, fmt.Errorf("%s is not a persistent agent of this build", a.Name)
	}
	cfg, err := LoadConfig(opts.Home, a.Name)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("the %s agent is not enabled: agentry agent enable %s", a.Name, a.Name)
	}
	if pending := Unconsented(BuildRoster(a, cfg, nil), cfg); len(pending) > 0 {
		return nil, &ConsentError{Agent: a.Name, Entries: pending}
	}
	owner, err := Acquire(opts.Home, a.Name, opts.Client)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		opts: opts, owner: owner, since: time.Now().UTC(),
		adapter:  localrun.ImageAdapter(adapter.DefaultFor(opts.Policy)),
		watchers: map[int]chan session.Event{},
		done:     make(chan struct{}),
	}
	if opts.Policy != nil {
		s.model = opts.Policy.Model.Provider + "/" + opts.Policy.Model.Name
	}
	s.seg.Lock()
	err = s.startSegment(ctx)
	s.seg.Unlock()
	if err != nil {
		owner.Release()
		return nil, err
	}
	if err := s.listen(); err != nil {
		_, _ = s.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// startSegment starts the agent's next segment from its config as it is,
// records how its conversation began and starts its pump. The caller holds
// seg.
func (s *Supervisor) startSegment(ctx context.Context) error {
	a, home := s.opts.Agent, s.opts.Home
	name := a.Name
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return err
	}
	roster := BuildRoster(a, cfg, nil)
	grants, applied, packsDone, err := Apply(ctx, home, a, cfg, roster, s.opts.Packs)
	if err != nil {
		return err
	}
	runID, err := engine.NewRunID()
	if err != nil {
		packsDone()
		return err
	}
	continueID, mismatch := "", ""
	if c := cfg.Conversation; c != nil && c.ID != "" {
		if c.Adapter == s.adapter {
			continueID = c.ID
		} else {
			mismatch = fmt.Sprintf("the conversation was stored by the %s harness, and this segment runs on %s, which cannot read it", c.Adapter, s.adapter)
		}
	}
	segment := cfg.Segments + 1
	outDir := filepath.Join(Dir(home, name), segmentsDir, runID)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		packsDone()
		return fmt.Errorf("the segment's folder: %w", err)
	}
	seg := localrun.Segment{
		Builtin: a.Builtin, Home: home, Policy: s.opts.Policy, PolicySHA: s.opts.PolicySHA,
		PolicyFile: s.opts.PolicyFile, Build: s.opts.Build, OutDir: outDir, RunID: runID, Stderr: s.opts.Stderr,
		Persistent: engine.PersistentSegment{
			Agent: name, Segment: segment,
			SessionVolume: SessionVolume(name), DataVolume: DataVolume(name),
			ContinueID: continueID, Roster: roster.Receipt(),
		},
		Grants: grants,
	}
	start := s.opts.StartSegment
	if start == nil {
		start = func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error) {
			return seg.Start(ctx, s.opts.Docker)
		}
	}
	sess, err := start(ctx, seg)
	if err != nil {
		packsDone()
		// A start refused before the engine wrote anything leaves no
		// folder behind; one that wrote its envelope keeps it.
		_ = os.Remove(outDir)
		return err
	}
	id, resumed, reason := sess.Conversation()
	if mismatch != "" {
		reason = mismatch
	}
	carry := ""
	if !resumed {
		// Whatever kept the conversation from continuing (a harness that
		// cannot, a store that would not load, another harness), the new
		// one is given the last exchange the agent had, if it had one.
		entries, _ := Transcript(home, name, 400)
		if u, a, ok := lastExchange(entries); ok {
			carry = carried(u, a)
			reason += "; the runner gave the new conversation the last exchange from the transcript"
		} else if cfg.Segments == 0 {
			reason = "the agent's first segment"
		}
		sess.SetResumeReason(reason)
	}
	cfg.Segments = segment
	cfg.Conversation = nil
	if id != "" {
		cfg.Conversation = &Conversation{Adapter: s.adapter, ID: id}
	}
	persona := cfg.PersonaGiven
	if !resumed {
		// A new conversation has been given nothing yet.
		persona = ""
	}
	if err := SaveConfig(home, name, cfg); err != nil {
		_, _ = sess.End(context.WithoutCancel(ctx), false)
		packsDone()
		return err
	}
	ending := &atomic.Bool{}
	pumpDone := make(chan struct{})
	s.mu.Lock()
	s.sess, s.runID, s.outDir, s.segment = sess, runID, outDir, segment
	s.applied, s.grants, s.packsDone, s.ending, s.pumpDone = applied, grantsKey(a, cfg), packsDone, ending, pumpDone
	s.carry, s.persona, s.resumed, s.resumeReason = carry, persona, resumed, reason
	s.usage, s.segEnded, s.segEnv, s.segErr = receipt.Usage{}, false, nil, nil
	s.mu.Unlock()
	text := "continued the conversation"
	if !resumed {
		text = reason
	}
	_ = appendEntry(home, name, Entry{Segment: segment, Run: runID, Resumed: &resumed,
		Event: session.Event{Kind: KindSegmentStart, Time: time.Now().UTC(), Text: text}})
	go s.pump(sess, segment, runID, ending, pumpDone)
	return nil
}

// pump records a segment's events in the transcript, hands them to every
// watcher, and finishes the supervisor when the segment ends by itself. The
// end of a segment ended to start the next is the transcript's alone: a
// chat stays attached across it.
func (s *Supervisor) pump(sess engine.PersistentSession, segment int, runID string, ending *atomic.Bool, done chan struct{}) {
	defer close(done)
	for ev := range sess.Events() {
		swapping := ev.Kind == session.KindSessionEnd && ending.Load()
		s.mu.Lock()
		switch ev.Kind {
		case session.KindUserTurn:
			s.busy = true
			if !ev.Synthetic {
				s.usage.UserTurns++
			}
		case session.KindAssistantTurn, session.KindError, session.KindSessionEnd:
			s.busy = false
		case session.KindToolCall:
			s.usage.Turns++
		case session.KindUsage:
			s.usage.InputTokens += ev.InputTokens
			s.usage.OutputTokens += ev.OutputTokens
			s.usage.TotalTokens += ev.TotalTokens
		}
		if !swapping {
			s.broadcast(ev)
		}
		s.mu.Unlock()
		if kept(ev.Kind) {
			ev.Raw = nil
			_ = appendEntry(s.opts.Home, s.opts.Agent.Name, Entry{Segment: segment, Run: runID, Event: ev})
		}
		if ev.Kind == session.KindSessionEnd && !swapping {
			go func() { _, _ = s.Stop(context.Background()) }()
		}
	}
}

// broadcast hands ev to every watcher. The caller holds mu.
func (s *Supervisor) broadcast(ev session.Event) {
	for id, w := range s.watchers {
		select {
		case w <- ev:
		default:
			// A watcher that stopped reading is let go; the transcript
			// and the receipts have every event.
			close(w)
			delete(s.watchers, id)
		}
	}
}

// Send sends one turn and returns when the agent has answered it. When the
// roster changed since the segment started, the segment is ended and the
// next started first, so the turn reaches an agent given the change. The
// first turn of a conversation that did not continue starts with the last
// exchange, and a turn after the person named the agent or changed its
// personality starts with them.
func (s *Supervisor) Send(ctx context.Context, text string) error {
	if !s.turn.TryLock() {
		return ErrBusy
	}
	defer s.turn.Unlock()
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	stale := grantsKey(s.opts.Agent, cfg) != s.grants
	s.mu.Unlock()
	if stale {
		if err := s.nextSegment(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	sess, carry, given := s.sess, s.carry, s.persona
	s.carry = ""
	s.mu.Unlock()
	frame := carry
	persona := personaKey(cfg)
	if persona != given {
		frame += personaFrame(cfg)
	}
	if frame != "" {
		err = sess.SendCarrying(ctx, frame, text)
	} else {
		err = sess.Send(ctx, text)
	}
	if err == nil && persona != given {
		s.mu.Lock()
		s.persona = persona
		s.mu.Unlock()
		if c, lerr := LoadConfig(s.opts.Home, s.opts.Agent.Name); lerr == nil {
			c.PersonaGiven = persona
			_ = SaveConfig(s.opts.Home, s.opts.Agent.Name, c)
		}
	}
	return err
}

// nextSegment ends the running segment and starts the next from the config
// as it is now, keeping the watchers attached. A next segment that cannot
// start stops the agent, saying why. The caller holds turn.
func (s *Supervisor) nextSegment(ctx context.Context) error {
	s.seg.Lock()
	defer s.seg.Unlock()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return session.ErrEnded
	}
	s.broadcast(session.Event{Kind: session.KindNotice, Time: time.Now().UTC(), Text: "Applying your change: the agent restarts with it, and the conversation continues"})
	s.mu.Unlock()
	_, _ = s.endSegment(ctx, true)
	if err := s.startSegment(ctx); err != nil {
		s.mu.Lock()
		s.broadcast(session.Event{Kind: session.KindError, Time: time.Now().UTC(), Text: "the change could not be applied, so the agent stopped: " + err.Error()})
		s.mu.Unlock()
		go func() { _, _ = s.Stop(context.Background()) }()
		return err
	}
	return nil
}

// endSegment ends the running segment without a final result, waits for its
// pump, adds its usage to the agent's and closes its packs; swap says the
// next segment starts after it. The caller holds seg.
func (s *Supervisor) endSegment(ctx context.Context, swap bool) (*receipt.Envelope, error) {
	s.mu.Lock()
	if s.segEnded {
		defer s.mu.Unlock()
		return s.segEnv, s.segErr
	}
	sess, ending, pumpDone, packsDone := s.sess, s.ending, s.pumpDone, s.packsDone
	s.packsDone = nil
	s.mu.Unlock()
	// A segment ended to start the next ends out of the chats' sight.
	ending.Store(swap)
	env, err := sess.End(ctx, false)
	select {
	case <-pumpDone:
	case <-time.After(10 * time.Second):
	}
	if packsDone != nil {
		packsDone()
	}
	if env != nil {
		u, _ := LoadUsage(s.opts.Home, s.opts.Agent.Name)
		u.Segments++
		u.InputTokens += env.Usage.InputTokens
		u.OutputTokens += env.Usage.OutputTokens
		u.TotalTokens += env.Usage.TotalTokens
		u.UserTurns += env.Usage.UserTurns
		u.ToolCalls += env.Usage.Turns
		u.Updated = time.Now().UTC()
		_ = saveUsage(s.opts.Home, s.opts.Agent.Name, u)
	}
	s.mu.Lock()
	s.segEnded, s.segEnv, s.segErr = true, env, err
	s.mu.Unlock()
	return env, err
}

// Change applies a change to the running agent's config, the supervisor
// alone writing it while it runs. A change to what the segment is given
// takes effect at the next turn, which starts the next segment; the roster
// answered shows it pending until then.
func (s *Supervisor) Change(ch Change) (Roster, error) {
	a := s.opts.Agent
	cfg, err := LoadConfig(s.opts.Home, a.Name)
	if err != nil {
		return nil, err
	}
	if _, err := ApplyChange(s.opts.Home, a, &cfg, ch); err != nil {
		return nil, err
	}
	if err := SaveConfig(s.opts.Home, a.Name, cfg); err != nil {
		return nil, err
	}
	return s.Roster(), nil
}

// Roster is the running agent's roster: an entry switched since the segment
// started is pending.
func (s *Supervisor) Roster() Roster {
	cfg, _ := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	return BuildRoster(s.opts.Agent, cfg, &applied)
}

// Subscribe hands every event from now on to a channel, until cancel or the
// agent's end closes it.
func (s *Supervisor) Subscribe() (<-chan session.Event, func()) {
	ch := make(chan session.Event, 1024)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		close(ch)
		return ch, func() {}
	}
	id := s.nextWatcher
	s.nextWatcher++
	s.watchers[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if w, ok := s.watchers[id]; ok {
			close(w)
			delete(s.watchers, id)
		}
	}
}

// Status is a running agent's state, as `agentry agent show` prints it.
type Status struct {
	Agent        string        `json:"agent"`
	State        string        `json:"state"`
	Client       string        `json:"client"`
	PID          int           `json:"pid"`
	Since        time.Time     `json:"since"`
	Segment      int           `json:"segment"`
	Run          string        `json:"run"`
	SegmentDir   string        `json:"segmentDir"`
	Adapter      string        `json:"adapter"`
	Model        string        `json:"model"`
	Resumed      bool          `json:"resumed"`
	ResumeReason string        `json:"resumeReason,omitempty"`
	Busy         bool          `json:"busy"`
	Usage        receipt.Usage `json:"usage"`
	// Pending says a change waits for the next turn's segment.
	Pending bool `json:"pending,omitempty"`
}

// StateRunning is a running agent's state.
const StateRunning = "running"

// Status is the agent's state now.
func (s *Supervisor) Status() Status {
	cfg, _ := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	pending := grantsKey(s.opts.Agent, cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Agent: s.opts.Agent.Name, State: StateRunning, Client: s.opts.Client, PID: os.Getpid(), Since: s.since,
		Segment: s.segment, Run: s.runID, SegmentDir: s.outDir, Adapter: s.adapter, Model: s.model,
		Resumed: s.resumed, ResumeReason: s.resumeReason, Busy: s.busy, Usage: s.usage,
		Pending: pending != s.grants,
	}
}

// Done is closed once the supervisor has finished: the segment ended, its
// envelope written, the socket closed and the lock released.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// Stop ends the segment without a final result, writes its receipts and
// envelope, adds its usage to the agent's, and lets the agent go. The
// volumes stay. Safe to call more than once and after the segment ended by
// itself; every call returns the segment's envelope.
func (s *Supervisor) Stop(ctx context.Context) (*receipt.Envelope, error) {
	s.stopOnce.Do(func() {
		s.seg.Lock()
		s.env, s.endErr = s.endSegment(ctx, false)
		s.seg.Unlock()
		s.mu.Lock()
		if s.ln != nil {
			_ = s.ln.Close()
			_ = os.Remove(SocketPath(s.opts.Home, s.opts.Agent.Name))
		}
		for id, w := range s.watchers {
			close(w)
			delete(s.watchers, id)
		}
		s.stopped = true
		s.mu.Unlock()
		s.owner.Release()
		close(s.done)
	})
	return s.env, s.endErr
}
