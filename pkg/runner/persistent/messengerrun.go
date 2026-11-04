package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

// MessengerConn is a running messenger role as the supervisor holds it
// (engine.MessengerLink).
type MessengerConn interface {
	Ready() messenger.Ready
	Frames() <-chan messenger.FromRole
	Send(messenger.ToRole) error
	Close(context.Context)
}

// MessengerStart is what starting a messenger role needs.
type MessengerStart struct {
	Kind  messenger.Kind
	Token string
	// Server is the account's own server, where its kind has one.
	Server string
	Peer   *messenger.Peer
	// Dir is a folder of the account's own; Log gets the role's errors.
	Dir string
	Log io.Writer
}

// The bounds of a messenger: the messages that wait for a turn, the
// messages sent an hour, and the wait between attempts to connect.
const (
	MaxMessengerQueue      = 20
	MaxMessengerSentAnHour = 60
	messengerRetryMin      = 5 * time.Second
	messengerRetryMax      = 5 * time.Minute
	messengerTypingEvery   = 4 * time.Second
)

// maxMessengerReason bounds a receipt line's reason.
const maxMessengerReason = 200

// messengerReceipt is the segment's record of its messengers' messages.
const messengerReceipt = "messengers.ndjson"

// The directions a messenger receipt records: a message that came in, one
// that went out, and none for a messenger that did not run.
const (
	dirIn   = "in"
	dirOut  = "out"
	dirNone = "none"
)

// The outcomes a messenger receipt records.
const (
	msgAccepted       = "accepted"
	msgDropped        = "dropped"
	msgRefused        = "refused"
	msgPaired         = "paired"
	msgPairingRefused = "pairing-refused"
	msgSent           = "sent"
	msgNotSent        = "not-sent"
	msgCapped         = "capped"
	msgStopped        = "stopped"
)

// messengerHub is the supervisor's messengers: a role for each account that
// is on, the messages waiting for a turn, and the kind whose messages the
// turn in flight answers.
type messengerHub struct {
	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	roles  map[string]*messengerRole
	queue  []queuedMessage
	turn   string
	nextID uint64
}

// messengerRole is one account's role and its state.
type messengerRole struct {
	kind   messenger.Kind
	set    time.Time
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	conn  MessengerConn
	token string
	peer  *messenger.Peer
	state string
	sent  []time.Time
	// lastID is the newest message id the paired person's chat passed on.
	lastID int64
}

type queuedMessage struct {
	kind, chat, text string
	cut              bool
}

func (r *messengerRole) setState(state string) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

func (r *messengerRole) current() (MessengerConn, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn, r.token
}

// startMessengers starts the hub, a role for each messenger that is on, and
// the turn that takes their messages.
func (s *Supervisor) startMessengers() {
	ctx, cancel := context.WithCancel(context.Background())
	hub := &messengerHub{ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1), roles: map[string]*messengerRole{}}
	s.mu.Lock()
	s.msgs = hub
	s.mu.Unlock()
	hub.wg.Add(1)
	go func() {
		defer hub.wg.Done()
		s.dispatchMessengers(hub)
	}()
	s.reconcileMessengers()
}

// stopMessengers stops every role and tells the turn that takes their
// messages to stop; the func it returns waits for that turn, which a turn in
// flight holds until the segment ends.
func (s *Supervisor) stopMessengers() (wait func()) {
	s.mu.Lock()
	hub := s.msgs
	s.msgs = nil
	s.mu.Unlock()
	if hub == nil {
		return func() {}
	}
	hub.cancel()
	hub.mu.Lock()
	roles := make([]*messengerRole, 0, len(hub.roles))
	for _, r := range hub.roles {
		roles = append(roles, r)
	}
	hub.roles = map[string]*messengerRole{}
	hub.mu.Unlock()
	for _, r := range roles {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
		}
	}
	return hub.wg.Wait
}

