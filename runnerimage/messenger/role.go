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

// DroppedStale is a message older than the role passes on.
const DroppedStale = "stale"

// errNoStart refuses a link whose first line is not its start.
var errNoStart = errors.New("the messenger link's first line was not its start")

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
	if start.Kind != KindTelegram {
		return fmt.Errorf("this role does not carry %q", start.Kind)
	}
	if start.Token == "" {
		return errors.New("the start carries no credential")
	}
	api := cfg.API
	if api == "" {
		api = TelegramAPI
	}
	tg, err := newTelegram(api, start.Token, cfg.Proxy)
	if err != nil {
		return err
	}
	r := &role{tg: tg, w: NewWriter(out), log: logw, peer: start.Peer, started: time.Now()}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	me, err := r.connect(ctx)
	if err != nil {
		return err
	}
	name := me.FirstName
	if me.Username != "" {
		name = "@" + me.Username
	}
	if err := r.w.Write(FromRole{Ready: &Ready{Account: id(me.ID), Name: name}}); err != nil {
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
	r.poll(ctx)
	return nil
}

// role is one account's connection.
type role struct {
	tg      *telegram
	w       *Writer
	log     io.Writer
	started time.Time

	mu        sync.Mutex
	peer      *Peer
	pairings  []time.Time
	connected *bool
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

// connect asks the service who the bot is, until it answers: a refused
// token ends the role, anything else is tried again with backoff.
func (r *role) connect(ctx context.Context) (telegramUser, error) {
	wait := minBackoff
	for {
		me, err := r.tg.getMe(ctx)
		if err == nil {
			r.state(true, "")
			return me, nil
		}
		if errors.Is(err, errTokenRefused) {
			r.state(false, err.Error())
			return telegramUser{}, err
		}
		r.state(false, err.Error())
		if !sleep(ctx, max(wait, retryAfter(err))) {
			return telegramUser{}, ctx.Err()
		}
		wait = min(wait*2, maxBackoff)
	}
}

// poll long-polls the service for messages until ctx ends, reconnecting
// with backoff. The relay never closes a connection, so a poll that outlives
// its own deadline is given up on here.
func (r *role) poll(ctx context.Context) {
	var offset int64
	wait := minBackoff
	for ctx.Err() == nil {
		us, err := r.tg.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errTokenRefused) {
				// Revoked while it ran: nothing will connect until it is
				// set again.
				r.state(false, err.Error())
				return
			}
			why := err.Error()
			var te *TelegramError
			if errors.As(err, &te) && te.Code == 409 {
				why = "another program is reading this bot's messages (a second copy of this agent, or a webhook set on the bot): " + te.Description
			}
			r.state(false, why)
			r.logf("%s", why)
			if !sleep(ctx, max(wait, retryAfter(err))) {
				return
			}
			wait = min(wait*2, maxBackoff)
			continue
		}
		wait = minBackoff
		r.state(true, "")
		for _, u := range us {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				r.pass(*u.Message)
			}
		}
	}
}

// pass judges one message: the paired person's goes on, a pairing code from
// a direct message goes on for the supervisor to judge, and every other
// message is dropped, with its reason alone.
func (r *role) pass(m telegramMessage) {
	drop := func(reason string) { _ = r.w.Write(FromRole{Dropped: &Dropped{Reason: reason}}) }
	if m.Chat.Type != "private" || m.From == nil || m.From.IsBot {
		drop(DroppedNotDirect)
		return
	}
	if m.Date > 0 && time.Unix(m.Date, 0).Before(r.started.Add(-staleAfter)) {
		drop(DroppedStale)
		return
	}
	if m.Text == "" {
		drop(DroppedNoText)
		return
	}
	chat, sender := id(m.Chat.ID), id(m.From.ID)
	r.mu.Lock()
	peer := r.peer
	r.mu.Unlock()
	if peer != nil && peer.Sender == sender && peer.Chat == chat {
		if cmd := strings.Fields(m.Text); len(cmd) > 0 && (cmd[0] == "/start" || strings.HasPrefix(cmd[0], "/start@")) {
			drop(DroppedCommand)
			return
		}
		text, cut := m.Text, false
		if len(text) > MaxInboundText {
			text, cut = strings.ToValidUTF8(text[:MaxInboundText], ""), true
		}
		_ = r.w.Write(FromRole{Inbound: &Inbound{ID: m.MessageID, Chat: chat, Sender: sender, Text: text, Cut: cut}})
		return
	}
	code, ok := NormalizeCode(m.Text)
	if !ok {
		drop(DroppedUnpaired)
		return
	}
	if !r.pairingAllowed() {
		drop(DroppedPairings)
		return
	}
	_ = r.w.Write(FromRole{Pairing: &Pairing{Chat: chat, Sender: sender, Code: code}})
}

// pairingAllowed counts a pairing code against the minute's bound.
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
	r.mu.Lock()
	peer := r.peer
	r.mu.Unlock()
	switch {
	case f.Paired != nil:
		r.mu.Lock()
		r.peer = f.Paired.Peer
		r.mu.Unlock()
	case f.Typing != nil:
		if peer != nil && f.Typing.Chat == peer.Chat {
			if err := r.tg.sendTyping(ctx, peer.Chat); err != nil {
				r.logf("typing: %s", err)
			}
		}
	case f.Reply != nil:
		_ = r.w.Write(FromRole{Sent: r.reply(ctx, peer, *f.Reply)})
	}
}

// reply sends an answer to the paired person's chat as Telegram's HTML,
// part by part, each part plain where its HTML is refused.
func (r *role) reply(ctx context.Context, peer *Peer, rep Reply) *Sent {
	if peer == nil || rep.Chat != peer.Chat {
		return &Sent{ID: rep.ID, Error: "not the paired person's chat"}
	}
	parts, cut := Split(rep.Text, maxMessage(KindTelegram))
	sent := 0
	for _, p := range parts {
		err := r.tg.sendMessage(ctx, peer.Chat, p.HTML, true)
		if unparsable(err) {
			err = r.tg.sendMessage(ctx, peer.Chat, p.Plain, false)
		}
		if d := retryAfter(err); d > 0 && d <= maxBackoff && sleep(ctx, d) {
			err = r.tg.sendMessage(ctx, peer.Chat, p.HTML, true)
		}
		if err != nil {
			return &Sent{ID: rep.ID, Parts: sent, Cut: cut, Error: err.Error()}
		}
		sent++
	}
	return &Sent{ID: rep.ID, Parts: sent, Cut: cut}
}

func maxMessage(kind string) int {
	k, _ := Lookup(kind)
	return k.MaxMessage
}

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
