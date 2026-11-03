package messenger

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Config is how the role reaches its service.
type Config struct {
	// Proxy is the relay's http:// address, the role's only route out.
	Proxy string
	// API is the service's address; empty is its own (a test's server
	// otherwise).
	API string
}

// The pairing code a person sends from their own account: CodeLength
// characters of CodeAlphabet, which leaves out the ones read alike (0 and
// O, 1 and I and L).
const (
	CodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	CodeLength   = 8
)

// NormalizeCode is text as a pairing code, upper case, without the spaces
// and dashes a person may type in it, and ok when it is one.
func NormalizeCode(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > 2*CodeLength {
		return "", false
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(text) {
		switch {
		case r == ' ' || r == '-':
			continue
		case strings.ContainsRune(CodeAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	if b.Len() != CodeLength {
		return "", false
	}
	return b.String(), true
}

// The role's bounds: the pairing codes it passes on a minute, the oldest
// message it passes on when it starts (Telegram keeps a bot's messages for a
// day), and its reconnection's backoff.
const (
	maxPairingsPerMinute = 3
	staleAfter           = time.Hour
	minBackoff           = time.Second
	maxBackoff           = time.Minute
)

// DroppedStale is a message older than the role passes on, and
// DroppedEncrypted one encrypted end to end, which the role cannot read.
const (
	DroppedStale     = "stale"
	DroppedEncrypted = "encrypted"
)

// errNoStart refuses a link whose first line is not its start.
var errNoStart = errors.New("the messenger link's first line was not its start")

// client is one service's connection, as the role drives it: who the
// account is, the messages the service delivers, and the role's answers
// and typing. Each kind has one (telegram.go, discord.go, slack.go,
// matrix.go).
type client interface {
	// connect asks the service who the account is: errTokenRefused when it
	// does not take the credential.
	connect(ctx context.Context) (Ready, error)
	// listen passes each message the service delivers to r.pass, calling
	// r.up while the connection holds, until it fails or ctx ends; it
	// always ends with why.
	listen(ctx context.Context, r *role) error
	// send writes one part of an answer to chat.
	send(ctx context.Context, chat string, p Part) error
	// typing shows chat that the agent is answering, where the service
	// lets an account do that.
	typing(ctx context.Context, chat string) error
}

// message is one message a service delivered, as the role judges it.
type message struct {
	id     int64
	chat   string
	sender string
	// direct says it was written in a direct conversation with the account.
	direct bool
	// own says the account itself wrote it, as a service shows the
	// account its own answers: passed over without a word.
	own bool
	// bot says another bot wrote it.
	bot bool
	at  time.Time
	// text is empty for a message with no text: a sticker, a photo, a
	// voice note.
	text string
	// command says it is the service's own command (Telegram's /start).
	command bool
	// encrypted says it is end-to-end encrypted, which the role cannot read.
	encrypted bool
}

// newClient is kind's client for the credential token, reaching base (the
// service's address; a test server's, or for Matrix the account's
// homeserver) through proxy.
func newClient(kind, base, token, proxy string) (client, error) {
	switch kind {
	case KindTelegram:
		if base == "" {
			base = TelegramAPI
		}
		return newTelegram(base, token, proxy)
	case KindDiscord:
		return newDiscord(base, token, proxy)
	case KindSlack:
		return newSlack(base, token, proxy)
	case KindMatrix:
		return newMatrix(base, token, proxy)
	}
	return nil, fmt.Errorf("this role does not carry %q", kind)
}

// Serve is the role: it reads its start from in, connects to the service
// with the credential it carries, writes ready to out, then passes on the
// paired person's messages and sends the replies it is given until in ends
// or ctx does. logw gets its own errors, never a credential or a message.
func Serve(ctx context.Context, in io.Reader, out io.Writer, logw io.Writer, cfg Config) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), MaxLine)
	if !sc.Scan() {
		return errNoStart
	}
	first, err := ReadToRole(bytes.TrimSpace(sc.Bytes()))
	if err != nil || first.Start == nil {
		return errNoStart
	}
	start := *first.Start
	k, ok := Lookup(start.Kind)
	if !ok {
		return fmt.Errorf("this role does not carry %q", start.Kind)
	}
	if start.Token == "" {
		return errors.New("the start carries no credential")
	}
	base := cfg.API
	if k.NamesServer {
		if _, err := ServerHost(start.Server); err != nil {
			return err
		}
		if base == "" {
			base = start.Server
		}
	}
	c, err := newClient(k.Name, base, start.Token, cfg.Proxy)
	if err != nil {
		return err
	}
	r := &role{kind: k, c: c, w: NewWriter(out), log: logw, peer: start.Peer, started: time.Now(), wait: minBackoff}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready, err := r.connect(ctx)
	if err != nil {
		return err
	}
	if err := r.w.Write(FromRole{Ready: &ready}); err != nil {
		return err
	}
	// The supervisor's lines, until it closes the link: then the role ends.
	go func() {
		defer cancel()
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			f, err := ReadToRole(line)
			if err != nil || f.Start != nil {
				r.logf("a line from the supervisor that is not a reply, typing or pairing was dropped")
				continue
			}
			r.handle(ctx, f)
		}
	}()
	r.run(ctx)
	return nil
}