// messengerWanted is the agent's messenger accounts that should run now:
// on, agreed to as their sentences read, and allowed by the policy; and why
// each of the others does not.
func (s *Supervisor) messengerWanted(cfg Config) (map[string]MessengerAccount, map[string]string) {
	want, why := map[string]MessengerAccount{}, map[string]string{}
	a := s.opts.Agent
	if !a.TakesMessengers() {
		return want, why
	}
	r := BuildRosterUnder(a, cfg, nil, s.opts.Policy.MalwareCheckOn())
	for _, m := range cfg.Messengers {
		e, ok := r.Entry(MessengerEntryID(m.Kind))
		switch {
		case !ok:
			continue
		case !e.On:
			continue
		case !e.agreed(cfg.Consented[e.ID]):
			why[m.Kind] = "it is on in config.yaml, and you have not agreed to it on this computer: switch it off and on again"
			continue
		}
		if ok, reason := s.opts.Policy.MessengerAllowed(m.Kind); !ok {
			why[m.Kind] = reason
			continue
		}
		want[m.Kind] = m
	}
	return want, why
}

// reconcileMessengers starts the role of each messenger that should run and
// has none, starts again the one whose credential was set again, stops the
// one that should not run, and tells a role whose pairing changed.
func (s *Supervisor) reconcileMessengers() {
	hub := s.hub()
	if hub == nil {
		return
	}
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return
	}
	want, refused := s.messengerWanted(cfg)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.ctx.Err() != nil {
		return
	}
	for kind, r := range hub.roles {
		if m, ok := want[kind]; !ok || !m.Set.Equal(r.set) {
			r.cancel()
			delete(hub.roles, kind)
			// Its waiting messages go with it: they were sent to an
			// account the agent no longer answers on.
			if _, on := want[kind]; !on {
				hub.dropQueued(kind)
			}
		}
	}
	for kind, m := range want {
		if r, ok := hub.roles[kind]; ok {
			s.tellPeer(r, peerOf(m.Peer))
			continue
		}
		k, _ := messenger.Lookup(kind)
		ctx, cancel := context.WithCancel(hub.ctx)
		r := &messengerRole{kind: k, set: m.Set, cancel: cancel, done: make(chan struct{}), state: MessengerConnecting, peer: peerOf(m.Peer)}
		hub.roles[kind] = r
		go s.runMessengerRole(ctx, hub, r, m)
	}
	for kind, why := range refused {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirNone, Outcome: msgRefused, Reason: why})
	}
}

// tellPeer tells a running role who is paired, when that changed.
func (s *Supervisor) tellPeer(r *messengerRole, peer *messenger.Peer) {
	r.mu.Lock()
	same := (r.peer == nil) == (peer == nil) && (peer == nil || *r.peer == *peer)
	r.peer = peer
	conn := r.conn
	r.mu.Unlock()
	if !same && conn != nil {
		_ = conn.Send(messenger.ToRole{Paired: &messenger.Paired{Peer: peer}})
	}
}

func peerOf(p *MessengerPeer) *messenger.Peer {
	if p == nil {
		return nil
	}
	return &messenger.Peer{Sender: p.Sender, Chat: p.Chat}
}

// errMessengerCredential says a messenger's credential could not be read.
var errMessengerCredential = errors.New("its credential could not be read from where it is kept")

