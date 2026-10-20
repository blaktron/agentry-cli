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
	// StartSegment starts a segment; nil starts it with localrun. Tests
	// put a session of their own here.
	StartSegment func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error)
}

// Supervisor owns one running agent: its lock, its segment, its socket.
type Supervisor struct {
	opts    Options
	owner   *Owner
	sess    engine.PersistentSession
	runID   string
	outDir  string
	segment int
	since   time.Time
	adapter string
	model   string

	turn sync.Mutex // one turn at a time; TryLock refuses a second as busy

	mu           sync.Mutex
	carry        string // the last exchange, for the first turn of a conversation that did not continue
	resumed      bool
	resumeReason string
	busy         bool
	usage        receipt.Usage
	watchers     map[int]chan session.Event
	nextWatcher  int
	ln           net.Listener
	stopped      bool // set under mu once Stop has let the watchers go

	pumpDone chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	env      *receipt.Envelope
	endErr   error
}

// Start takes the agent's owner lock and starts a segment: the conversation
// the last segment was in is continued where the harness can, and otherwise
// the new conversation's first turn carries the last exchange of the
// transcript, which the envelope records. It serves the owner socket until
// Stop, or until the segment ends by itself.
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
	owner, err := Acquire(opts.Home, a.Name, opts.Client)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		opts: opts, owner: owner, since: time.Now().UTC(),
		adapter:  localrun.ImageAdapter(adapter.DefaultFor(opts.Policy)),
		watchers: map[int]chan session.Event{},
		pumpDone: make(chan struct{}), done: make(chan struct{}),
	}
	if opts.Policy != nil {
		s.model = opts.Policy.Model.Provider + "/" + opts.Policy.Model.Name
	}
	if err := s.startSegment(ctx, cfg); err != nil {
		owner.Release()
		return nil, err
	}
	go s.pump()
	if err := s.listen(); err != nil {
		_, _ = s.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// startSegment starts the agent's next segment and records how its
// conversation began.
func (s *Supervisor) startSegment(ctx context.Context, cfg Config) error {
	name := s.opts.Agent.Name
	runID, err := engine.NewRunID()
	if err != nil {
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
	s.runID, s.segment = runID, cfg.Segments+1
	s.outDir = filepath.Join(Dir(s.opts.Home, name), segmentsDir, runID)
	if err := os.MkdirAll(s.outDir, 0o700); err != nil {
		return fmt.Errorf("the segment's folder: %w", err)
	}
	seg := localrun.Segment{
		Builtin: s.opts.Agent.Builtin, Home: s.opts.Home, Policy: s.opts.Policy, PolicySHA: s.opts.PolicySHA,
		PolicyFile: s.opts.PolicyFile, Build: s.opts.Build, OutDir: s.outDir, RunID: runID, Stderr: s.opts.Stderr,
		Persistent: engine.PersistentSegment{
			Agent: name, Segment: s.segment,
			SessionVolume: SessionVolume(name), DataVolume: DataVolume(name),
			ContinueID: continueID,
		},
	}
	start := s.opts.StartSegment
	if start == nil {
		start = func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error) {
			return seg.Start(ctx, s.opts.Docker)
		}
	}
	sess, err := start(ctx, seg)
	if err != nil {
		// A start refused before the engine wrote anything leaves no
		// folder behind; one that wrote its envelope keeps it.
		_ = os.Remove(s.outDir)
		return err
	}
	s.sess = sess
	id, resumed, reason := sess.Conversation()
	if mismatch != "" {
		reason = mismatch
	}
	if !resumed {
		// Whatever kept the conversation from continuing (a harness that
		// cannot, a store that would not load, another harness), the new
		// one is given the last exchange the agent had, if it had one.
		entries, _ := Transcript(s.opts.Home, name, 400)
		if u, a, ok := lastExchange(entries); ok {
			s.carry = carried(u, a)
			reason += "; the runner gave the new conversation the last exchange from the transcript"
		} else if cfg.Segments == 0 {
			reason = "the agent's first segment"
		}
		sess.SetResumeReason(reason)
	}
	s.resumed, s.resumeReason = resumed, reason
	cfg.Segments = s.segment
	cfg.Conversation = nil
	if id != "" {
		cfg.Conversation = &Conversation{Adapter: s.adapter, ID: id}
	}
	if err := SaveConfig(s.opts.Home, name, cfg); err != nil {
		_, _ = sess.End(context.WithoutCancel(ctx), false)
		return err
	}
	text := "continued the conversation"
	if !resumed {
		text = reason
	}
	_ = appendEntry(s.opts.Home, name, Entry{Segment: s.segment, Run: runID, Resumed: &resumed,
		Event: session.Event{Kind: KindSegmentStart, Time: time.Now().UTC(), Text: text}})
	return nil
}

// pump records the segment's events in the transcript, hands them to every
// watcher, and finishes the supervisor when the segment ends.
func (s *Supervisor) pump() {
	defer close(s.pumpDone)
	for ev := range s.sess.Events() {
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
		s.mu.Unlock()
		if kept(ev.Kind) {
			ev.Raw = nil
			_ = appendEntry(s.opts.Home, s.opts.Agent.Name, Entry{Segment: s.segment, Run: s.runID, Event: ev})
		}
		if ev.Kind == session.KindSessionEnd {
			go func() { _, _ = s.Stop(context.Background()) }()
		}
	}
}

// Send sends one turn and returns when the agent has answered it. The first
// turn of a conversation that did not continue starts with the last
// exchange.
func (s *Supervisor) Send(ctx context.Context, text string) error {
	if !s.turn.TryLock() {
		return ErrBusy
	}
	defer s.turn.Unlock()
	s.mu.Lock()
	carry := s.carry
	s.carry = ""
	s.mu.Unlock()
	if carry != "" {
		return s.sess.SendCarrying(ctx, carry, text)
	}
	return s.sess.Send(ctx, text)
}

// Subscribe hands every event from now on to a channel, until cancel or the
// segment's end closes it.
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
}

// StateRunning is a running agent's state.
const StateRunning = "running"

// Status is the agent's state now.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Agent: s.opts.Agent.Name, State: StateRunning, Client: s.opts.Client, PID: os.Getpid(), Since: s.since,
		Segment: s.segment, Run: s.runID, SegmentDir: s.outDir, Adapter: s.adapter, Model: s.model,
		Resumed: s.resumed, ResumeReason: s.resumeReason, Busy: s.busy, Usage: s.usage,
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
		s.env, s.endErr = s.sess.End(ctx, false)
		select {
		case <-s.pumpDone:
		case <-time.After(10 * time.Second):
		}
		if s.env != nil {
			u, _ := LoadUsage(s.opts.Home, s.opts.Agent.Name)
			u.Segments++
			u.InputTokens += s.env.Usage.InputTokens
			u.OutputTokens += s.env.Usage.OutputTokens
			u.TotalTokens += s.env.Usage.TotalTokens
			u.UserTurns += s.env.Usage.UserTurns
			u.ToolCalls += s.env.Usage.Turns
			u.Updated = time.Now().UTC()
			_ = saveUsage(s.opts.Home, s.opts.Agent.Name, u)
		}
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
