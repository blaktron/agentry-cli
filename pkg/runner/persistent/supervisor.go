package persistent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// ErrBusy refuses a turn sent while another is in flight.
var ErrBusy = session.ErrBusy

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
	// OnStage, when set, is called as each segment's start reaches each of
	// its steps (Stage; Stage.Words is how a person is shown it): the
	// agent's first segment, and each a roster change or a restart starts.
	// A server installed in the background while the agent runs is
	// reported too, as its install begins, on that install's goroutine. It
	// is called on the reporting goroutine, so it returns quickly; nil does
	// nothing.
	OnStage func(Stage)
	// Packs opens the knowledge packs the roster has on, for each segment;
	// nil refuses a roster with one on.
	Packs Packs
	// DevHost readies the Coding agent's dev server for each segment; nil
	// readies it under Home and Policy (ReadyDevHost).
	DevHost HostReady
	// Secrets is where the stored secrets' values are read for each
	// segment; nil is the keychain (KeychainSecrets).
	Secrets Secrets
	// AppLink answers the agent's app link calls other than self_configure,
	// which the supervisor answers itself: the desktop's. Nil answers them
	// as Agentry Desktop's.
	AppLink AppLinkHandler
	// StartSegment starts a segment; nil starts it with localrun. Tests
	// put a session of their own here.
	StartSegment func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error)
	// InstallServer installs a server in the background; nil installs it
	// with localrun under Docker and Policy. Tests put an install of their
	// own here.
	InstallServer InstallServer
	// Now is the clock the heartbeat and the time frame read; nil is the
	// computer's. Tick is how often the heartbeat looks; 0 is a minute.
	// Tests set both.
	Now  func() time.Time
	Tick time.Duration
	// CountFacts counts the fact lines of the agent's memory for a
	// compaction's record; nil reads the memory volume with a one-shot.
	// Tests put a count of their own here.
	CountFacts func(context.Context) int
}

// Stage is a step a segment's start has reached (engine.Stage).
type Stage = engine.Stage