// role is one account's connection.
type role struct {
	kind    Kind
	c       client
	w       *Writer
	log     io.Writer
	started time.Time

	mu        sync.Mutex
	peer      *Peer
	pairings  []time.Time
	connected *bool
	wait      time.Duration
}

func (r *role) logf(format string, args ...any) {
	if r.log != nil {
		_, _ = fmt.Fprintf(r.log, "messenger: "+format+"\n", args...)
	}
}

// state reports a change of the connection's state, once.
func (r *role) state(connected bool, why string) {
	r.mu.Lock()
	same := r.connected != nil && *r.connected == connected && connected
	r.connected = &connected
	r.mu.Unlock()
	if same {
		return
	}
	_ = r.w.Write(FromRole{State: &State{Connected: connected, Error: why}})
}

// up says the connection holds: connected, and the next failure waits the
// least again.
func (r *role) up() {
	r.mu.Lock()
	r.wait = minBackoff
	r.mu.Unlock()
	r.state(true, "")
}

// currentPeer is the person paired now, if any.
func (r *role) currentPeer() *Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peer
}

// connect asks the service who the account is, until it answers: a refused
// credential ends the role, anything else is tried again with backoff.
func (r *role) connect(ctx context.Context) (Ready, error) {
	wait := minBackoff
	for {
		ready, err := r.c.connect(ctx)
		if err == nil {
			r.state(true, "")
			return ready, nil
		}
		r.state(false, err.Error())
		if errors.Is(err, errTokenRefused) {
			return Ready{}, err
		}
		if !sleep(ctx, max(wait, retryAfter(err))) {
			return Ready{}, ctx.Err()
		}
		wait = min(wait*2, maxBackoff)
	}
}

// run holds the service's connection until ctx ends, opening it again with
// backoff each time it fails. Every client keeps its own deadlines, since
// the relay never closes a connection that went quiet.
func (r *role) run(ctx context.Context) {
	for ctx.Err() == nil {
		err := r.c.listen(ctx, r)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errTokenRefused) {
			// Revoked while it ran: nothing will connect until it is set
			// again.
			r.state(false, err.Error())
			return
		}
		why := "the connection ended"
		if err != nil {
			why = err.Error()
		}
		r.state(false, why)
		r.logf("%s", why)
		r.mu.Lock()
		wait := r.wait
		r.wait = min(r.wait*2, maxBackoff)
		r.mu.Unlock()
		if !sleep(ctx, max(wait, retryAfter(err))) {
			return
		}
	}
}