// runMessengerRole keeps one account's role running until ctx ends: it
// starts it, takes its frames, and when it ends starts it again, waiting
// longer each time it fails, up to messengerRetryMax. A credential the
// service refuses is not tried again until it is set again.
func (s *Supervisor) runMessengerRole(ctx context.Context, hub *messengerHub, r *messengerRole, m MessengerAccount) {
	defer close(r.done)
	a := s.opts.Agent
	keys := s.opts.Secrets
	if keys == nil {
		keys = KeychainSecrets{}
	}
	dir := filepath.Join(Dir(s.opts.Home, a.Name), "messengers", r.kind.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.setState(err.Error())
		return
	}
	logf, err := os.OpenFile(filepath.Join(dir, "role.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the agent's own folder
	if err != nil {
		r.setState(err.Error())
		return
	}
	defer func() { _ = logf.Close() }()
	wait := messengerRetryMin
	for ctx.Err() == nil {
		token, err := keys.Get(MessengerKeychainAccount(a.Name, r.kind.Name))
		if err != nil || token == "" {
			r.setState(fmt.Sprintf("%v: add it again (agentry agent messenger add %s %s)", errMessengerCredential, a.Name, r.kind.Name))
			return
		}
		r.setState(MessengerConnecting)
		r.mu.Lock()
		peer := r.peer
		r.mu.Unlock()
		conn, err := s.startMessengerConn(ctx, MessengerStart{Kind: r.kind, Token: token, Server: m.Server, Peer: peer, Dir: dir, Log: logf})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			why := maskValues(err.Error(), messenger.CredentialParts(r.kind.Name, token))
			r.setState(why)
			if strings.Contains(why, messenger.TokenRefused) {
				return
			}
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = min(wait*2, messengerRetryMax)
			continue
		}
		if got := conn.Ready().Account; got != m.Account {
			conn.Close(context.WithoutCancel(ctx))
			r.setState(fmt.Sprintf("the credential kept is for another %s account (%s, not %s): add it again", r.kind.Account, got, m.Account))
			return
		}
		wait = messengerRetryMin
		r.mu.Lock()
		r.conn, r.token, r.state = conn, token, MessengerConnected
		// A pairing made while the role was starting reaches it now.
		if r.peer != peer {
			_ = conn.Send(messenger.ToRole{Paired: &messenger.Paired{Peer: r.peer}})
		}
		r.mu.Unlock()
		stop := context.AfterFunc(ctx, func() { conn.Close(context.Background()) })
		for f := range conn.Frames() {
			s.messengerFrame(ctx, hub, r, f)
		}
		stop()
		conn.Close(context.WithoutCancel(ctx))
		r.mu.Lock()
		r.conn, r.token = nil, ""
		r.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		r.setState("the connection ended; connecting again")
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// startMessengerConn starts a role: the client's, or the engine's with the
// runner image under the agent's policy.
func (s *Supervisor) startMessengerConn(ctx context.Context, st MessengerStart) (MessengerConn, error) {
	if s.opts.StartMessenger != nil {
		return s.opts.StartMessenger(ctx, st)
	}
	if s.opts.Docker == nil || s.opts.Policy == nil {
		return nil, errors.New("this client starts no messenger")
	}
	l, err := engine.StartMessenger(ctx, s.opts.Docker, engine.MessengerParams{
		Kind: st.Kind, Token: st.Token, Server: st.Server, Peer: st.Peer, Dir: st.Dir, Log: st.Log,
		Image: s.opts.Policy.RunnerImageTag(runnerimage.ContentVersion()), Policy: s.opts.Policy,
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// messengerFrame takes one frame from a role. The supervisor judges every
// sender again: a message that is not the paired person's never becomes a
// turn, whatever the role passed on.
func (s *Supervisor) messengerFrame(ctx context.Context, hub *messengerHub, r *messengerRole, f messenger.FromRole) {
	kind := r.kind.Name
	switch {
	case f.Dropped != nil:
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgDropped, Reason: f.Dropped.Reason})
	case f.State != nil:
		state := MessengerConnected
		if !f.State.Connected {
			state = f.State.Error
		}
		r.setState(state)
	case f.Sent != nil:
		r.mu.Lock()
		now := time.Now()
		for range f.Sent.Parts {
			r.sent = append(r.sent, now)
		}
		r.mu.Unlock()
		rec := messengerRecord{Messenger: kind, Direction: dirOut, Outcome: msgSent, Parts: f.Sent.Parts, Cut: f.Sent.Cut}
		if f.Sent.Error != "" {
			rec.Outcome, rec.Reason = msgNotSent, f.Sent.Error
		}
		s.logMessenger(rec)
	case f.Pairing != nil:
		s.messengerPairing(r, *f.Pairing)
	case f.Inbound != nil:
		s.messengerInbound(ctx, hub, r, *f.Inbound)
	}
}

// pairedPeer is the person paired on kind's account, as config.yaml has it.
func (s *Supervisor) pairedPeer(kind string) *MessengerPeer {
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return nil
	}
	if i := findMessenger(cfg.Messengers, kind); i >= 0 {
		return cfg.Messengers[i].Peer
	}
	return nil
}

// messengerPairing pairs the sender of the code the open pairing takes, and
// tells them; any other code is refused without an answer.
func (s *Supervisor) messengerPairing(r *messengerRole, p messenger.Pairing) {
	kind := r.kind.Name
	now := s.now()
	var peer *MessengerPeer
	closed := false
	err := s.updateConfig(func(c *Config) error {
		i := findMessenger(c.Messengers, kind)
		if i < 0 || c.Messengers[i].Pairing == nil {
			return errPairingRefused
		}
		if !c.Messengers[i].Pairing.takes(p.Code, now) {
			// A wrong code counts against the pairing, which closes after
			// MaxPairingTries of them.
			c.Messengers[i].Pairing.Tries++
			if c.Messengers[i].Pairing.Tries >= MaxPairingTries || now.After(c.Messengers[i].Pairing.Until) {
				c.Messengers[i].Pairing, closed = nil, true
			}
			return nil
		}
		peer = &MessengerPeer{Sender: p.Sender, Chat: p.Chat, Paired: now.UTC()}
		c.Messengers[i].Peer, c.Messengers[i].Pairing = peer, nil
		return nil
	})
	if err != nil || peer == nil {
		rec := messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgPairingRefused}
		if closed {
			rec.Reason = "the pairing closed"
		}
		s.logMessenger(rec)
		return
	}
	s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgPaired})
	s.tellPeer(r, peerOf(peer))
	cfg, _ := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	s.messengerReply(r, p.Chat, "Paired. This "+r.kind.Title+" account now reaches "+s.opts.Agent.DisplayName(cfg)+", and no other does. "+
		"Write to it here as you would in Agentry. Send /stop to stop what it is doing. Anything it asks your agreement to waits for you in Agentry.")
	s.notice("Paired your " + r.kind.Title + " account: its messages reach the agent from now on")
}

