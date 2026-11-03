package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Discord, through a bot: its REST API (v10) to learn the bot, answer and
// show typing, and its gateway's WebSocket for the messages sent to it.
// The gateway is asked for direct messages alone (the DIRECT_MESSAGES
// intent), whose text Discord gives a bot without the privileged message
// content intent. Every session is identified afresh, never resumed: a
// resume goes to an address the gateway names, which the relay does not
// reach.

// The Discord API's addresses.
const (
	DiscordAPI     = "https://discord.com/api/v10"
	discordGateway = "wss://gateway.discord.gg/?v=10&encoding=json"
)

// discordDirectMessages is the gateway intent for direct messages.
const discordDirectMessages = 1 << 12

// The gateway's opcodes the role uses.
const (
	discordDispatch       = 0
	discordHeartbeat      = 1
	discordIdentify       = 2
	discordReconnect      = 7
	discordInvalidSession = 9
	discordHello          = 10
	discordHeartbeatAck   = 11
)

// discordSuppressEmbeds keeps a link in an answer from opening a preview.
const discordSuppressEmbeds = 1 << 2

// discord is one bot's client.
type discord struct {
	rest, gateway string
	token         string
	proxy         string
	client        *http.Client
	self          string
}

// newDiscord is a client for the bot whose token is token. base is "" for
// Discord's own addresses, or a test server's http:// address that serves
// the REST API under /api/v10 and the gateway at /gateway.
func newDiscord(base, token, proxy string) (*discord, error) {
	c, err := newHTTPClient(proxy, callTimeout)
	if err != nil {
		return nil, err
	}
	d := &discord{rest: DiscordAPI, gateway: discordGateway, token: token, proxy: proxy, client: c}
	if base != "" {
		base = strings.TrimRight(base, "/")
		d.rest = base + "/api/v10"
		d.gateway = "ws" + strings.TrimPrefix(base, "http") + "/gateway?v=10&encoding=json"
	}
	return d, nil
}

func (d *discord) scrub(text string) string { return scrubber("the bot's token", d.token)(text) }

// errDiscordRefused says Discord does not know the bot's token.
var errDiscordRefused = refused("Discord does not know the bot's token, which was reset or is not a bot token from the developer portal")

