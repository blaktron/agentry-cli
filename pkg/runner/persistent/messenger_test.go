package persistent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

const (
	botToken    = "4242:AAbot-token-for-tests-only"
	telegramBot = "4242"
	you         = "777"
)

// fakeRole is a messenger role: the frames a test writes, and what the
// supervisor sent it.
type fakeRole struct {
	mu     sync.Mutex
	frames chan messenger.FromRole
	got    []messenger.ToRole
	once   sync.Once
}

func (f *fakeRole) Ready() messenger.Ready {
	return messenger.Ready{Account: telegramBot, Name: "@helper_bot"}
}
func (f *fakeRole) Frames() <-chan messenger.FromRole { return f.frames }
func (f *fakeRole) Send(t messenger.ToRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, t)
	return nil
}
func (f *fakeRole) Close(context.Context) { f.once.Do(func() { close(f.frames) }) }

func (f *fakeRole) replies() []messenger.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []messenger.Reply
	for _, t := range f.got {
		if t.Reply != nil {
			out = append(out, *t.Reply)
		}
	}
	return out
}

func (f *fakeRole) paired() []messenger.Paired {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []messenger.Paired
	for _, t := range f.got {
		if t.Paired != nil {
			out = append(out, *t.Paired)
		}
	}
	return out
}

func (f *fakeRole) send(text string) {
	f.frames <- messenger.FromRole{Inbound: &messenger.Inbound{Chat: you, Sender: you, Text: text}}
}

// roles starts fake roles and records what each start was given.
type roles struct {
	mu     sync.Mutex
	starts []MessengerStart
	all    []*fakeRole
}

func (r *roles) start(_ context.Context, st MessengerStart) (MessengerConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := &fakeRole{frames: make(chan messenger.FromRole, 64)}
	r.starts, r.all = append(r.starts, st), append(r.all, f)
	return f, nil
}

func (r *roles) last(t *testing.T) *fakeRole {
	t.Helper()
	var f *fakeRole
	eventually(t, "a messenger role started", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.all) == 0 {
			return false
		}
		f = r.all[len(r.all)-1]
		return true
	})
	return f
}

func (r *roles) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.all)
}

