package messenger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// discordTestToken is in a bot token's shape, built from its parts so that
// no scanner of secrets takes the source for a real token.
const discordTestToken = "TESTnotAToken0000000000" + "." + "GabcDE" + "." + "abcdefghijklmnopqrstuvwxyz0123456789AB"

// fakeDiscord is Discord's REST API and gateway: it serves queued gateway
// events and records what the role sent.
type fakeDiscord struct {
	t        *testing.T
	mu       sync.Mutex
	events   chan map[string]any
	sent     []map[string]any
	sentTo   []string
	typing   []string
	identify []map[string]any
	refuse   bool
	closeAt  int // close the first session with this code, once
	sessions int
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	return &fakeDiscord{t: t, events: make(chan map[string]any, 16)}
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gateway" {
		f.gateway(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bot "+discordTestToken || f.refuse {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v10")
	switch {
	case path == "/users/@me":
		_, _ = w.Write([]byte(`{"id":"900","username":"helper","bot":true}`))
	case strings.HasSuffix(path, "/messages"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, body)
		f.sentTo = append(f.sentTo, strings.Split(path, "/")[2])
		_, _ = w.Write([]byte(`{"id":"1"}`))
	case strings.HasSuffix(path, "/typing"):
		f.typing = append(f.typing, strings.Split(path, "/")[2])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDiscord) gateway(w http.ResponseWriter, r *http.Request) {
	p := acceptWS(f.t, w, r)
	if p == nil {
		return
	}
	f.mu.Lock()
	f.sessions++
	n := f.sessions
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"op": discordHello, "d": map[string]int{"heartbeat_interval": 200}})
	p.text(b)
	b, ok := p.read()
	if !ok {
		return
	}
	var id map[string]any
	_ = json.Unmarshal(b, &id)
	f.mu.Lock()
	f.identify = append(f.identify, id)
	closeAt := f.closeAt
	f.mu.Unlock()
	if n == 1 && closeAt != 0 {
		p.closeWith(closeAt)
		return
	}
	seq := 0
	dispatch := func(t string, d any) {
		seq++
		b, _ := json.Marshal(map[string]any{"op": discordDispatch, "s": seq, "t": t, "d": d})
		p.text(b)
	}
	dispatch("READY", map[string]any{"user": map[string]any{"id": "900", "bot": true}, "session_id": "s"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			b, ok := p.read()
			if !ok {
				return
			}
			var hb struct {
				Op int `json:"op"`
			}
			if json.Unmarshal(b, &hb) == nil && hb.Op == discordHeartbeat {
				p.text([]byte(`{"op":11}`))
			}
		}
	}()
	for {
		select {
		case ev := <-f.events:
			dispatch("MESSAGE_CREATE", ev)
		case <-done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func discordDM(id, from, channel, text string) map[string]any {
	return map[string]any{"id": id, "channel_id": channel, "author": map[string]any{"id": from}, "content": text, "type": 0, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)}
}

func startDiscordRole(t *testing.T, f *fakeDiscord, peer *Peer) *roleUnderTest {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return serveRole(t, srv.URL, Start{Kind: KindDiscord, Token: discordTestToken, Peer: peer})
}

var discordPaired = &Peer{Sender: "42", Chat: "7001"}

func TestDiscordSaysWhoTheBotIsAndAsksForDirectMessagesAlone(t *testing.T) {
	f := newFakeDiscord(t)
	r := startDiscordRole(t, f, nil)
	if fr := r.next(t); fr.Ready == nil || fr.Ready.Account != "900" || fr.Ready.Name != "@helper" {
		t.Fatalf("first frame = %+v, want ready as the bot", fr)
	}
	f.events <- discordDM("1", "77", "7002", "hello")
	r.next(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	d, _ := f.identify[0]["d"].(map[string]any)
	if d["token"] != discordTestToken || d["intents"] != float64(discordDirectMessages) {
		t.Fatalf("identify = %v, want the token and DIRECT_MESSAGES alone", d)
	}
}

func TestDiscordUnpairedSenderNeverReachesTheAgent(t *testing.T) {
	f := newFakeDiscord(t)
	r := startDiscordRole(t, f, discordPaired)
	r.next(t)
	f.events <- discordDM("10", "77", "7002", "what's on tomorrow?")
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedUnpaired {
		t.Fatalf("a stranger's message = %+v, want dropped as unpaired", fr)
	}
	f.events <- discordDM("11", "77", "7002", "abcd-efgh")
	if fr := r.next(t); fr.Pairing == nil || fr.Pairing.Code != "ABCDEFGH" || fr.Pairing.Sender != "77" {
		t.Fatalf("a code = %+v, want it passed on for the supervisor to judge", fr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 0 {
		t.Fatalf("the role answered a stranger: %v", f.sent)
	}
}

func TestDiscordPassesThePairedPersonAndDropsAServerMessage(t *testing.T) {
	f := newFakeDiscord(t)
	r := startDiscordRole(t, f, discordPaired)
	r.next(t)
	server := discordDM("20", "42", "7001", "in a server")
	server["guild_id"] = "5"
	f.events <- server
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedNotDirect {
		t.Fatalf("a server's message = %+v, want dropped as not direct", fr)
	}
	f.events <- discordDM("21", "900", "7001", "the bot's own answer")
	f.events <- discordDM("22", "42", "7001", "what's on tomorrow?")
	if fr := r.next(t); fr.Inbound == nil || fr.Inbound.Text != "what's on tomorrow?" || fr.Inbound.ID != 22 || fr.Inbound.Chat != "7001" {
		t.Fatalf("the paired person's message = %+v, want it passed on (and the bot's own passed over)", fr)
	}
}

func TestDiscordSendsMarkdownToThePairedPersonAlonePingingNoOne(t *testing.T) {
	f := newFakeDiscord(t)
	r := startDiscordRole(t, f, discordPaired)
	r.next(t)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "7002", Text: "to a stranger"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error == "" {
		t.Fatalf("a reply to another chat = %+v, want it refused", fr)
	}
	r.write(t, ToRole{Reply: &Reply{ID: 2, Chat: "7001", Text: "**Tomorrow** @everyone:\n\n- `standup` at 9"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error != "" || fr.Sent.Parts != 1 {
		t.Fatalf("a reply = %+v, want it sent as one message", fr)
	}
	r.write(t, ToRole{Typing: &Typing{Chat: "7001"}})
	r.write(t, ToRole{Typing: &Typing{Chat: "7002"}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.typing)
		f.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 || f.sentTo[0] != "7001" {
		t.Fatalf("sent %v to %v, want one message to the paired chat", f.sent, f.sentTo)
	}
	if got := f.sent[0]["content"]; got != "**Tomorrow** @everyone:\n\n- `standup` at 9" {
		t.Fatalf("content = %q, want the Markdown as it was written", got)
	}
	if am, _ := f.sent[0]["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
		t.Fatalf("allowed_mentions = %v, want no one pinged", f.sent[0]["allowed_mentions"])
	}
	if len(f.typing) != 1 || f.typing[0] != "7001" {
		t.Fatalf("typing shown in %v, want the paired chat alone", f.typing)
	}
}

func TestDiscordIdentifiesAgainAfterTheGatewayCloses(t *testing.T) {
	f := newFakeDiscord(t)
	f.closeAt = 4000
	r := startDiscordRole(t, f, discordPaired)
	r.next(t)
	f.events <- discordDM("30", "42", "7001", "after the close")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fr := <-r.frames:
			if fr.Inbound != nil {
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.identify) < 2 {
					t.Fatalf("identified %d times, want again after the close", len(f.identify))
				}
				return
			}
		case <-deadline:
			t.Fatal("the role never identified again")
		}
	}
}

func TestDiscordRefusedTokenEndsTheRoleWithoutRepeatingIt(t *testing.T) {
	f := newFakeDiscord(t)
	f.refuse = true
	r := startDiscordRole(t, f, nil)
	select {
	case err := <-r.done:
		if err == nil || !strings.Contains(err.Error(), TokenRefused) || strings.Contains(err.Error(), discordTestToken) {
			t.Fatalf("Serve = %v, want a refusal that does not repeat the token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role kept going on a refused token")
	}
}

func TestDiscordGatewayAuthenticationFailureIsARefusal(t *testing.T) {
	f := newFakeDiscord(t)
	f.closeAt = 4004
	r := startDiscordRole(t, f, nil)
	r.next(t)
	for {
		select {
		case fr := <-r.frames:
			if fr.State != nil && !fr.State.Connected && strings.Contains(fr.State.Error, TokenRefused) {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the gateway's 4004 was not taken as a refused token")
		}
	}
}
