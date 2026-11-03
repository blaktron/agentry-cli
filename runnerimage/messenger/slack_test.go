package messenger

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// In Slack's shape, built from parts so that no scanner of secrets
	// takes the source for a real token.
	slackAppToken = "xapp" + "-1-A0TEST-1234567890-" + "abcdef0123456789abcdef"
	slackBotToken = "xoxb" + "-1234567890-0987654321-" + "abcdefghijklmnop"
	slackTestCred = slackAppToken + " " + slackBotToken
)

// fakeSlack is Slack's Web API and Socket Mode: it serves queued events and
// records what the role sent and acknowledged.
type fakeSlack struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	events     chan map[string]any
	sent       []map[string]any
	acks       []string
	refuseBot  bool
	disconnect bool // tell the first connection to connect again
	opened     int
	envelope   int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{t: t, events: make(chan map[string]any, 16)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		f.socket(w, r)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/api/auth.test":
		if auth != slackBotToken || f.refuseBot {
			write(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		write(map[string]any{"ok": true, "team_id": "T1", "team": "Home", "user_id": "UBOT", "user": "helper", "bot_id": "B1"})
	case "/api/apps.connections.open":
		if auth != slackAppToken {
			write(map[string]any{"ok": false, "error": "not_allowed_token_type"})
			return
		}
		f.opened++
		write(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/socket?ticket=x"})
	case "/api/chat.postMessage":
		if auth != slackBotToken {
			write(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, body)
		write(map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSlack) socket(w http.ResponseWriter, r *http.Request) {
	p := acceptWS(f.t, w, r)
	if p == nil {
		return
	}
	p.text([]byte(`{"type":"hello","num_connections":1}`))
	f.mu.Lock()
	first := f.opened == 1
	disconnect := f.disconnect
	f.mu.Unlock()
	if first && disconnect {
		p.text([]byte(`{"type":"disconnect","reason":"refresh_requested"}`))
		_, _ = p.read()
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			b, ok := p.read()
			if !ok {
				return
			}
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(b, &ack) == nil && ack.EnvelopeID != "" {
				f.mu.Lock()
				f.acks = append(f.acks, ack.EnvelopeID)
				f.mu.Unlock()
			}
		}
	}()
	for {
		select {
		case ev := <-f.events:
			f.mu.Lock()
			f.envelope++
			id := "env-" + string(rune('a'+f.envelope))
			f.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"envelope_id": id, "type": "events_api", "payload": map[string]any{"event": ev}})
			p.text(b)
		case <-done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func slackIM(user, channel, ts, text string) map[string]any {
	return map[string]any{"type": "message", "channel_type": "im", "channel": channel, "user": user, "text": text, "ts": ts}
}

func startSlackRole(t *testing.T, f *fakeSlack, peer *Peer) *roleUnderTest {
	t.Helper()
	return serveRole(t, f.srv.URL, Start{Kind: KindSlack, Token: slackTestCred, Peer: peer})
}

var slackPaired = &Peer{Sender: "U42", Chat: "D42"}

func TestSlackSaysWhoTheAppIs(t *testing.T) {
	f := newFakeSlack(t)
	r := startSlackRole(t, f, nil)
	if fr := r.next(t); fr.Ready == nil || fr.Ready.Account != "T1/UBOT" || fr.Ready.Name != "@helper in Home" {
		t.Fatalf("first frame = %+v, want ready as the app's bot", fr)
	}
}

func TestSlackPassesThePairedPersonAndAcknowledgesEveryEnvelope(t *testing.T) {
	f := newFakeSlack(t)
	r := startSlackRole(t, f, slackPaired)
	r.next(t)
	now := time.Now().Unix()
	ts := func(n int) string { return itoa(now) + "." + pad6(n) }
	f.events <- map[string]any{"type": "message", "channel_type": "channel", "channel": "C1", "user": "U42", "text": "in a channel", "ts": ts(1)}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedNotDirect {
		t.Fatalf("a channel's message = %+v, want dropped as not direct", fr)
	}
	f.events <- map[string]any{"type": "message", "channel_type": "im", "channel": "D42", "user": "UBOT", "bot_id": "B1", "text": "the app's own", "ts": ts(2)}
	edited := slackIM("U42", "D42", ts(3), "edited")
	edited["subtype"] = "message_changed"
	f.events <- edited
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedNoText {
		t.Fatalf("an edit = %+v, want dropped (and the app's own passed over)", fr)
	}
	f.events <- slackIM("U42", "D42", ts(4), "what&apos;s on &lt;tomorrow&gt; &amp; after?")
	fr := r.next(t)
	if fr.Inbound == nil || fr.Inbound.Text != "what&apos;s on <tomorrow> & after?" || fr.Inbound.Chat != "D42" {
		t.Fatalf("the paired person's message = %+v, want it passed on, unescaped", fr)
	}
	if want := now*1_000_000 + 4; fr.Inbound.ID != want {
		t.Fatalf("id = %d, want the ts as one number (%d)", fr.Inbound.ID, want)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.acks)
		f.mu.Unlock()
		if n == 4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("acknowledged %d envelopes, want 4", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSlackUnpairedSenderNeverReachesTheAgent(t *testing.T) {
	f := newFakeSlack(t)
	r := startSlackRole(t, f, slackPaired)
	r.next(t)
	f.events <- slackIM("U77", "D77", slackNow(1), "hello")
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedUnpaired {
		t.Fatalf("a stranger's message = %+v, want dropped as unpaired", fr)
	}
	f.events <- slackIM("U77", "D77", slackNow(2), "ABCD EFGH")
	if fr := r.next(t); fr.Pairing == nil || fr.Pairing.Sender != "U77" || fr.Pairing.Chat != "D77" {
		t.Fatalf("a code = %+v, want it passed on", fr)
	}
}

func TestSlackSendsMrkdwnToThePairedPersonAlone(t *testing.T) {
	f := newFakeSlack(t)
	r := startSlackRole(t, f, slackPaired)
	r.next(t)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "D77", Text: "to a stranger"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error == "" {
		t.Fatalf("a reply to another chat = %+v, want it refused", fr)
	}
	r.write(t, ToRole{Reply: &Reply{ID: 2, Chat: "D42", Text: "**Tomorrow** <b>, see [the plan](https://example.com/p)"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error != "" {
		t.Fatalf("a reply = %+v, want it sent", fr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || f.sent[0]["channel"] != "D42" {
		t.Fatalf("sent %v, want one message to the paired chat", f.sent)
	}
	if got, want := f.sent[0]["text"], "*Tomorrow* &lt;b&gt;, see <https://example.com/p|the plan>"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if f.sent[0]["unfurl_links"] != false {
		t.Fatalf("a link's preview was not turned off: %v", f.sent[0])
	}
}

func TestSlackConnectsAgainWhenAskedTo(t *testing.T) {
	f := newFakeSlack(t)
	f.disconnect = true
	r := startSlackRole(t, f, slackPaired)
	r.next(t)
	f.events <- slackIM("U42", "D42", slackNow(1), "after the refresh")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fr := <-r.frames:
			if fr.Inbound != nil {
				return
			}
		case <-deadline:
			t.Fatal("the role never connected again")
		}
	}
}

func TestSlackRefusedTokenEndsTheRoleWithoutRepeatingIt(t *testing.T) {
	f := newFakeSlack(t)
	f.refuseBot = true
	r := startSlackRole(t, f, nil)
	select {
	case err := <-r.done:
		if err == nil || !strings.Contains(err.Error(), TokenRefused) || strings.Contains(err.Error(), slackBotToken) || strings.Contains(err.Error(), slackAppToken) {
			t.Fatalf("Serve = %v, want a refusal that repeats neither token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role kept going on a refused token")
	}
}

func TestSlackManifestAsksForDirectMessagesOverSocketModeAlone(t *testing.T) {
	var m struct {
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			SocketMode bool `json:"socket_mode_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(SlackManifest("Helper")), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Settings.SocketMode || strings.Join(m.Settings.Events.Bot, ",") != "message.im" || strings.Join(m.OAuth.Scopes.Bot, ",") != "chat:write,im:history" {
		t.Fatalf("manifest = %+v, want Socket Mode, message.im and two scopes alone", m)
	}
}

func slackNow(n int) string { return itoa(time.Now().Unix()) + "." + pad6(n) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func pad6(n int) string {
	s := itoa(int64(n))
	return strings.Repeat("0", 6-len(s)) + s
}

func TestSlackDialsNoSocketModeAddressOffItsHosts(t *testing.T) {
	f := newFakeSlack(t)
	s, err := newSlack(f.srv.URL, slackTestCred, "")
	if err != nil {
		t.Fatal(err)
	}
	// As against Slack's own address: only its Socket Mode hosts are dialled.
	s.anyWSHost = false
	err = s.listen(t.Context(), &role{w: NewWriter(io.Discard)})
	if err == nil || !strings.Contains(err.Error(), "does not reach") {
		t.Fatalf("listen = %v, want the test server's address refused", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opened != 1 {
		t.Fatalf("opened %d connections, want the one apps.connections.open named and none dialled", f.opened)
	}
}