var errPairingRefused = errors.New("the pairing does not take that code")

// stopCommand is what the paired person sends to stop the turn in flight.
const stopCommand = "/stop"

// messengerInbound takes the paired person's message: /stop stops the turn
// in flight; anything else waits for the next turn, at most
// MaxMessengerQueue of them.
func (s *Supervisor) messengerInbound(ctx context.Context, hub *messengerHub, r *messengerRole, in messenger.Inbound) {
	kind := r.kind.Name
	peer := s.pairedPeer(kind)
	if peer == nil || in.Sender != peer.Sender || in.Chat != peer.Chat {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgRefused, Reason: messenger.DroppedUnpaired})
		return
	}
	// A role started again may be given a message it passed on already.
	if in.ID > 0 {
		r.mu.Lock()
		seen := in.ID <= r.lastID
		if !seen {
			r.lastID = in.ID
		}
		r.mu.Unlock()
		if seen {
			s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgDropped, Reason: "seen"})
			return
		}
	}
	if strings.EqualFold(strings.TrimSpace(in.Text), stopCommand) {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgStopped})
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Interrupt(ictx)
		cancel()
		answer := "Stopped."
		if err != nil {
			answer = "Nothing was running."
		}
		s.messengerReply(r, peer.Chat, answer)
		return
	}
	hub.mu.Lock()
	full := len(hub.queue) >= MaxMessengerQueue
	if !full {
		hub.queue = append(hub.queue, queuedMessage{kind: kind, chat: peer.Chat, text: in.Text, cut: in.Cut})
	}
	hub.mu.Unlock()
	if full {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgDropped, Reason: "queue-full"})
		s.messengerReply(r, peer.Chat, fmt.Sprintf("%d of your messages are waiting for the agent already, so this one was not taken. Send it again once it answers.", MaxMessengerQueue))
		return
	}
	s.logMessenger(messengerRecord{Messenger: kind, Direction: dirIn, Outcome: msgAccepted, Cut: in.Cut})
	select {
	case hub.kick <- struct{}{}:
	default:
	}
}

// dispatchMessengers sends the waiting messages as turns, one turn at a
// time, until the hub stops.
func (s *Supervisor) dispatchMessengers(hub *messengerHub) {
	for {
		select {
		case <-hub.ctx.Done():
			return
		case <-hub.kick:
		}
		for s.messengerTurn(hub) {
		}
	}
}

// dropQueued takes kind's waiting messages out of the queue. The caller
// holds hub.mu.
func (hub *messengerHub) dropQueued(kind string) {
	kept := hub.queue[:0]
	for _, q := range hub.queue {
		if q.kind != kind {
			kept = append(kept, q)
		}
	}
	hub.queue = kept
}