// pass judges one message: the paired person's goes on, a pairing code from
// a direct message goes on for the supervisor to judge, and every other
// message is dropped, with its reason alone.
func (r *role) pass(m message) {
	if m.own {
		return
	}
	if !m.direct || m.bot || m.sender == "" {
		r.drop(DroppedNotDirect)
		return
	}
	if !m.at.IsZero() && m.at.Before(r.started.Add(-staleAfter)) {
		r.drop(DroppedStale)
		return
	}
	if m.encrypted {
		r.drop(DroppedEncrypted)
		return
	}
	if m.text == "" {
		r.drop(DroppedNoText)
		return
	}
	peer := r.currentPeer()
	if peer != nil && peer.Sender == m.sender && peer.Chat == m.chat {
		if m.command {
			r.drop(DroppedCommand)
			return
		}
		text, cut := m.text, false
		if len(text) > MaxInboundText {
			text, cut = strings.ToValidUTF8(text[:MaxInboundText], ""), true
		}
		_ = r.w.Write(FromRole{Inbound: &Inbound{ID: m.id, Chat: m.chat, Sender: m.sender, Text: text, Cut: cut}})
		return
	}
	code, ok := NormalizeCode(m.text)
	if !ok {
		r.drop(DroppedUnpaired)
		return
	}
	if !r.pairingAllowed() {
		r.drop(DroppedPairings)
		return
	}
	_ = r.w.Write(FromRole{Pairing: &Pairing{Chat: m.chat, Sender: m.sender, Code: code}})
}

// drop reports a message the role did not pass on, by its reason alone.
func (r *role) drop(reason string) { _ = r.w.Write(FromRole{Dropped: &Dropped{Reason: reason}}) }

// pairingAllowed counts a pairing code, or a conversation an unpaired
// sender opens with the account, against the minute's bound.
func (r *role) pairingAllowed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	kept := r.pairings[:0]
	for _, t := range r.pairings {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	r.pairings = kept
	if len(r.pairings) >= maxPairingsPerMinute {
		return false
	}
	r.pairings = append(r.pairings, now)
	return true
}

// handle carries out one of the supervisor's lines. A reply or typing for
// any chat but the paired person's is refused.
func (r *role) handle(ctx context.Context, f ToRole) {
	peer := r.currentPeer()
	switch {
	case f.Paired != nil:
		r.mu.Lock()
		r.peer = f.Paired.Peer
		r.mu.Unlock()
	case f.Typing != nil:
		if peer != nil && f.Typing.Chat == peer.Chat && r.kind.Typing {
			if err := r.c.typing(ctx, peer.Chat); err != nil {
				r.logf("typing: %s", err)
			}
		}
	case f.Reply != nil:
		_ = r.w.Write(FromRole{Sent: r.reply(ctx, peer, *f.Reply)})
	}
}

// reply sends an answer to the paired person's chat in the service's own
// format, part by part.
func (r *role) reply(ctx context.Context, peer *Peer, rep Reply) *Sent {
	if peer == nil || rep.Chat != peer.Chat {
		return &Sent{ID: rep.ID, Error: "not the paired person's chat"}
	}
	parts, cut := SplitAs(r.kind.Format, rep.Text, r.kind.MaxMessage)
	sent := 0
	for _, p := range parts {
		err := r.c.send(ctx, peer.Chat, p)
		if d := retryAfter(err); d > 0 && d <= maxBackoff && sleep(ctx, d) {
			err = r.c.send(ctx, peer.Chat, p)
		}
		if err != nil {
			return &Sent{ID: rep.ID, Parts: sent, Cut: cut, Error: err.Error()}
		}
		sent++
	}
	return &Sent{ID: rep.ID, Parts: sent, Cut: cut}
}

// retryAfterer is a service's refusal that says how long to wait.
type retryAfterer interface{ retryAfter() time.Duration }

// retryAfter is how long the service asked the role to wait, or 0.
func retryAfter(err error) time.Duration {
	var ra retryAfterer
	if errors.As(err, &ra) {
		return ra.retryAfter()
	}
	return 0
}

// ServiceError is a service's refusal of a request.
type ServiceError struct {
	Service string
	Method  string
	Code    int
	// Description is the service's own words, without the credential.
	Description string
	After       time.Duration
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("%s refused %s (%d): %s", e.Service, e.Method, e.Code, e.Description)
}

func (e *ServiceError) retryAfter() time.Duration { return e.After }

// sleep waits d, false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