// Supervisor owns one running agent: its lock, its segment, its socket.
type Supervisor struct {
	opts    Options
	owner   *Owner
	since   time.Time
	adapter string
	model   string

	turn sync.Mutex // one turn at a time; TryLock refuses a second as busy
	// cfg serialises the supervisor's writes of the agent's config.yaml,
	// each a read, a change and a write, so none loses another's.
	cfg sync.Mutex
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
	segEnded bool
	segEnv   *receipt.Envelope
	segErr   error
	pumpDone chan struct{}
	carry    string // the last exchange, for the first turn of a conversation that did not continue
	// roster is what the running segment was given, and told what the
	// agent was last told it has; byChange says a change started the
	// segment, so its next turn says what changed since told.
	roster       Roster
	told         Roster
	byChange     bool
	persona      string // personaKey of what the conversation has been given
	resumed      bool
	resumeReason string
	busy         bool
	usage        receipt.Usage
	watchers     map[int]chan session.Event
	nextWatcher  int
	ln           net.Listener
	appLn        net.Listener // the app link's socket, for an agent that has one
	appCtx       context.Context
	appCancel    context.CancelFunc
	stopped      bool // set under mu once Stop has let the watchers go
	// served are the tools the running segment's gateway serves; joined
	// are the servers whose background install finished since the agent
	// was last told, and joinTools the tools the segments they joined
	// brought, which the next turn tells it.
	served    []string
	joined    map[string]bool
	joinTools []string
	// installs are the background installs under way, by roster id;
	// bgCtx ends them all at the stop, which waits for installWG.
	installs  map[string]*bgInstall
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	installWG sync.WaitGroup
	// osvRefreshing says OSV's databases are being brought up to date for
	// the Coding agent's dependency scan, in the background like an install.
	osvRefreshing bool

	// tasksMu serialises the supervisor's writes of the agent's tasks.
	// hbCancel and hbDone end and wait for the heartbeat; missed names the
	// tasks that came due while the agent was stopped, for its next turn;
	// taskTurn is the task whose turn was sent, until its user turn
	// takes it into the transcript.
	tasksMu  sync.Mutex
	hbCancel context.CancelFunc
	hbDone   chan struct{}
	missed   string
	taskTurn string
	// taskText is the text the task's turn was sent with, as upkeepText is
	// the upkeep's: a turn of the person's still on its way through the
	// events when one is sent never takes its mark.
	taskText string
	// ranUntil is when each task's last turn ended: a time of the task's
	// that came before it is skipped.
	ranUntil map[string]time.Time

	// upkeep is what memory upkeep decides by (upkeep.json), and
	// upkeepDirty says it changed since last saved. upkeeping is set while
	// an upkeep turn runs, so a person's message waits for it, and
	// upkeepTurn until its user turn takes the mark into the transcript.
	// handoff is the handoff the next segment's new conversation opens
	// with.
	upkeep      upkeepState
	upkeepDirty bool
	upkeepSave  sync.Mutex // one write of upkeep.json at a time
	upkeeping   bool
	upkeepTurn  bool
	handoff     string

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
	if pending := Unconsented(BuildRosterUnder(a, cfg, nil, opts.Policy.MalwareCheckOn()), cfg); len(pending) > 0 {
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
		joined:   map[string]bool{}, installs: map[string]*bgInstall{}, ranUntil: map[string]time.Time{},
		done: make(chan struct{}),
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	if opts.Policy != nil {
		s.model = opts.Policy.Model.Provider + "/" + opts.Policy.Model.Name
	}
	if a.Builtin.KeepsMemory() {
		s.upkeep = loadUpkeep(opts.Home, a.Name)
	}
	if err := s.listenAppLink(); err != nil {
		owner.Release()
		return nil, err
	}
	s.seg.Lock()
	err = s.startSegment(ctx, false)
	s.seg.Unlock()
	if err != nil {
		s.bgCancel()
		s.closeAppLink()
		owner.Release()
		return nil, err
	}
	if err := s.listen(); err != nil {
		_, _ = s.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	// The servers the store does not hold were left out of the segment:
	// they install now, and join at the turn after they are ready.
	s.startInstalls()
	s.startHeartbeat()
	return s, nil
}

// startSegment starts the agent's next segment from its config as it is,
// records how its conversation began and starts its pump; byChange says a
// change to the agent started it. The caller holds seg.
func (s *Supervisor) startSegment(ctx context.Context, byChange bool) error {
	a, home := s.opts.Agent, s.opts.Home
	name := a.Name
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return err
	}
	roster := BuildRosterUnder(a, cfg, nil, s.opts.Policy.MalwareCheckOn())
	// Every segment is held to the person's agreement, not only the first:
	// a config.yaml changed some other way does not widen the next one.
	if pending := Unconsented(roster, cfg); len(pending) > 0 {
		return &ConsentError{Agent: name, Entries: pending}
	}
	ready := s.opts.DevHost
	if ready == nil {
		ready = ReadyDevHost(home, s.opts.Policy)
	}
	keys := s.opts.Secrets
	if keys == nil {
		keys = KeychainSecrets{}
	}
	grants, applied, packsDone, err := Apply(ctx, home, a, cfg, roster, s.opts.Packs, ready, keys)
	if err != nil {
		return err
	}
	if c := grants.Coder; c != nil && c.DependencyScan {
		s.refreshDependencyDB()
	}
	// A server Apply left out is not given, and the roster this segment's
	// envelope records and its frames are told from says so.
	given := func(id string) bool { return slices.Contains(applied.On, id) }
	roster = leftOut(roster, cfg, given)
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
	// The share's mirror is kept only while a server of a checked share
	// sees it: otherwise no copy of the person's files is kept.
	var mirror string
	if len(grants.ShareServers) > 0 && a.Builtin.ChecksShare() {
		mirror = shareMirror(home, name)
	} else if err := removeShareMirror(home, name); err != nil {
		packsDone()
		return err
	}
	// A home that has already run this agent may have its conversation on
	// the legacy volumes, which every home could use: the start copies them
	// into the home's own. A home that has not run it is given none of
	// another's.
	var legacySession, legacyData string
	if cfg.Segments > 0 {
		legacySession, legacyData = SessionVolume(name), DataVolume(name)
	}
	memory, err := memorySegment(home, a, time.Now())
	if err != nil {
		packsDone()
		return err
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
		OnStage: s.opts.OnStage,
		Persistent: engine.PersistentSegment{
			Agent: name, Segment: segment,
			SessionVolume: SessionVolumeIn(home, name), DataVolume: DataVolumeIn(home, name),
			Home: HomeID(home), LegacySessionVolume: legacySession, LegacyDataVolume: legacyData,
			ContinueID: continueID, Roster: roster.Receipt(),
			ShareVerdicts: filepath.Join(Dir(home, name), shareVerdictsFile),
			ShareText:     filepath.Join(Dir(home, name), shareTextDir),
			ShareMirror:   mirror,
			Memory:        memory,
		},
		Grants: grants,
	}
	if s.appLn != nil {
		seg.Grants.AppLink = AppSocketPath(home, name)
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
		return withServerFix(a, cfg, err)
	}
	// Unrecorded, the next start seeds again, which leaves a seeded memory
	// as it is.
	_ = memoryStarted(home, name, memory)
	id, resumed, reason := sess.Conversation()
	if mismatch != "" {
		reason = mismatch
	}
	carry := ""
	s.mu.Lock()
	handoff := s.handoff
	s.handoff = ""
	s.mu.Unlock()
	if !resumed && handoff != "" {
		// A compaction ended the conversation: the new one opens with the
		// handoff the agent wrote for it.
		carry = handoffCarried(handoff)
		reason = "a compaction: the new conversation opens with the agent's handoff"
		sess.SetResumeReason(reason)
	} else if !resumed {
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
	if a.Builtin.Codes() && cfg.Segments > 0 && !byChange {
		// The conversation goes on, and the programs it started in the
		// background did not: they ran in the segment before. A restart
		// for a change says so in the change's own frame.
		carry += "(Agentry started you again since your last turn. " + backgroundEnded + ")\n\n"
	}
	persona := cfg.PersonaGiven
	if !resumed {
		// A new conversation has been given nothing yet.
		persona = ""
	}
	// Only the segment's own fields are written, onto the config as it is
	// now: a change made while the segment started is kept, and is pending.
	if err := s.updateConfig(func(c *Config) error {
		c.Segments = segment
		c.Conversation = nil
		if id != "" {
			c.Conversation = &Conversation{Adapter: s.adapter, ID: id}
		}
		c.PersonaGiven = persona
		return nil
	}); err != nil {
		_, _ = sess.End(context.WithoutCancel(ctx), false)
		packsDone()
		return err
	}
	ending := &atomic.Bool{}
	pumpDone := make(chan struct{})
	// The segment's key counts the servers it was given, as Apply decided:
	// one whose install finished since is a change the next turn applies.
	key := grantsKeyWith(a, cfg, func(srv manifest.MCPServer) bool { return given(ServerEntryID(srv)) })
	served := servedTools(sess)
	s.mu.Lock()
	if byChange && len(s.joined) > 0 {
		s.joinTools = append(s.joinTools, newTools(s.served, served)...)
	}
	s.served = served
	s.sess, s.runID, s.outDir, s.segment = sess, runID, outDir, segment
	s.applied, s.grants, s.packsDone, s.ending, s.pumpDone = applied, key, packsDone, ending, pumpDone
	s.carry, s.persona, s.resumed, s.resumeReason = carry, persona, resumed, reason
	if !resumed {
		// A new conversation: what upkeep knew of the last one is gone with
		// it.
		s.upkeep.ContextTokens, s.upkeep.TurnsSince = 0, 0
		s.upkeepDirty = true
	}
	s.roster, s.byChange = roster, byChange
	if !byChange {
		s.told = roster
	}
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
				// The heartbeat's turn takes the mark it was sent with, on
				// the event every watcher is handed and the transcript keeps.
				if s.taskTurn != "" && ev.Text == s.taskText {
					ev.Task, s.taskTurn = s.taskTurn, ""
				}
				// An upkeep turn is the runner's, and says so likewise.
				if s.upkeepTurn && ev.Text == upkeepText {
					ev.Upkeep, s.upkeepTurn = true, false
				}
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
		s.noteUpkeepEvent(ev)
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
// exchange; the first turn after a change says what it switched on and off
// since the agent was last told, since the conversation the agent continues
// was had with what it had before; and a turn after the person named the
// agent or changed its personality starts with them. An agent that keeps
// time is told the time at every turn, and, at its first turn after a start,
// the tasks that came due while it was stopped.
func (s *Supervisor) Send(ctx context.Context, text string) error {
	if !s.turn.TryLock() {
		s.mu.Lock()
		upkeeping := s.upkeeping
		s.mu.Unlock()
		if !upkeeping {
			return ErrBusy
		}
		// A message that arrives during memory upkeep waits for it, at
		// most as long as an upkeep turn may run, and goes to the new
		// conversation.
		s.notice("Saving to memory first; your message follows")
		wctx, cancel := context.WithTimeout(ctx, upkeepTurnCap+time.Minute)
		ok := s.waitTurn(wctx)
		cancel()
		if !ok {
			return ErrBusy
		}
	}
	defer s.turn.Unlock()
	if trigger := s.upkeepDue(); trigger != "" {
		// The last turn filled most of the model's window, or the person
		// comes back to a large conversation the heartbeat has not yet
		// compacted (a start): it is compacted before their message.
		_, _ = s.compactLocked(ctx, trigger)
	}
	return s.sendLocked(ctx, "", text)
}

// inUseDue says an in-use compaction is due, for an agent that keeps a
// memory.
func (s *Supervisor) inUseDue() bool {
	return s.upkeepDue() == UpkeepInUse
}

// upkeepDue is the compaction due now, if any, for an agent that keeps a
// memory: in use first, then idle.
func (s *Supervisor) upkeepDue() string {
	if !s.opts.Agent.Builtin.KeepsMemory() {
		return ""
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.upkeep.inUseDue(now):
		return UpkeepInUse
	case s.upkeep.idleDue(now):
		return UpkeepIdle
	}
	return ""
}

// sendLocked is Send for a caller that holds turn; pre is a frame of the
// caller's own, last before the text (a task's).
func (s *Supervisor) sendLocked(ctx context.Context, pre, text string) error {
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	stale := grantsKey(s.opts.Home, s.opts.Agent, cfg) != s.grants
	s.mu.Unlock()
	if stale {
		if err := s.nextSegment(ctx); err != nil {
			return err
		}
		if cfg, err = LoadConfig(s.opts.Home, s.opts.Agent.Name); err != nil {
			return err
		}
	}
	s.mu.Lock()
	sess, carry, given := s.sess, s.carry, s.persona
	byChange, roster := s.byChange, s.roster
	joined := maps.Clone(s.joined)
	missed := s.missed
	frame := ""
	if s.opts.Agent.Builtin.KeepsTime() {
		frame = timeFrame(s.now())
	}
	frame += carry + missed
	if byChange {
		change := changeFrame(s.told, roster, s.resumed, joined)
		restarted := ""
		if change == "" && hadOn(s.told, EntryCommands) {
			restarted = backgroundEnded
		}
		frame += change + joinFrame(roster, joined, s.joinTools, restarted)
	}
	s.carry = ""
	s.mu.Unlock()
	failed := failFrame(BuildRoster(s.opts.Agent, cfg, nil), cfg.ServerFailures)
	frame += failed
	persona := personaKey(cfg)
	if persona != given {
		frame += personaFrame(cfg)
	}
	frame += pre
	if frame != "" {
		err = sess.SendCarrying(ctx, frame, text)
	} else {
		err = sess.Send(ctx, text)
	}
	if err == nil && byChange {
		s.mu.Lock()
		if s.sess == sess {
			s.told, s.byChange, s.joinTools = roster, false, nil
			for _, e := range roster {
				if joined[e.ID] && e.State == StateOn {
					delete(s.joined, e.ID)
				}
			}
		}
		s.mu.Unlock()
	}
	if err == nil && missed != "" {
		s.mu.Lock()
		if s.missed == missed {
			s.missed = ""
		}
		s.mu.Unlock()
	}
	if err == nil && failed != "" {
		_ = s.updateConfig(func(c *Config) error {
			for id, f := range c.ServerFailures {
				if was, ok := cfg.ServerFailures[id]; ok && was.At.Equal(f.At) {
					f.Told = true
					c.ServerFailures[id] = f
				}
			}
			return nil
		})
	}
	if err == nil && persona != given {
		s.mu.Lock()
		s.persona = persona
		s.mu.Unlock()
		_ = s.updateConfig(func(c *Config) error {
			c.PersonaGiven = persona
			return nil
		})
	}
	return err
}

// nextSegment ends the running segment and starts the next from the config
// as it is now, keeping the watchers attached. A next segment that cannot
// start stops the agent, saying why. The caller holds turn.
func (s *Supervisor) nextSegment(ctx context.Context) error {
	return s.nextSegmentFor(ctx, "Applying your change: the agent restarts with it, and the conversation continues")
}

// nextSegmentFor is nextSegment, telling the watchers why in notice.
func (s *Supervisor) nextSegmentFor(ctx context.Context, notice string) error {
	s.seg.Lock()
	defer s.seg.Unlock()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return session.ErrEnded
	}
	s.broadcast(session.Event{Kind: session.KindNotice, Time: time.Now().UTC(), Text: notice})
	s.mu.Unlock()
	_, _ = s.endSegment(ctx, true)
	err := s.startSegment(ctx, true)
	if err != nil {
		err = s.withoutNewServers(ctx, err)
	}
	if err != nil {
		s.mu.Lock()
		s.broadcast(session.Event{Kind: session.KindError, Time: time.Now().UTC(), Text: "the change could not be applied, so the agent stopped: " + err.Error()})
		s.mu.Unlock()
		go func() { _, _ = s.Stop(context.Background()) }()
		return err
	}
	return nil
}

// withoutNewServers starts the next segment again without the servers it
// was to be given that the running one was not (a server whose install
// finished, or one the person switched on), when the segment that had them
// could not start (err): each is failed with that reason, and the agent
// runs on without them. When that start fails too, the servers were not
// why, so their failures are taken back and its error is returned. The
// caller holds seg.
func (s *Supervisor) withoutNewServers(ctx context.Context, err error) error {
	a, home := s.opts.Agent, s.opts.Home
	cfg, lerr := LoadConfig(home, a.Name)
	if lerr != nil {
		return err
	}
	s.mu.Lock()
	had, joined := slices.Clone(s.applied.On), maps.Clone(s.joined)
	s.mu.Unlock()
	r := BuildRoster(a, cfg, nil)
	var added []manifest.MCPServer
	for _, as := range agentServers(a, cfg) {
		srv, id := as.Server, as.ID()
		if e, ok := r.Entry(id); ok && e.On && !slices.Contains(had, id) && serverReady(home, a, cfg, srv) {
			added = append(added, srv)
		}
	}
	if len(added) == 0 {
		return err
	}
	reason := failureReason("the agent could not start with it: " + err.Error())
	var failed []manifest.MCPServer
	for _, srv := range added {
		if s.recordFailure(ServerEntryID(srv), reason) {
			failed = append(failed, srv)
		}
	}
	if len(failed) == 0 {
		return err
	}
	again := s.startSegment(ctx, true)
	if again == nil {
		for _, srv := range failed {
			s.notice("The MCP server " + serverName(srv) + " could not start, and the agent keeps running without it: " + reason + ". Switch it off and on to try again.")
		}
		return nil
	}
	_ = s.updateConfig(func(c *Config) error {
		for _, srv := range failed {
			delete(c.ServerFailures, ServerEntryID(srv))
		}
		return nil
	})
	s.mu.Lock()
	for _, srv := range failed {
		if id := ServerEntryID(srv); joined[id] {
			s.joined[id] = true
		}
	}
	s.mu.Unlock()
	return again
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
	err := s.updateConfig(func(c *Config) error {
		_, err := applyChange(s.opts.Home, s.opts.Agent, c, ch, s.opts.Policy.MalwareCheckOn())
		return err
	})
	if err != nil {
		return nil, err
	}
	// A server the change switched on that the store does not hold
	// installs now, and an install whose server it switched off or removed
	// is cancelled.
	s.startInstalls()
	return s.Roster(), nil
}

// updateConfig reads the agent's config, changes it and writes it, one
// change at a time.
func (s *Supervisor) updateConfig(change func(*Config) error) error {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	c, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return err
	}
	if err := change(&c); err != nil {
		return err
	}
	return SaveConfig(s.opts.Home, s.opts.Agent.Name, c)
}

// Roster is the running agent's roster: an entry switched since the segment
// started is pending.
func (s *Supervisor) Roster() Roster {
	cfg, _ := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	r := BuildRosterUnder(s.opts.Agent, cfg, &applied, s.opts.Policy.MalwareCheckOn())
	// A server that is on and that the segment was not given is being
	// installed, or is installed and joins at the next turn; one that
	// waits for what the person must set up keeps saying so.
	installing := s.installing()
	byID := map[string]manifest.MCPServer{}
	for _, as := range agentServers(s.opts.Agent, cfg) {
		byID[as.ID()] = as.Server
	}
	for i, e := range r {
		if e.Kind != KindServer || !e.On || e.State == StateFailed || e.State == StateNeedsSetUp || slices.Contains(applied.On, e.ID) {
			continue
		}
		r[i].State = StatePending
		if installing[e.ID] || !serverReady(s.opts.Home, s.opts.Agent, cfg, byID[e.ID]) {
			r[i].State = StateInstalling
		}
	}
	return r
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
	pending := grantsKey(s.opts.Home, s.opts.Agent, cfg)
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
		// The background installs are cancelled first and take down their
		// containers while the segment ends; the lock is let go only once
		// they have, so the next start of the agent finds none of them.
		// The heartbeat fires nothing more; a task's turn in flight ends
		// with the segment.
		s.cancelInstalls()
		s.cancelHeartbeat()
		s.seg.Lock()
		s.env, s.endErr = s.endSegment(ctx, false)
		s.seg.Unlock()
		s.stopHeartbeat()
		s.closeAppLink()
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
		s.waitInstalls()
		s.owner.Release()
		close(s.done)
	})
	return s.env, s.endErr
}