// takeQueue takes the waiting messages of the first one's messenger.
func (hub *messengerHub) takeQueue() []queuedMessage {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.queue) == 0 {
		return nil
	}
	kind := hub.queue[0].kind
	var take, keep []queuedMessage
	for _, q := range hub.queue {
		if q.kind == kind {
			take = append(take, q)
		} else {
			keep = append(keep, q)
		}
	}
	hub.queue = keep
	return take
}

func (hub *messengerHub) role(kind string) *messengerRole {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.roles[kind]
}

// messengerTurn waits for the turn, then sends the messages waiting as one
// turn and the answer back where they came from; false when none waited.
func (s *Supervisor) messengerTurn(hub *messengerHub) bool {
	if !s.waitTurn(hub.ctx) {
		return false
	}
	// A stop that began while it waited sends nothing more.
	if hub.ctx.Err() != nil {
		s.turn.Unlock()
		return false
	}
	batch := hub.takeQueue()
	if len(batch) == 0 {
		s.turn.Unlock()
		return false
	}
	// The person's turn, from wherever they wrote it.
	s.compactBeforePerson(hub.ctx)
	kind, chat := batch[0].kind, batch[0].chat
	k, _ := messenger.Lookup(kind)
	texts := make([]string, 0, len(batch))
	for _, q := range batch {
		t := q.text
		if q.cut {
			t += "\n[cut: the message was longer than " + strconv.Itoa(messenger.MaxInboundText/1024) + " KiB]"
		}
		texts = append(texts, t)
	}
	text := strings.Join(texts, "\n\n")
	frame := messengerFrameText(k, len(batch))
	events, unsubscribe := s.Subscribe()
	defer unsubscribe()
	s.mu.Lock()
	s.msgTurn, s.msgText = kind, text
	s.mu.Unlock()
	hub.mu.Lock()
	hub.turn = kind
	hub.mu.Unlock()
	stopTyping := s.messengerTyping(hub, kind, chat)
	err := s.sendAs(hub.ctx, frame, text, true)
	stopTyping()
	hub.mu.Lock()
	hub.turn = ""
	hub.mu.Unlock()
	s.mu.Lock()
	if err != nil {
		// No user turn came to take the mark.
		s.msgTurn, s.msgText = "", ""
	}
	s.mu.Unlock()
	s.turn.Unlock()
	r := hub.role(kind)
	if r == nil {
		return true
	}
	if err != nil {
		s.messengerReply(r, chat, "That did not reach the agent: its turn failed. Agentry has the details.")
		return true
	}
	if answer := lastAnswer(events, func(ev session.Event) bool { return ev.Messenger == kind && ev.Text == text }); strings.TrimSpace(answer) != "" {
		s.messengerReply(r, chat, answer)
	}
	return true
}

// messengerFrameText is the frame a messenger's turn opens with.
func messengerFrameText(k messenger.Kind, n int) string {
	from := "(A message from you on " + k.Title + "."
	if n > 1 {
		from = "(" + strconv.Itoa(n) + " messages from you on " + k.Title + ", sent while you were busy, in order."
	}
	return from + " Your answer is sent back there, as " + k.Title + " shows it. Nothing is approved from " + k.Title +
		": anything that needs the person's agreement waits for them in Agentry, so say so if it comes up.)\n\n"
}

