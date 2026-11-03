package messenger

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "123456:AAtest-token-value"

// fakeBot is a Bot API that serves queued updates and records what was sent.
type fakeBot struct {
	mu       sync.Mutex
	updates  [][]telegramUpdate
	sent     []map[string]any
	actions  []map[string]any
	failPoll int
	refuse   bool
	nextID   int64
}

func (b *fakeBot) queue(ms ...telegramMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var us []telegramUpdate
	for _, m := range ms {
		b.nextID++
		m := m
		us = append(us, telegramUpdate{UpdateID: b.nextID, Message: &m})
	}
	b.updates = append(b.updates, us)
}

func (b *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
		http.Error(w, "no", http.StatusNotFound)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
	var params map[string]any
	_ = json.NewDecoder(r.Body).Decode(&params)
	answer := func(v any) {
		res, _ := json.Marshal(v)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": json.RawMessage(res)})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	switch method {
	case "getMe":
		answer(telegramUser{ID: 123456, IsBot: true, FirstName: "Helper", Username: "helper_bot"})
	case "getUpdates":
		if b.failPoll > 0 {
			b.failPoll--
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		if len(b.updates) == 0 {
			b.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			b.mu.Lock()
			answer([]telegramUpdate{})
			return
		}
		us := b.updates[0]
		b.updates = b.updates[1:]
		answer(us)
	case "sendMessage":
		if mode, _ := params["parse_mode"].(string); mode == "HTML" && strings.Contains(params["text"].(string), "unclosed") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities"})
			return
		}
		b.sent = append(b.sent, params)
		answer(map[string]any{"message_id": len(b.sent)})
	case "sendChatAction":
		b.actions = append(b.actions, params)
		answer(true)
	default:
		http.Error(w, "no", http.StatusNotFound)
	}
}

func (b *fakeBot) sentTo() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, s := range b.sent {
		out = append(out, s["chat_id"].(string))
	}
	return out
}

// roleUnderTest runs Serve against bot, with peer paired.
type roleUnderTest struct {
	in     *io.PipeWriter
	frames chan FromRole
	done   chan error
	cancel context.CancelFunc
}

func startRole(t *testing.T, bot *fakeBot, peer *Peer) *roleUnderTest {
	t.Helper()
	old := telegramPoll
	telegramPoll = 100 * time.Millisecond
	t.Cleanup(func() { telegramPoll = old })
	srv := httptest.NewServer(bot)
	t.Cleanup(srv.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := &roleUnderTest{in: inW, frames: make(chan FromRole, 64), done: make(chan error, 1), cancel: cancel}
	go func() {
		err := Serve(ctx, inR, outW, io.Discard, Config{API: srv.URL})
		_ = outW.Close()
		r.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), MaxLine)
		for sc.Scan() {
			f, err := ReadFromRole(sc.Bytes())
			if err != nil {
				t.Errorf("the role wrote a line that is not a frame: %s", sc.Text())
				continue
			}
			r.frames <- f
		}
		close(r.frames)
	}()
	t.Cleanup(func() { cancel(); _ = inW.Close() })
	r.write(t, ToRole{Start: &Start{Kind: KindTelegram, Token: testToken, Peer: peer}})
	return r
}

func (r *roleUnderTest) write(t *testing.T, f ToRole) {
	t.Helper()
	b, _ := json.Marshal(f)
	if _, err := r.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// next is the next frame that is not a state report.
func (r *roleUnderTest) next(t *testing.T) FromRole {
	t.Helper()
	for {
		select {
		case f, ok := <-r.frames:
			if !ok {
				t.Fatal("the role ended")
			}
			if f.State != nil {
				continue
			}
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("no frame from the role")
		}
	}
}

func direct(from, chat int64, text string) telegramMessage {
	return telegramMessage{From: &telegramUser{ID: from, FirstName: "x"}, Chat: telegramChat{ID: chat, Type: "private"}, Date: time.Now().Unix(), Text: text}
}

var paired = &Peer{Sender: "42", Chat: "42"}

func TestTheRoleSaysWhoTheBotIs(t *testing.T) {
	r := startRole(t, &fakeBot{}, nil)
	f := r.next(t)
	if f.Ready == nil || f.Ready.Account != "123456" || f.Ready.Name != "@helper_bot" {
		t.Fatalf("first frame = %+v, want the bot's ready line", f)
	}
}

func TestUnpairedSenderNeverReachesTheAgent(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, paired)
	r.next(t) // ready
	bot.queue(direct(7, 7, "hello, who are you?"), direct(42, 42, "it is me"))
	if f := r.next(t); f.Dropped == nil || f.Dropped.Reason != DroppedUnpaired {
		t.Fatalf("a stranger's message = %+v, want it dropped as unpaired", f)
	}
	if f := r.next(t); f.Inbound == nil || f.Inbound.Text != "it is me" || f.Inbound.Sender != "42" {
		t.Fatalf("the paired person's message = %+v, want it passed on", f)
	}
	if got := bot.sentTo(); len(got) != 0 {
		t.Fatalf("the role answered someone: %v", got)
	}
}

func TestAGroupMessageIsDroppedEvenFromThePairedPerson(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, paired)
	r.next(t)
	m := direct(42, -100, "in a group")
	m.Chat.Type = "group"
	bot.queue(m)
	if f := r.next(t); f.Dropped == nil || f.Dropped.Reason != DroppedNotDirect {
		t.Fatalf("a group message = %+v, want it dropped", f)
	}
}