// call sends a REST request and decodes its answer into out.
func (d *discord) call(ctx context.Context, method, path string, body, out any, what string) error {
	a, err := doJSON(ctx, d.client, method, d.rest+path, "Bot "+d.token, body, callTimeout, what, d.scrub)
	if err != nil {
		return err
	}
	if a.status == http.StatusUnauthorized {
		return errDiscordRefused
	}
	if a.status/100 != 2 {
		var e struct {
			Message    string  `json:"message"`
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(a.body, &e)
		se := &ServiceError{Service: "Discord", Method: what, Code: a.status, Description: d.scrub(e.Message)}
		if a.status == http.StatusTooManyRequests {
			se.After = max(time.Duration(e.RetryAfter*float64(time.Second)), retryAfterHeader(a.header))
		}
		return se
	}
	if out != nil && len(a.body) > 0 {
		if err := json.Unmarshal(a.body, out); err != nil {
			return fmt.Errorf("%s: Discord's answer could not be read", what)
		}
	}
	return nil
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

func (d *discord) connect(ctx context.Context) (Ready, error) {
	var me discordUser
	if err := d.call(ctx, http.MethodGet, "/users/@me", nil, &me, "users/@me"); err != nil {
		return Ready{}, err
	}
	if me.ID == "" {
		return Ready{}, errors.New("users/@me: Discord named no account")
	}
	if !me.Bot {
		return Ready{}, refused("the token is a person's own Discord account, not a bot's: make a bot in the developer portal")
	}
	d.self = me.ID
	return Ready{Account: me.ID, Name: "@" + me.Username}, nil
}

// discordPayload is one gateway message.
type discordPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type discordMessage struct {
	ID        string      `json:"id"`
	ChannelID string      `json:"channel_id"`
	GuildID   string      `json:"guild_id"`
	Author    discordUser `json:"author"`
	Content   string      `json:"content"`
	Timestamp string      `json:"timestamp"`
	Type      int         `json:"type"`
}

// The close codes after which identifying again cannot help.
var discordFatalClose = map[int]string{
	4004: "", // authentication failed: errDiscordRefused
	4010: "Discord refused the shard the role asked for",
	4011: "Discord asks this bot to be sharded, which the role does not do",
	4012: "Discord no longer serves the gateway version the role speaks",
	4013: "Discord refused the intents the role asked for",
	4014: "Discord refused an intent the role asked for, which the bot is not allowed",
}

func (d *discord) listen(ctx context.Context, r *role) error {
	ws, err := dialWebSocket(ctx, d.gateway, d.proxy)
	if err != nil {
		return fmt.Errorf("the Discord gateway: %w", err)
	}
	done := make(chan struct{})
	defer func() { close(done); ws.close() }()
	go func() {
		select {
		case <-ctx.Done():
			ws.close()
		case <-done:
		}
	}()
	p, err := d.readPayload(ws, callTimeout)
	if err != nil {
		return err
	}
	var hello struct {
		Interval int `json:"heartbeat_interval"`
	}
	if p.Op != discordHello || json.Unmarshal(p.D, &hello) != nil || hello.Interval <= 0 {
		return errors.New("the Discord gateway did not open with its hello")
	}
	interval := time.Duration(hello.Interval) * time.Millisecond
	identify := map[string]any{"op": discordIdentify, "d": map[string]any{
		"token": d.token, "intents": discordDirectMessages,
		"properties": map[string]string{"os": "linux", "browser": "agentry", "device": "agentry"},
	}}
	if err := d.writePayload(ws, identify); err != nil {
		return err
	}
	// The heartbeat, whose acknowledgement must come before the next one:
	// a connection that stops acknowledging is closed and opened again.
	var seq seqBox
	acked := make(chan struct{}, 1)
	acked <- struct{}{}
	go func() {
		first := time.Duration(rand.Float64() * float64(interval))
		t := time.NewTimer(first)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			select {
			case <-acked:
			default:
				r.logf("the Discord gateway stopped acknowledging the heartbeat")
				ws.close()
				return
			}
			if err := d.writePayload(ws, map[string]any{"op": discordHeartbeat, "d": seq.get()}); err != nil {
				return
			}
			t.Reset(interval)
		}
	}()
	for {
		p, err := d.readPayload(ws, 2*interval+callTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if p.S != nil {
			seq.set(*p.S)
		}
		switch p.Op {
		case discordHeartbeatAck:
			select {
			case acked <- struct{}{}:
			default:
			}
		case discordHeartbeat:
			if err := d.writePayload(ws, map[string]any{"op": discordHeartbeat, "d": seq.get()}); err != nil {
				return err
			}
		case discordReconnect:
			return errors.New("the Discord gateway asked the role to connect again")
		case discordInvalidSession:
			return errors.New("the Discord gateway ended the session")
		case discordDispatch:
			switch p.T {
			case "READY":
				r.up()
			case "MESSAGE_CREATE":
				var m discordMessage
				if json.Unmarshal(p.D, &m) == nil {
					r.pass(d.message(m))
				}
			}
		}
	}
}

// message is a gateway message as the role judges it: direct when it came
// from no server.
func (d *discord) message(m discordMessage) message {
	n, _ := strconv.ParseInt(m.ID, 10, 64)
	out := message{id: n, chat: m.ChannelID, sender: m.Author.ID, direct: m.GuildID == "", own: m.Author.ID == d.self, bot: m.Author.Bot, text: m.Content}
	if t, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
		out.at = t
	}
	// Only a message a person wrote (the default kind, or a reply) is read:
	// a pin, a call or a join is the service's own.
	if m.Type != 0 && m.Type != 19 {
		out.text = ""
	}
	return out
}

func (d *discord) readPayload(ws *wsConn, idle time.Duration) (discordPayload, error) {
	b, err := ws.read(idle)
	if err != nil {
		var ce *wsCloseError
		if errors.As(err, &ce) {
			if why, fatal := discordFatalClose[ce.Code]; fatal {
				if ce.Code == 4004 {
					return discordPayload{}, errDiscordRefused
				}
				return discordPayload{}, fmt.Errorf("%s (%d)", why, ce.Code)
			}
		}
		return discordPayload{}, fmt.Errorf("the Discord gateway: %s", d.scrub(err.Error()))
	}
	var p discordPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return discordPayload{}, errors.New("the Discord gateway sent something that is not its JSON")
	}
	return p, nil
}

func (d *discord) writePayload(ws *wsConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := ws.writeText(b); err != nil {
		return fmt.Errorf("the Discord gateway: %s", d.scrub(err.Error()))
	}
	return nil
}

// send sends a part as Discord's Markdown, pinging no one and opening no
// link's preview.
func (d *discord) send(ctx context.Context, chat string, p Part) error {
	body := map[string]any{"content": p.Rich, "allowed_mentions": map[string]any{"parse": []string{}}, "flags": discordSuppressEmbeds}
	return d.call(ctx, http.MethodPost, "/channels/"+url.PathEscape(chat)+"/messages", body, nil, "a message")
}

func (d *discord) typing(ctx context.Context, chat string) error {
	return d.call(ctx, http.MethodPost, "/channels/"+url.PathEscape(chat)+"/typing", nil, nil, "typing")
}

// seqBox is the gateway's last sequence number, null before the first.
type seqBox struct {
	mu sync.Mutex
	n  *int64
}

func (s *seqBox) get() *int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *seqBox) set(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = &n
}