// withTelegram enables the Assistant with a Telegram account added and
// agreed to, paired with you when paired says so; keys holds its token.
func withTelegram(t *testing.T, home string, paired bool) fakeSecrets {
	t.Helper()
	enable(t, home)
	a := assistantAgent(t)
	c, err := LoadConfig(home, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	m := MessengerAccount{Kind: messenger.KindTelegram, Account: telegramBot, Name: "@helper_bot", Set: time.Now().UTC()}
	if _, err := ApplyChange(home, a, &c, Change{SetMessenger: &m, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if paired {
		c.Messengers[0].Peer = &MessengerPeer{Sender: you, Chat: you, Paired: time.Now().UTC()}
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	return fakeSecrets{MessengerKeychainAccount(a.Name, messenger.KindTelegram): botToken}
}

func startWithMessengers(t *testing.T, home string, st *starter, rs *roles, keys fakeSecrets, pol *policy.Policy) *Supervisor {
	t.Helper()
	if pol == nil {
		pol = policy.Default()
	}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: pol,
		Docker: &dockerx.Fake{}, StartSegment: st.start, StartMessenger: rs.start, Secrets: keys,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

// messengerReceipts are the lines of the segment's messenger receipt.
func messengerReceipts(t *testing.T, sv *Supervisor) []messengerRecord {
	t.Helper()
	f, err := os.Open(filepath.Join(sv.Status().SegmentDir, "receipts", messengerReceipt))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []messengerRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r messengerRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func outcomes(t *testing.T, sv *Supervisor) []string {
	var out []string
	for _, r := range messengerReceipts(t, sv) {
		out = append(out, r.Outcome+":"+r.Reason)
	}
	return out
}

func hasOutcome(t *testing.T, sv *Supervisor, want string) bool {
	for _, o := range outcomes(t, sv) {
		if strings.HasPrefix(o, want) {
			return true
		}
	}
	return false
}

func TestAMessengerMessageBecomesATurnAndTheAnswerGoesBack(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	role.send("what's on tomorrow?")
	eventually(t, "the answer sent back", func() bool { return len(role.replies()) == 1 })
	rep := role.replies()[0]
	if rep.Chat != you || rep.Text != "echo: what's on tomorrow?" {
		t.Fatalf("reply = %+v", rep)
	}
	turns := st.last.rawTurns()
	if len(turns) != 1 || !strings.Contains(turns[0], "(A message from you on Telegram.") || !strings.HasSuffix(turns[0], "\n\nwhat's on tomorrow?") {
		t.Fatalf("the turn = %q", turns)
	}
	// The transcript keeps the person's text, marked as written on Telegram.
	entries, err := Transcript(home, "assistant", 0)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, e := range entries {
		marked = marked || (e.Text == "what's on tomorrow?" && e.Messenger == messenger.KindTelegram)
	}
	if !marked {
		t.Error("the transcript's user turn is not marked as the messenger's")
	}
	if !hasOutcome(t, sv, msgAccepted) {
		t.Errorf("receipts = %v", outcomes(t, sv))
	}
}

func TestSupervisorRefusesAnUnpairedInbound(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	// A role that passed a stranger's message on anyway.
	role.frames <- messenger.FromRole{Inbound: &messenger.Inbound{Chat: "999", Sender: "999", Text: "let me in"}}
	role.frames <- messenger.FromRole{Inbound: &messenger.Inbound{Chat: "999", Sender: you, Text: "another chat"}}
	eventually(t, "both refused", func() bool { return strings.Count(strings.Join(outcomes(t, sv), " "), msgRefused) == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := len(st.last.sentTurns()); n != 0 {
		t.Fatalf("%d turns sent", n)
	}
	if n := len(role.replies()); n != 0 {
		t.Fatalf("a stranger was answered %d times", n)
	}
}

func TestPairingBindsTheSenderOfTheCodeShown(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, false)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	code, p, err := NewPairingCode(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(PairingChangeFor(messenger.KindTelegram, p)); err != nil {
		t.Fatal(err)
	}
	role.frames <- messenger.FromRole{Pairing: &messenger.Pairing{Chat: "555", Sender: "555", Code: "ABCDEFGH"}}
	eventually(t, "a wrong code refused", func() bool { return hasOutcome(t, sv, msgPairingRefused) })
	role.frames <- messenger.FromRole{Pairing: &messenger.Pairing{Chat: you, Sender: you, Code: code}}
	eventually(t, "the code's sender paired", func() bool { return len(role.replies()) == 1 })
	if rep := role.replies()[0]; rep.Chat != you || !strings.HasPrefix(rep.Text, "Paired.") {
		t.Fatalf("reply = %+v", rep)
	}
	if ps := role.paired(); len(ps) != 1 || ps[0].Peer == nil || ps[0].Peer.Sender != you {
		t.Fatalf("the role was told %+v", ps)
	}
	c, _ := LoadConfig(home, "assistant")
	if m := c.Messengers[0]; m.Peer == nil || m.Peer.Sender != you || m.Pairing != nil {
		t.Fatalf("config = %+v", m)
	}
	// The code is taken once.
	role.frames <- messenger.FromRole{Pairing: &messenger.Pairing{Chat: "555", Sender: "555", Code: code}}
	eventually(t, "the code refused a second time", func() bool {
		return strings.Count(strings.Join(outcomes(t, sv), " "), msgPairingRefused) == 2
	})
}

func TestAnExpiredPairingCodeIsRefused(t *testing.T) {
	p := MessengerPairing{CodeDigest: codeDigest("ABCDEFGH"), Until: time.Now().Add(-time.Second)}
	if p.takes("ABCDEFGH", time.Now()) {
		t.Fatal("an expired code was taken")
	}
	p.Until = time.Now().Add(time.Minute)
	if !p.takes("ABCDEFGH", time.Now()) || p.takes("ABCDEFGJ", time.Now()) {
		t.Fatal("the code's judgement is wrong")
	}
}

func TestMessengerSendsOnlyToThePairedPerson(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	sv.mu.Lock()
	r := sv.msgs.roles[messenger.KindTelegram]
	sv.mu.Unlock()
	eventually(t, "connected", func() bool { c, _ := r.current(); return c != nil })
	sv.messengerReply(r, "999", "to a stranger")
	if n := len(role.replies()); n != 0 {
		t.Fatalf("sent to another chat: %+v", role.replies())
	}
	if !hasOutcome(t, sv, msgNotSent+":not the paired person's chat") {
		t.Errorf("receipts = %v", outcomes(t, sv))
	}
}

func TestAMessageDuringATurnWaitsAndTheWaitingGoTogether(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	hold := make(chan struct{})
	st, rs := &starter{hold: hold}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(context.Background(), "the person in Agentry") }()
	eventually(t, "the person's turn in flight", func() bool { return len(st.last.sentTurns()) == 1 })
	role.send("first")
	role.send("second")
	eventually(t, "both queued", func() bool { return strings.Count(strings.Join(outcomes(t, sv), " "), msgAccepted) == 2 })
	close(hold)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	eventually(t, "one turn for both", func() bool { return len(st.last.sentTurns()) == 2 })
	turn := st.last.rawTurns()[1]
	if !strings.Contains(turn, "(2 messages from you on Telegram, sent while you were busy, in order.") || !strings.HasSuffix(turn, "first\n\nsecond") {
		t.Fatalf("the waiting turn = %q", turn)
	}
	eventually(t, "the answer sent back", func() bool { return len(role.replies()) == 1 })
}

func TestMessengerQueueIsBounded(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	hold := make(chan struct{})
	defer close(hold)
	st, rs := &starter{hold: hold}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	go func() { _ = sv.Send(context.Background(), "busy") }()
	eventually(t, "a turn in flight", func() bool { return len(st.last.sentTurns()) == 1 })
	for range MaxMessengerQueue + 1 {
		role.send("again")
	}
	eventually(t, "the one over the bound refused", func() bool { return hasOutcome(t, sv, msgDropped+":queue-full") })
	eventually(t, "told so", func() bool { return len(role.replies()) == 1 })
	if reps := role.replies(); len(reps) != 1 || !strings.Contains(reps[0].Text, "not taken") {
		t.Fatalf("replies = %+v", reps)
	}
	sv.mu.Lock()
	n := len(sv.msgs.queue)
	sv.mu.Unlock()
	if n != MaxMessengerQueue {
		t.Fatalf("%d waiting, want %d", n, MaxMessengerQueue)
	}
}

func TestStopFromAMessengerInterruptsTheTurn(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	hold := make(chan struct{})
	defer close(hold)
	st, rs := &starter{hold: hold}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(context.Background(), "a long job") }()
	eventually(t, "a turn in flight", func() bool { return len(st.last.sentTurns()) == 1 })
	role.send("/stop")
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("the turn was not stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn kept going")
	}
	eventually(t, "told it stopped", func() bool { return len(role.replies()) == 1 })
	if r := role.replies()[0]; r.Text != "Stopped." {
		t.Fatalf("reply = %q", r.Text)
	}
	if n := len(st.last.sentTurns()); n != 1 {
		t.Fatalf("/stop became a turn: %v", st.last.sentTurns())
	}
}

func TestApprovalNeverAnsweredFromAMessenger(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	var sv *Supervisor
	asked := make(chan error, 1)
	st.answer = func(text string) string {
		if text == "install it" {
			asked <- sv.Ask(context.Background(), "Install the helper tool?", "It runs on this computer.")
		}
		return "done: " + text
	}
	sv = startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	role.send("install it")
	if err := <-asked; !errors.Is(err, ErrNobodyToAsk) {
		t.Fatalf("Ask = %v, want it asked in Agentry alone", err)
	}
	eventually(t, "the messenger told, then answered", func() bool { return len(role.replies()) == 2 })
	if r := role.replies()[0]; !strings.Contains(r.Text, "asks for your agreement in Agentry") || !strings.Contains(r.Text, "Nothing is approved from Telegram") {
		t.Fatalf("the messenger was told %q", r.Text)
	}
}

func TestMessengerReplyMaskedBeforeSplit(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	// The token sits across where a cut would fall.
	st.answer = func(string) string { return strings.Repeat("a", 4090) + botToken + strings.Repeat("b", 100) }
	startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	role.send("show me")
	eventually(t, "the answer sent back", func() bool { return len(role.replies()) == 1 })
	text := role.replies()[0].Text
	if strings.Contains(text, botToken) || !strings.Contains(text, "[redacted]") {
		t.Fatal("the answer was not masked whole")
	}
	parts, _ := messenger.SplitAs(messenger.FormatHTML, text, 4096)
	for _, p := range parts {
		for n := 8; n <= len(botToken); n++ {
			if strings.Contains(p.Plain, botToken[:n]) || strings.Contains(p.Plain, botToken[len(botToken)-n:]) {
				t.Fatalf("a part holds %d characters of the token", n)
			}
		}
	}
}

func TestMessengerOutboundRateCapped(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	sv.mu.Lock()
	r := sv.msgs.roles[messenger.KindTelegram]
	sv.mu.Unlock()
	eventually(t, "connected", func() bool { c, _ := r.current(); return c != nil })
	role.frames <- messenger.FromRole{Sent: &messenger.Sent{ID: 1, Parts: MaxMessengerSentAnHour}}
	eventually(t, "counted", func() bool { return hasOutcome(t, sv, msgSent) })
	role.send("one more")
	eventually(t, "the answer capped", func() bool { return hasOutcome(t, sv, msgCapped) })
	if n := len(role.replies()); n != 0 {
		t.Fatalf("sent %d over the cap", n)
	}
}

func TestMessengerReceiptsEveryMessage(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	role.frames <- messenger.FromRole{Dropped: &messenger.Dropped{Reason: messenger.DroppedUnpaired}}
	role.send("a secret plan")
	eventually(t, "answered", func() bool { return len(role.replies()) == 1 })
	role.frames <- messenger.FromRole{Sent: &messenger.Sent{ID: 1, Parts: 1}}
	eventually(t, "every line", func() bool { return len(messengerReceipts(t, sv)) == 3 })
	got := outcomes(t, sv)
	if got[0] != msgDropped+":"+messenger.DroppedUnpaired || got[1] != msgAccepted+":" || got[2] != msgSent+":" {
		t.Fatalf("receipts = %v", got)
	}
	b, _ := os.ReadFile(filepath.Join(sv.Status().SegmentDir, "receipts", messengerReceipt))
	if strings.Contains(string(b), "secret plan") || strings.Contains(string(b), you) {
		t.Fatal("the receipt holds a message's text or a sender")
	}
}

func TestPolicyRefusesAMessenger(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	off := false
	pol := policy.Default()
	pol.AllowMessengers = &off
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, pol)
	time.Sleep(100 * time.Millisecond)
	if rs.count() != 0 {
		t.Fatal("a messenger the policy refuses was started")
	}
	ms := sv.Status().Messengers
	if len(ms) != 1 || !strings.Contains(ms[0].Refused, "allowMessengers: false") {
		t.Fatalf("status = %+v", ms)
	}
}

func TestMessengerTokenReachesTheRoleAlone(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	startWithMessengers(t, home, st, rs, keys, nil)
	rs.last(t)
	if rs.starts[0].Token != botToken {
		t.Fatal("the role was not given the token")
	}
	st.mu.Lock()
	given := fmt.Sprintf("%+v", st.segments)
	st.mu.Unlock()
	if !strings.Contains(given, "assistant") {
		t.Fatalf("the segments' dump is not the segments: %.200s", given)
	}
	if strings.Contains(given, botToken) {
		t.Fatal("the agent's segment carries the messenger's token")
	}
	c, _ := os.ReadFile(filepath.Join(Dir(home, "assistant"), configFile))
	if strings.Contains(string(c), botToken) {
		t.Fatal("config.yaml holds the token")
	}
	if !strings.Contains(string(c), "version: 12\n") {
		t.Fatal("a configuration with a messenger is not written at the version that knows them")
	}
}

func TestSwitchingAMessengerRestartsNothingElse(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	first := rs.last(t)
	if _, err := sv.Change(Change{Switch: &Switch{ID: MessengerEntryID(messenger.KindTelegram), On: false}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the role stopped", func() bool {
		select {
		case _, ok := <-first.frames:
			return !ok
		default:
			return false
		}
	})
	if _, err := sv.Change(Change{Switch: &Switch{ID: MessengerEntryID(messenger.KindTelegram), On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a role again", func() bool { return rs.count() == 2 })
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n := len(st.segments)
	st.mu.Unlock()
	if n != 1 {
		t.Fatalf("switching the messenger started %d segments", n)
	}
}

func TestMessengerAccountBindsOneAgent(t *testing.T) {
	home := shortHome(t)
	withTelegram(t, home, false)
	coder, err := Lookup("coder")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true}
	m := MessengerAccount{Kind: messenger.KindTelegram, Account: telegramBot, Set: time.Now()}
	if _, err := ApplyChange(home, coder, &c, Change{SetMessenger: &m, Consent: true}); err == nil || !strings.Contains(err.Error(), "the assistant agent's") {
		t.Fatalf("a second agent took the account: %v", err)
	}
	m.Account = "5151"
	if _, err := ApplyChange(home, coder, &c, Change{SetMessenger: &m, Consent: true}); err != nil {
		t.Fatalf("an account of its own: %v", err)
	}
}

func TestMainTakesNoMessenger(t *testing.T) {
	home := shortHome(t)
	main, err := Lookup("main")
	if err != nil {
		t.Skip("no main agent in this build")
	}
	c := Config{Enabled: true}
	m := MessengerAccount{Kind: messenger.KindTelegram, Account: telegramBot, Set: time.Now()}
	if err := addMessenger(main, &c, m); err == nil {
		t.Fatal("Main took a messenger")
	}
	_ = home
}

func TestAMessengerEntryRendersAgree(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	c, _ := LoadConfig(home, a.Name)
	m := MessengerAccount{Kind: messenger.KindTelegram, Account: telegramBot, Name: "@helper_bot", Set: time.Now()}
	_, err := ApplyChange(home, a, &c, Change{SetMessenger: &m})
	var ce *ConsentError
	if !errors.As(err, &ce) || len(ce.Entries) != 1 {
		t.Fatalf("adding a messenger asks nothing: %v", err)
	}
	line := ce.Entries[0].Line
	for _, want := range []string{"Telegram", "@helper_bot", "does not encrypt them end to end", "Nothing is approved from Telegram", "api.telegram.org"} {
		if !strings.Contains(line, want) {
			t.Errorf("the sentence does not say %q: %s", want, line)
		}
	}
	if _, err := ApplyChange(home, a, &c, Change{SetMessenger: &m, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	e, ok := r.Entry(MessengerEntryID(messenger.KindTelegram))
	if !ok || e.Line != line || e.Kind != KindMessenger || e.GrantedBy != GrantedByYou || !e.On {
		t.Fatalf("the roster's entry = %+v", e)
	}
	for _, re := range r.Receipt() {
		if re.ID == e.ID && re.Line != line {
			t.Fatal("the receipt's line is not the roster's")
		}
	}
}

func TestWrongCodesCloseThePairing(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, false)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	code, p, err := NewPairingCode(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A client that asks for a longer pairing gets the time a code is valid.
	p.Until = time.Now().Add(24 * time.Hour)
	if _, err := sv.Change(PairingChangeFor(messenger.KindTelegram, p)); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig(home, "assistant")
	if until := c.Messengers[0].Pairing.Until; until.After(time.Now().Add(PairingValidFor + 2*time.Minute)) {
		t.Fatalf("the pairing runs until %v", until)
	}
	wrong := "ABCDEFGH"
	if wrong == code {
		wrong = "HGFEDCBA"
	}
	for range MaxPairingTries {
		role.frames <- messenger.FromRole{Pairing: &messenger.Pairing{Chat: "555", Sender: "555", Code: wrong}}
	}
	eventually(t, "the pairing closed", func() bool { return hasOutcome(t, sv, msgPairingRefused+":the pairing closed") })
	role.frames <- messenger.FromRole{Pairing: &messenger.Pairing{Chat: you, Sender: you, Code: code}}
	eventually(t, "the right code refused once closed", func() bool {
		return strings.Count(strings.Join(outcomes(t, sv), " "), msgPairingRefused) == MaxPairingTries+1
	})
	if c, _ := LoadConfig(home, "assistant"); c.Messengers[0].Peer != nil {
		t.Fatal("a closed pairing paired someone")
	}
}

func TestAMessagePassedOnTwiceIsOneTurn(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	st, rs := &starter{}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	in := messenger.FromRole{Inbound: &messenger.Inbound{ID: 7, Chat: you, Sender: you, Text: "once"}}
	role.frames <- in
	role.frames <- in
	eventually(t, "the second one dropped", func() bool { return hasOutcome(t, sv, msgDropped+":seen") })
	eventually(t, "answered", func() bool { return len(role.replies()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(st.last.sentTurns()); n != 1 {
		t.Fatalf("%d turns", n)
	}
}

func TestSwitchingAMessengerOffDropsItsWaitingMessages(t *testing.T) {
	home := shortHome(t)
	keys := withTelegram(t, home, true)
	hold := make(chan struct{})
	st, rs := &starter{hold: hold}, &roles{}
	sv := startWithMessengers(t, home, st, rs, keys, nil)
	role := rs.last(t)
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(context.Background(), "busy") }()
	eventually(t, "a turn in flight", func() bool { return len(st.last.sentTurns()) == 1 })
	role.send("waiting")
	eventually(t, "queued", func() bool { return hasOutcome(t, sv, msgAccepted) })
	if _, err := sv.Change(Change{Switch: &Switch{ID: MessengerEntryID(messenger.KindTelegram), On: false}}); err != nil {
		t.Fatal(err)
	}
	close(hold)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(st.last.sentTurns()); n != 1 {
		t.Fatalf("a switched-off messenger's message became a turn: %v", st.last.sentTurns())
	}
}

func TestAMatrixMessengerNamesItsHomeserverAndNeedsOne(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	c, _ := LoadConfig(home, a.Name)
	m := MessengerAccount{Kind: messenger.KindMatrix, Account: "@agent:example.org", Name: "@agent:example.org", Set: time.Now()}
	if _, err := ApplyChange(home, a, &c, Change{SetMessenger: &m, Consent: true}); err == nil {
		t.Fatal("a Matrix account with no homeserver was taken")
	}
	m.Server = "https://matrix.example.org"
	_, err := ApplyChange(home, a, &c, Change{SetMessenger: &m})
	var ce *ConsentError
	if !errors.As(err, &ce) || len(ce.Entries) != 1 {
		t.Fatalf("adding a messenger asks nothing: %v", err)
	}
	line := ce.Entries[0].Line
	for _, want := range []string{"Matrix", "your homeserver, matrix.example.org, carries them and does not encrypt them end to end, so it can read", "reaches matrix.example.org and nothing else"} {
		if !strings.Contains(line, want) {
			t.Errorf("the sentence does not say %q: %s", want, line)
		}
	}
	tg := MessengerAccount{Kind: messenger.KindTelegram, Account: telegramBot, Server: "https://ignored.example.org", Set: time.Now()}
	if _, err := ApplyChange(home, a, &c, Change{SetMessenger: &tg, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := MessengerOf(c, messenger.KindTelegram); got.Server != "" {
		t.Fatalf("a Telegram account kept a server: %+v", got)
	}
}