func TestAPairingCodeGoesToTheSupervisorAndIsBounded(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, nil)
	r.next(t)
	bot.queue(direct(9, 9, "abcd-efgh"), direct(9, 9, "ABCDEFGH"), direct(9, 9, "ABCDEFGH"), direct(9, 9, "ABCDEFGH"))
	for i := range 3 {
		f := r.next(t)
		if f.Pairing == nil || f.Pairing.Code != "ABCDEFGH" || f.Pairing.Sender != "9" {
			t.Fatalf("code %d = %+v, want a pairing frame", i, f)
		}
	}
	if f := r.next(t); f.Dropped == nil || f.Dropped.Reason != DroppedPairings {
		t.Fatalf("the fourth code in a minute = %+v, want it dropped", f)
	}
}

func TestMessengerSendsOnlyToThePairedPerson(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, paired)
	r.next(t)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "7", Text: "to a stranger"}})
	if f := r.next(t); f.Sent == nil || f.Sent.Error == "" {
		t.Fatalf("a reply to another chat = %+v, want it refused", f)
	}
	r.write(t, ToRole{Reply: &Reply{ID: 2, Chat: "42", Text: "to you"}})
	if f := r.next(t); f.Sent == nil || f.Sent.Error != "" || f.Sent.Parts != 1 {
		t.Fatalf("a reply to the paired person = %+v, want it sent", f)
	}
	if got := bot.sentTo(); len(got) != 1 || got[0] != "42" {
		t.Fatalf("sent to %v, want the paired person's chat alone", got)
	}
}

func TestAReplyWhoseHTMLIsRefusedGoesPlain(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, paired)
	r.next(t)
	r.write(t, ToRole{Reply: &Reply{ID: 1, Chat: "42", Text: "```\n<unclosed>\n```"}})
	if f := r.next(t); f.Sent == nil || f.Sent.Error != "" {
		t.Fatalf("sent = %+v", f)
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) != 1 || bot.sent[0]["parse_mode"] != nil || bot.sent[0]["text"] != "<unclosed>" {
		t.Fatalf("sent %+v, want the part again as plain text", bot.sent)
	}
}

func TestPairedChangesWhoTheRolePassesOn(t *testing.T) {
	bot := &fakeBot{}
	r := startRole(t, bot, nil)
	r.next(t)
	r.write(t, ToRole{Paired: &Paired{Peer: paired}})
	time.Sleep(50 * time.Millisecond)
	bot.queue(direct(42, 42, "now paired"))
	if f := r.next(t); f.Inbound == nil || f.Inbound.Text != "now paired" {
		t.Fatalf("after pairing = %+v, want the message passed on", f)
	}
}

func TestTheRoleReconnectsAfterAFailedPoll(t *testing.T) {
	bot := &fakeBot{failPoll: 2}
	r := startRole(t, bot, paired)
	r.next(t)
	bot.queue(direct(42, 42, "after the outage"))
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f := <-r.frames:
			if f.Inbound != nil {
				return
			}
		case <-deadline:
			t.Fatal("the role never reconnected")
		}
	}
}

func TestARefusedTokenEndsTheRoleWithoutRepeatingIt(t *testing.T) {
	r := startRole(t, &fakeBot{refuse: true}, nil)
	select {
	case err := <-r.done:
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("Serve = %v, want a refusal that does not repeat the token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the role kept going on a refused token")
	}
}

func TestMessengerLinkTakesNoControlCommand(t *testing.T) {
	for _, line := range []string{
		`{"reply":{"id":1,"chat":"42","text":"x"},"typing":{"chat":"42"}}`,
		`{"stop":{}}`,
		`{"reply":{"id":1,"chat":"42","text":"x","tool":"grant"}}`,
		`{}`,
	} {
		if _, err := ReadToRole([]byte(line)); err == nil {
			t.Errorf("ReadToRole(%s) took it", line)
		}
	}
	for _, line := range []string{
		`{"inbound":{"chat":"1","sender":"1","text":"x"},"dropped":{"reason":"x"}}`,
		`{"change":{"switch":{"id":"commands","on":true}}}`,
		`{"inbound":{"chat":"1","sender":"1","text":"x","approve":true}}`,
	} {
		if _, err := ReadFromRole([]byte(line)); err == nil {
			t.Errorf("ReadFromRole(%s) took it", line)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"abcd efgh": "ABCDEFGH", "ABCD-EFGH": "ABCDEFGH", " 2345 6789 ": "23456789"} {
		if got, ok := NormalizeCode(in); !ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"ABCDEFG", "ABCDEFGHJ", "ABCDEFG0", "hello there", ""} {
		if _, ok := NormalizeCode(in); ok {
			t.Errorf("NormalizeCode(%q) took it", in)
		}
	}
}

func TestCheckNamesTheBotAndRefusesAnotherShape(t *testing.T) {
	srv := httptest.NewServer(&fakeBot{})
	defer srv.Close()
	got, err := Check(context.Background(), KindTelegram, "123456:"+strings.Repeat("A", 35), srv.URL)
	if err == nil {
		t.Fatalf("Check took a token the service does not know: %+v", got)
	}
	if _, err := Check(context.Background(), KindTelegram, "not a token", srv.URL); err == nil || !strings.Contains(err.Error(), "BotFather") {
		t.Fatalf("Check of a malformed token = %v", err)
	}
}
