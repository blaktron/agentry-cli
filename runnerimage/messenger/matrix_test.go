package messenger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const matrixTestToken = "syt_YWdlbnQ_testtokenvalue_0abc12"

// fakeHomeserver is a Matrix homeserver's client-server API: /sync serves
// queued batches, and the rest is recorded.
type fakeHomeserver struct {
	mu      sync.Mutex
	first   map[string]any // the first sync's rooms
	batches chan map[string]any
	members map[string]int
	joined  []string
	left    []string
	sent    []map[string]any
	sentTo  []string
	typing  []string
	refuse  bool
	syncs   int
}

func newFakeHomeserver() *fakeHomeserver {
	return &fakeHomeserver{batches: make(chan map[string]any, 16), members: map[string]int{}}
}

func (h *fakeHomeserver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+matrixTestToken || h.refuse {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token passed."}`))
		return
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/_matrix/client/v3")
	parts := strings.Split(path, "/")
	room := ""
	if len(parts) > 2 {
		room, _ = url.PathUnescape(parts[2])
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/account/whoami":
		write(map[string]string{"user_id": "@agent:hs.test", "device_id": "D"})
	case path == "/sync":
		h.mu.Lock()
		h.syncs++
		n := h.syncs
		first := h.first
		h.mu.Unlock()
		if r.URL.Query().Get("filter") == "" {
			http.Error(w, "no filter", http.StatusBadRequest)
			return
		}
		if n == 1 {
			if r.URL.Query().Get("since") != "" {
				http.Error(w, "since on the first sync", http.StatusBadRequest)
				return
			}
			write(map[string]any{"next_batch": "b1", "rooms": first})
			return
		}
		select {
		case rooms := <-h.batches:
			write(map[string]any{"next_batch": "b" + itoa(int64(n)), "rooms": rooms})
		case <-time.After(100 * time.Millisecond):
			write(map[string]any{"next_batch": "b" + itoa(int64(n))})
		case <-r.Context().Done():
		}
	case len(parts) == 3 && parts[1] == "join":
		h.mu.Lock()
		h.joined = append(h.joined, room)
		h.mu.Unlock()
		write(map[string]string{"room_id": room})
	case strings.HasSuffix(path, "/leave"):
		h.mu.Lock()
		h.left = append(h.left, room)
		h.mu.Unlock()
		write(map[string]any{})
	case strings.HasSuffix(path, "/joined_members"):
		h.mu.Lock()
		n := h.members[room]
		h.mu.Unlock()
		joined := map[string]any{}
		for i := range n {
			joined["@m"+itoa(int64(i))+":hs.test"] = map[string]any{}
		}
		write(map[string]any{"joined": joined})
	case strings.Contains(path, "/send/m.room.message/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.sent = append(h.sent, body)
		h.sentTo = append(h.sentTo, room)
		h.mu.Unlock()
		write(map[string]string{"event_id": "$e"})
	case strings.Contains(path, "/typing/"):
		h.mu.Lock()
		h.typing = append(h.typing, room)
		h.mu.Unlock()
		write(map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func matrixText(sender, body string, ts int64) map[string]any {
	return map[string]any{"type": "m.room.message", "sender": sender, "origin_server_ts": ts, "content": map[string]any{"msgtype": "m.text", "body": body}}
}

func joinedRoom(events ...map[string]any) map[string]any {
	return map[string]any{"timeline": map[string]any{"events": events}}
}

func invite(sender string, direct bool) map[string]any {
	return map[string]any{"invite_state": map[string]any{"events": []any{
		map[string]any{"type": "m.room.member", "state_key": "@agent:hs.test", "sender": sender, "content": map[string]any{"membership": "invite", "is_direct": direct}},
	}}}
}

func startMatrixRole(t *testing.T, h *fakeHomeserver, peer *Peer) *roleUnderTest {
	t.Helper()
	old := matrixPoll
	matrixPoll = 100 * time.Millisecond
	t.Cleanup(func() { matrixPoll = old })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return serveRole(t, "", Start{Kind: KindMatrix, Token: matrixTestToken, Server: srv.URL, Peer: peer})
}

var matrixPaired = &Peer{Sender: "@me:hs.test", Chat: "!dm:hs.test"}

func TestMatrixPassesThePairedPersonButNotWhatCameBefore(t *testing.T) {
	h := newFakeHomeserver()
	h.members["!dm:hs.test"] = 2
	now := time.Now().UnixMilli()
	h.first = map[string]any{"join": map[string]any{"!dm:hs.test": joinedRoom(matrixText("@me:hs.test", "from before the role", now-1000))}}
	r := startMatrixRole(t, h, matrixPaired)
	if fr := r.next(t); fr.Ready == nil || fr.Ready.Account != "@agent:hs.test" {
		t.Fatalf("first frame = %+v, want ready as the account", fr)
	}
	h.batches <- map[string]any{"join": map[string]any{"!dm:hs.test": joinedRoom(
		matrixText("@agent:hs.test", "the account's own answer", now),
		matrixText("@me:hs.test", "> <@agent:hs.test> earlier\n\nwhat's on tomorrow?", now+1),
	)}}
	fr := r.next(t)
	if fr.Inbound == nil || fr.Inbound.Text != "what's on tomorrow?" || fr.Inbound.Chat != "!dm:hs.test" || fr.Inbound.ID != now+1 {
		t.Fatalf("the paired person's message = %+v, want it passed on without its quote (and nothing from before)", fr)
	}
}

func TestMatrixDropsAnEncryptedRoomAndAGroup(t *testing.T) {
	h := newFakeHomeserver()
	h.members["!dm:hs.test"] = 2
	h.members["!group:hs.test"] = 3
	r := startMatrixRole(t, h, matrixPaired)
	r.next(t)
	now := time.Now().UnixMilli()
	h.batches <- map[string]any{"join": map[string]any{"!dm:hs.test": joinedRoom(
		map[string]any{"type": "m.room.encrypted", "sender": "@me:hs.test", "origin_server_ts": now, "content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "x"}},
	)}}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedEncrypted {
		t.Fatalf("an encrypted message = %+v, want dropped as encrypted", fr)
	}
	h.batches <- map[string]any{"join": map[string]any{"!group:hs.test": joinedRoom(matrixText("@me:hs.test", "in a group", now+1))}}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedNotDirect {
		t.Fatalf("a group's message = %+v, want dropped as not direct", fr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		left := strings.Join(h.left, ",")
		h.mu.Unlock()
		if left == "!group:hs.test" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("left %q, want the group the paired account heard from", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMatrixJoinsADirectInvitationOnlyWhileUnpaired(t *testing.T) {
	h := newFakeHomeserver()
	r := startMatrixRole(t, h, nil)
	r.next(t)
	h.batches <- map[string]any{"invite": map[string]any{"!dm:hs.test": invite("@me:hs.test", true), "!room:hs.test": invite("@me:hs.test", false)}}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedNotDirect {
		t.Fatalf("a room's invitation = %+v, want declined as not direct", fr)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.mu.Lock()
		n := len(h.joined)
		h.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	r.write(t, ToRole{Paired: &Paired{Peer: matrixPaired}})
	time.Sleep(50 * time.Millisecond)
	h.batches <- map[string]any{"invite": map[string]any{"!other:hs.test": invite("@stranger:hs.test", true)}}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedUnpaired {
		t.Fatalf("an invitation once paired = %+v, want declined", fr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Join(h.joined, ",") != "!dm:hs.test" {
		t.Fatalf("joined %v, want the direct invitation alone", h.joined)
	}
	if !strings.Contains(strings.Join(h.left, ","), "!room:hs.test") || !strings.Contains(strings.Join(h.left, ","), "!other:hs.test") {
		t.Fatalf("declined %v, want the room's and the stranger's", h.left)
	}
}

func TestMatrixUnpairedSenderNeverReachesTheAgent(t *testing.T) {
	h := newFakeHomeserver()
	h.members["!dm2:hs.test"] = 2
	r := startMatrixRole(t, h, nil)
	r.next(t)
	now := time.Now().UnixMilli()
	h.batches <- map[string]any{"join": map[string]any{"!dm2:hs.test": joinedRoom(matrixText("@me:hs.test", "hello", now), matrixText("@me:hs.test", "abcd efgh", now+1))}}
	if fr := r.next(t); fr.Dropped == nil || fr.Dropped.Reason != DroppedUnpaired {
		t.Fatalf("a message before pairing = %+v, want dropped as unpaired", fr)
	}
	if fr := r.next(t); fr.Pairing == nil || fr.Pairing.Sender != "@me:hs.test" || fr.Pairing.Chat != "!dm2:hs.test" {
		t.Fatalf("a code = %+v, want it passed on", fr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) != 0 {
		t.Fatalf("the role answered before pairing: %v", h.sent)
	}
}

func TestMatrixSendsHTMLAndPlainToThePairedRoomAlone(t *testing.T) {
	h := newFakeHomeserver()
	h.members["!dm:hs.test"] = 2
	r := startMatrixRole(t, h, matrixPaired)
	r.next(t)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "!other:hs.test", Text: "to a stranger"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error == "" {
		t.Fatalf("a reply to another room = %+v, want it refused", fr)
	}
	r.write(t, ToRole{Reply: &Reply{ID: 2, Chat: "!dm:hs.test", Text: "**Tomorrow**\nstandup at 9\n\n```\na\nb\n```"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error != "" {
		t.Fatalf("a reply = %+v, want it sent", fr)
	}
	r.write(t, ToRole{Typing: &Typing{Chat: "!dm:hs.test"}})
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) != 1 || h.sentTo[0] != "!dm:hs.test" {
		t.Fatalf("sent %v to %v, want one message to the paired room", h.sent, h.sentTo)
	}
	m := h.sent[0]
	if m["msgtype"] != "m.text" || m["format"] != "org.matrix.custom.html" || m["formatted_body"] != "<b>Tomorrow</b><br>standup at 9<br><br><pre>a\nb</pre>" || m["body"] != "**Tomorrow**\nstandup at 9\n\na\nb" {
		t.Fatalf("message = %v, want HTML with its line breaks and the plain text beside it", m)
	}
	if len(h.typing) != 1 {
		t.Fatalf("typing = %v, want one", h.typing)
	}
}

func TestMatrixRefusedTokenEndsTheRoleWithoutRepeatingIt(t *testing.T) {
	h := newFakeHomeserver()
	h.refuse = true
	r := startMatrixRole(t, h, nil)
	select {
	case err := <-r.done:
		if err == nil || !strings.Contains(err.Error(), TokenRefused) || strings.Contains(err.Error(), matrixTestToken) {
			t.Fatalf("Serve = %v, want a refusal that does not repeat the token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role kept going on a refused token")
	}
}

func TestMatrixStartNeedsTheAccountsServer(t *testing.T) {
	r := serveRole(t, "", Start{Kind: KindMatrix, Token: matrixTestToken})
	select {
	case err := <-r.done:
		if err == nil {
			t.Fatal("a Matrix start with no homeserver was taken")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role started with no homeserver")
	}
}

func TestMatrixSendsNothingToAPairedRoomSomeoneElseJoined(t *testing.T) {
	h := newFakeHomeserver()
	h.members["!dm:hs.test"] = 2
	r := startMatrixRole(t, h, matrixPaired)
	r.next(t)
	h.mu.Lock()
	h.members["!dm:hs.test"] = 3
	h.mu.Unlock()
	state := "@third:hs.test"
	h.batches <- map[string]any{"join": map[string]any{"!dm:hs.test": joinedRoom(map[string]any{"type": "m.room.member", "state_key": state, "sender": "@me:hs.test", "content": map[string]any{"membership": "join"}})}}
	time.Sleep(300 * time.Millisecond)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "!dm:hs.test", Text: "for the paired person"}})
	if fr := r.next(t); fr.Sent == nil || fr.Sent.Error == "" {
		t.Fatalf("a reply to a room of three = %+v, want it refused", fr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) != 0 {
		t.Fatalf("sent %v to a room someone else joined", h.sent)
	}
}

func TestMatrixBoundsTheRoomsAStrangerBringsItInto(t *testing.T) {
	h := newFakeHomeserver()
	r := startMatrixRole(t, h, nil)
	r.next(t)
	invites := map[string]any{}
	for i := range 6 {
		invites["!r"+itoa(int64(i))+":hs.test"] = invite("@stranger:hs.test", true)
	}
	h.batches <- map[string]any{"invite": invites}
	declined := 0
	for range 3 {
		if fr := r.next(t); fr.Dropped != nil && fr.Dropped.Reason == DroppedPairings {
			declined++
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.joined) != maxMatrixJoinsPerMinute || declined != 3 {
		t.Fatalf("joined %d of six invitations and declined %d, want %d joined in a minute", len(h.joined), declined, maxMatrixJoinsPerMinute)
	}
}
