package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Matrix, through an account on the homeserver the person names: the
// client-server API (v3), a long poll of /sync for the rooms the account is
// in, and /send to answer. The role reads direct rooms that are not
// encrypted: it holds no device keys, so a message encrypted end to end is
// dropped, never read. A direct room is one with the account and one person
// in it, and an answer goes to the paired room only while it is one. While
// no one is paired, the account joins a direct room it is invited to, so the
// person can send it the pairing code there, within a bound of its own a
// minute and on the rooms it is in at once; once someone is paired, it
// declines every invitation and leaves every other room.

// matrixPoll is how long one /sync waits for an event; its request's
// deadline is longer by matrixSlack. A variable so a test need not wait.
var (
	matrixPoll  = 50 * time.Second
	matrixSlack = 15 * time.Second
)

// The bounds on the rooms a stranger can bring the account into: the
// invitations it takes a minute, and the rooms it is in while no one is
// paired. With the filter's ten events a room, a /sync stays far under what
// the role reads of one answer.
const (
	maxMatrixJoinsPerMinute = 3
	maxMatrixUnpairedRooms  = 5
)

// matrixFilter keeps a /sync to what the role reads: the rooms' messages,
// and the state that says whether a room is encrypted and who is in it.
const matrixFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"account_data":{"not_types":["*"]},"ephemeral":{"not_types":["*"]},` +
	`"state":{"types":["m.room.encryption","m.room.member"],"lazy_load_members":true},` +
	`"timeline":{"limit":10,"types":["m.room.message","m.room.encrypted","m.room.encryption","m.room.member"]}}}`

// matrix is one account's client.
type matrix struct {
	base   string
	token  string
	client *http.Client
	self   string
	since  string
	// lastID is the id of the last message passed on: ids grow message
	// by message, as the supervisor counts on to know one it saw.
	lastID int64
	poll   time.Duration
	txn    atomic.Int64
	start  int64

	// mu guards rooms and joins, which the role's answers read too.
	mu    sync.Mutex
	rooms map[string]*matrixRoom
	joins []time.Time
}

// matrixRoom is what the role knows of a room it is in.
type matrixRoom struct {
	encrypted bool
	// members is the count of people who joined, -1 while unknown.
	members int
}

// newMatrix is a client for the account whose access token is token, on
// the homeserver at base (its https:// client address).
func newMatrix(base, token, proxy string) (*matrix, error) {
	if base == "" {
		return nil, errors.New("a Matrix account needs its homeserver's address")
	}
	poll := matrixPoll
	c, err := newHTTPClient(proxy, poll+matrixSlack)
	if err != nil {
		return nil, err
	}
	return &matrix{base: strings.TrimRight(base, "/") + "/_matrix/client/v3", token: token, client: c, poll: poll, start: time.Now().UnixNano(), rooms: map[string]*matrixRoom{}}, nil
}

func (m *matrix) scrub(text string) string {
	return scrubber("the account's access token", m.token)(text)
}

// errMatrixRefused says the homeserver does not take the access token.
var errMatrixRefused = refused("the homeserver does not take the account's access token, which was signed out or belongs to another server")

// call sends a client-server API request and decodes its answer into out.
func (m *matrix) call(ctx context.Context, method, path string, body, out any, timeout time.Duration, what string) error {
	a, err := doJSON(ctx, m.client, method, m.base+path, "Bearer "+m.token, body, timeout, what, m.scrub)
	if err != nil {
		return err
	}
	if a.status/100 != 2 {
		var e struct {
			Errcode    string `json:"errcode"`
			Error      string `json:"error"`
			RetryAfter int64  `json:"retry_after_ms"`
		}
		_ = json.Unmarshal(a.body, &e)
		if a.status == http.StatusUnauthorized || e.Errcode == "M_UNKNOWN_TOKEN" || e.Errcode == "M_MISSING_TOKEN" {
			return errMatrixRefused
		}
		se := &ServiceError{Service: "the homeserver", Method: what, Code: a.status, Description: m.scrub(strings.TrimSpace(e.Errcode + " " + e.Error))}
		if a.status == http.StatusTooManyRequests {
			se.After = max(time.Duration(e.RetryAfter)*time.Millisecond, retryAfterHeader(a.header), time.Second)
		}
		return se
	}
	if out != nil {
		if err := json.Unmarshal(a.body, out); err != nil {
			return fmt.Errorf("%s: the homeserver's answer could not be read", what)
		}
	}
	return nil
}

func (m *matrix) connect(ctx context.Context) (Ready, error) {
	var me struct {
		UserID string `json:"user_id"`
	}
	if err := m.call(ctx, http.MethodGet, "/account/whoami", nil, &me, callTimeout, "whoami"); err != nil {
		return Ready{}, err
	}
	if me.UserID == "" {
		return Ready{}, errors.New("whoami: the homeserver named no account")
	}
	m.self = me.UserID
	return Ready{Account: me.UserID, Name: me.UserID}, nil
}

type matrixEvent struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	StateKey *string         `json:"state_key"`
	TS       int64           `json:"origin_server_ts"`
	Content  json.RawMessage `json:"content"`
}

type matrixSync struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Summary struct {
				Joined *int `json:"m.joined_member_count"`
			} `json:"summary"`
			State    struct{ Events []matrixEvent } `json:"state"`
			Timeline struct{ Events []matrixEvent } `json:"timeline"`
		} `json:"join"`
		Invite map[string]struct {
			InviteState struct{ Events []matrixEvent } `json:"invite_state"`
		} `json:"invite"`
	} `json:"rooms"`
}

func (m *matrix) listen(ctx context.Context, r *role) error {
	for ctx.Err() == nil {
		q := url.Values{"timeout": {strconv.FormatInt(m.poll.Milliseconds(), 10)}, "filter": {matrixFilter}}
		if m.since != "" {
			q.Set("since", m.since)
		}
		var s matrixSync
		if err := m.call(ctx, http.MethodGet, "/sync?"+q.Encode(), nil, &s, m.poll+matrixSlack, "sync"); err != nil {
			return err
		}
		// The first sync since the role started is what came before it: it
		// says which rooms are which and passes no message on.
		first := m.since == ""
		m.since = s.NextBatch
		r.up()
		for id, inv := range s.Rooms.Invite {
			m.invited(ctx, r, id, inv.InviteState.Events)
		}
		for id, j := range s.Rooms.Join {
			m.mu.Lock()
			room := m.room(id)
			if j.Summary.Joined != nil {
				room.members = *j.Summary.Joined
			}
			for _, ev := range slices.Concat(j.State.Events, j.Timeline.Events) {
				switch ev.Type {
				case "m.room.encryption":
					room.encrypted = true
				case "m.room.member":
					if j.Summary.Joined == nil {
						room.members = -1
					}
				}
			}
			m.mu.Unlock()
			if first {
				continue
			}
			for _, ev := range j.Timeline.Events {
				if ev.Type == "m.room.message" || ev.Type == "m.room.encrypted" {
					r.pass(m.message(ctx, id, ev))
				}
			}
		}
		// Paired, the account is in the paired room alone.
		if peer := r.currentPeer(); peer != nil {
			for _, id := range m.roomsBut(peer.Chat) {
				m.leave(ctx, r, id)
			}
		}
	}
	return ctx.Err()
}

// room is what the role knows of the room id; m.mu is held.
func (m *matrix) room(id string) *matrixRoom {
	room, ok := m.rooms[id]
	if !ok {
		room = &matrixRoom{members: -1}
		m.rooms[id] = room
	}
	return room
}

// roomsBut are the rooms the account is in but keep.
func (m *matrix) roomsBut(keep string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id := range m.rooms {
		if id != keep {
			out = append(out, id)
		}
	}
	return out
}

// joinAllowed counts an invitation against the minute's bound and the rooms
// the account may be in while no one is paired.
func (m *matrix) joinAllowed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	kept := m.joins[:0]
	for _, t := range m.joins {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	m.joins = kept
	if len(m.joins) >= maxMatrixJoinsPerMinute || len(m.rooms) >= maxMatrixUnpairedRooms {
		return false
	}
	m.joins = append(m.joins, now)
	return true
}

// invited joins a direct room it was invited to while no one is paired,
// and declines every other invitation.
func (m *matrix) invited(ctx context.Context, r *role, id string, events []matrixEvent) {
	direct, inviter := false, ""
	for _, ev := range events {
		if ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == m.self {
			var c struct {
				Membership string `json:"membership"`
				IsDirect   bool   `json:"is_direct"`
			}
			if json.Unmarshal(ev.Content, &c) == nil && c.Membership == "invite" {
				direct, inviter = c.IsDirect, ev.Sender
			}
		}
	}
	reason := ""
	switch {
	case !direct || inviter == "":
		reason = DroppedNotDirect
	case r.currentPeer() != nil:
		reason = DroppedUnpaired
	case !m.joinAllowed():
		reason = DroppedPairings
	}
	if reason != "" {
		m.leave(ctx, r, id)
		r.drop(reason)
		return
	}
	if err := m.call(ctx, http.MethodPost, "/join/"+url.PathEscape(id), struct{}{}, nil, callTimeout, "join"); err != nil {
		r.logf("joining a room it was invited to: %s", err)
		return
	}
	m.mu.Lock()
	m.room(id)
	m.mu.Unlock()
}

// leave leaves (or declines) the room id.
func (m *matrix) leave(ctx context.Context, r *role, id string) {
	m.mu.Lock()
	delete(m.rooms, id)
	m.mu.Unlock()
	if err := m.call(ctx, http.MethodPost, "/rooms/"+url.PathEscape(id)+"/leave", struct{}{}, nil, callTimeout, "leave"); err != nil {
		r.logf("leaving a room: %s", err)
	}
}

// members is the count of people who joined the room id, -1 when it is
// not known.
func (m *matrix) members(ctx context.Context, id string) int {
	m.mu.Lock()
	n := m.room(id).members
	m.mu.Unlock()
	if n >= 0 {
		return n
	}
	var res struct {
		Joined map[string]json.RawMessage `json:"joined"`
	}
	if err := m.call(ctx, http.MethodGet, "/rooms/"+url.PathEscape(id)+"/joined_members", nil, &res, callTimeout, "joined_members"); err != nil {
		return -1
	}
	m.mu.Lock()
	m.room(id).members = len(res.Joined)
	m.mu.Unlock()
	return len(res.Joined)
}

// encrypted says the room id is encrypted.
func (m *matrix) encrypted(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.room(id).encrypted
}

// message is an event as the role judges it.
func (m *matrix) message(ctx context.Context, id string, ev matrixEvent) message {
	// An event's id is its server's time in milliseconds, made to grow
	// by one where two share a millisecond or a server's clock went back.
	m.lastID = max(ev.TS, m.lastID+1)
	out := message{id: m.lastID, chat: id, sender: ev.Sender, own: ev.Sender == m.self}
	if out.own {
		return out
	}
	out.direct = m.members(ctx, id) == 2
	if ev.TS > 0 {
		out.at = time.UnixMilli(ev.TS)
	}
	if ev.Type == "m.room.encrypted" || m.encrypted(id) {
		out.encrypted = true
		return out
	}
	var c struct {
		MsgType   string `json:"msgtype"`
		Body      string `json:"body"`
		RelatesTo struct {
			RelType string `json:"rel_type"`
		} `json:"m.relates_to"`
	}
	if json.Unmarshal(ev.Content, &c) != nil {
		return out
	}
	switch {
	case c.MsgType == "m.notice":
		// A notice is a bot's, by the protocol's own convention.
		out.bot = true
	case c.RelatesTo.RelType == "m.replace":
		// An edit of an earlier message is not a new one.
	case c.MsgType == "m.text" || c.MsgType == "m.emote":
		out.text = stripReplyFallback(c.Body)
	}
	return out
}

// stripReplyFallback is a reply's body without the quote of the message it
// answers, which a client puts before it.
func stripReplyFallback(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, ">") {
			if l == "" {
				return strings.Join(lines[i+1:], "\n")
			}
			break
		}
	}
	return body
}

// errNotDirect refuses an answer to a room someone else joined since it was
// paired: it is no longer the person's alone.
var errNotDirect = errors.New("the paired room has someone else in it now: the answer was not sent")

// send sends a part as HTML, with its plain text beside it, while the room
// is the paired person's and the account's alone.
func (m *matrix) send(ctx context.Context, chat string, p Part) error {
	if m.members(ctx, chat) != 2 {
		return errNotDirect
	}
	txn := strconv.FormatInt(m.start, 36) + "-" + strconv.FormatInt(m.txn.Add(1), 10)
	body := map[string]string{"msgtype": "m.text", "body": p.Plain, "format": "org.matrix.custom.html", "formatted_body": HTMLLineBreaks(p.Rich)}
	return m.call(ctx, http.MethodPut, "/rooms/"+url.PathEscape(chat)+"/send/m.room.message/"+url.PathEscape(txn), body, nil, callTimeout, "a message")
}

func (m *matrix) typing(ctx context.Context, chat string) error {
	if m.members(ctx, chat) != 2 {
		return errNotDirect
	}
	return m.call(ctx, http.MethodPut, "/rooms/"+url.PathEscape(chat)+"/typing/"+url.PathEscape(m.self), map[string]any{"typing": true, "timeout": 8000}, nil, callTimeout, "typing")
}

// MatrixLogin signs in to the homeserver at server as user with password
// and is the new session's access token: how a person adds a Matrix account
// without finding a token. It runs on the person's own computer, never in
// the role, and the password is never kept.
func MatrixLogin(ctx context.Context, server, user, password string) (string, error) {
	if _, err := ServerHost(server); err != nil {
		return "", err
	}
	c, err := newHTTPClient("", callTimeout)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]string{"type": "m.id.user", "user": user},
		"password":   password, "initial_device_display_name": "Agentry",
	}
	a, err := doJSON(ctx, c, http.MethodPost, strings.TrimRight(server, "/")+"/_matrix/client/v3/login", "", body, callTimeout, "login", scrubber("the password", password))
	if err != nil {
		return "", err
	}
	var res struct {
		AccessToken string `json:"access_token"`
		Errcode     string `json:"errcode"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(a.body, &res)
	if a.status/100 != 2 || res.AccessToken == "" {
		why := strings.TrimSpace(res.Errcode + " " + res.Error)
		if why == "" {
			why = "HTTP " + strconv.Itoa(a.status)
		}
		return "", fmt.Errorf("the homeserver did not sign in %s: %s", user, scrubber("the password", password)(why))
	}
	return res.AccessToken, nil
}