// messengerTyping shows the paired person's chat the agent is answering
// until the func it returns is called.
func (s *Supervisor) messengerTyping(hub *messengerHub, kind, chat string) func() {
	ctx, cancel := context.WithCancel(hub.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(messengerTypingEvery)
		defer t.Stop()
		for {
			if r := hub.role(kind); r != nil {
				if conn, _ := r.current(); conn != nil {
					_ = conn.Send(messenger.ToRole{Typing: &messenger.Typing{Chat: chat}})
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// messengerReply sends text to the paired person's chat on r's messenger:
// masked whole first, under the hourly bound, and to that chat alone.
func (s *Supervisor) messengerReply(r *messengerRole, chat, text string) {
	kind := r.kind.Name
	peer := s.pairedPeer(kind)
	if peer == nil || chat != peer.Chat {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirOut, Outcome: msgNotSent, Reason: "not the paired person's chat"})
		return
	}
	conn, token := r.current()
	if conn == nil {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirOut, Outcome: msgNotSent, Reason: "not connected"})
		return
	}
	now := time.Now()
	r.mu.Lock()
	kept := r.sent[:0]
	for _, t := range r.sent {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	r.sent = kept
	// Room for a whole answer, cut at its most messages.
	capped := len(r.sent)+messenger.MaxParts > MaxMessengerSentAnHour
	r.mu.Unlock()
	if capped {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirOut, Outcome: msgCapped, Reason: fmt.Sprintf("%d messages in the last hour", MaxMessengerSentAnHour)})
		return
	}
	masked := maskValues(text, append(s.storedSecretValues(), messenger.CredentialParts(kind, token)...))
	hub := s.hub()
	var id uint64
	if hub != nil {
		hub.mu.Lock()
		hub.nextID++
		id = hub.nextID
		hub.mu.Unlock()
	}
	if err := conn.Send(messenger.ToRole{Reply: &messenger.Reply{ID: id, Chat: chat, Text: masked}}); err != nil {
		s.logMessenger(messengerRecord{Messenger: kind, Direction: dirOut, Outcome: msgNotSent, Reason: "the role did not take it"})
	}
}

// tellMessengerOfQuestion tells the paired person, on the messenger whose
// message the turn in flight answers, that the agent asks for their
// agreement in Agentry, where it is answered (asked says a client is there
// to ask): it is never answered from the messenger.
func (s *Supervisor) tellMessengerOfQuestion(title string, asked bool) {
	hub := s.hub()
	if hub == nil {
		return
	}
	hub.mu.Lock()
	kind := hub.turn
	r := hub.roles[kind]
	hub.mu.Unlock()
	if r == nil {
		return
	}
	peer := s.pairedPeer(kind)
	if peer == nil {
		return
	}
	where := "Answer it there, on this computer"
	if !asked {
		where = "No Agentry window is open to answer it, so it was not asked: open Agentry on this computer and ask again"
	}
	s.messengerReply(r, peer.Chat, "The agent asks for your agreement in Agentry: "+strings.TrimSpace(title)+
		". "+where+". Nothing is approved from "+r.kind.Title+".")
}

// messengerStates are the agent's messengers with each one's connection.
func (s *Supervisor) messengerStates(cfg Config) []MessengerState {
	states := Messengers(s.opts.Agent, cfg, s.opts.Policy.MessengerAllowed)
	hub := s.hub()
	if hub == nil {
		return states
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for i, st := range states {
		if r, ok := hub.roles[st.Kind]; ok {
			r.mu.Lock()
			states[i].Connection = r.state
			r.mu.Unlock()
		}
	}
	return states
}

// messengerRecord is one line of the segment's messenger receipt: never a
// message's text, nor who an unpaired sender was.
type messengerRecord struct {
	Time      time.Time `json:"time"`
	Messenger string    `json:"messenger"`
	// Direction is in, out, or none for a messenger that did not run.
	Direction string `json:"direction"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Parts     int    `json:"parts,omitempty"`
	Cut       bool   `json:"cut,omitempty"`
	Segment   int    `json:"segment"`
	Run       string `json:"run"`
}

// logMessenger appends a line to the running segment's messenger receipt.
func (s *Supervisor) logMessenger(rec messengerRecord) {
	s.mu.Lock()
	out, segment, run := s.outDir, s.segment, s.runID
	s.mu.Unlock()
	if out == "" {
		return
	}
	dir := filepath.Join(out, "receipts")
	rec.Time, rec.Segment, rec.Run = s.now().UTC(), segment, run
	// A service's own error is cut short, so no receipt carries more than
	// its first words.
	rec.Reason = cutText(rec.Reason, maxMessengerReason)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, messengerReceipt), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the segment's own receipt
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	_ = errors.Join(werr, f.Close())
}

// sleepCtx waits d, false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// hub is the supervisor's messengers, nil before they start and after they
// stop.
func (s *Supervisor) hub() *messengerHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs
}
