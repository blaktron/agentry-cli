package persistent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// TestFastmailIsAskedAboutTheToken: the token goes as the bearer of an MCP
// initialize; an answer of 401 or 403 is Fastmail refusing it, a 2xx
// accepting it, anything else neither; a token that is not one word is
// refused before anything is sent.
func TestFastmailIsAskedAboutTheToken(t *testing.T) {
	const token = "fmu1-test-token-0123456789"
	status := http.StatusOK
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("asked %s with %v", r.Method, r.Header)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	ask := func(tok string) error {
		return fastmailTokenAccepted(context.Background(), srv.Client(), srv.URL, tok)
	}
	if err := ask(token); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status = s
		if err := ask(token); !errors.Is(err, ErrFastmailRefused) {
			t.Errorf("%d: %v", s, err)
		}
	}
	status = http.StatusBadGateway
	if err := ask(token); err == nil || errors.Is(err, ErrFastmailRefused) {
		t.Errorf("502: %v", err)
	}
	before := asked
	for _, bad := range []string{"", "  ", "two words"} {
		if err := ask(bad); err == nil {
			t.Errorf("%q was asked about", bad)
		}
	}
	if asked != before {
		t.Error("a malformed token was sent to Fastmail")
	}
}

// TestFastmailIsOnTheRosterOnlyWithItsProvider: Fastmail's offer reaches
// api.fastmail.com alone on the token it declares; with the provider
// disabled it is not on the roster, and switching it is refused; enabled
// without the token it waits, naming --fastmail-token; with the token kept
// and on, the segment is given it as a stored secret, which the declaring server alone receives.
func TestFastmailIsOnTheRosterOnlyWithItsProvider(t *testing.T) {
	withImageServers(t)
	withBuiltinOffers(t)
	home := shortHome(t)
	o := assistant.Fastmail()
	if err := o.Check(); err != nil {
		t.Fatal(err)
	}
	if u, err := url.Parse(o.Server.URL); err != nil || u.Scheme != "https" || u.Host != assistant.FastmailHost || len(o.Server.Egress) != 0 || !slices.Equal(o.Server.Secrets, []string{assistant.FastmailToken}) {
		t.Fatalf("Fastmail is %+v", o.Server)
	}
	id := ServerEntryID(o.Server)
	if id != "server:url-api-fastmail-com-mcp" {
		t.Fatalf("Fastmail's entry is %s", id)
	}
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true}
	if _, ok := BuildRoster(a, c, nil).Entry(id); ok {
		t.Fatal("Fastmail is on the roster with its provider disabled")
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err == nil || !strings.Contains(err.Error(), "--mail-provider fastmail") {
		t.Errorf("switching a disabled Fastmail: %v", err)
	}
	c.MailProviders = []string{assistant.MailFastmail}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	e, ok := BuildRoster(a, c, nil).Entry(id)
	if !ok || e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "agentry agent configure assistant --fastmail-token") {
		t.Fatalf("without the token: %+v", e)
	}
	if !strings.Contains(e.Line, "whatever your token allows") {
		t.Errorf("its sentence: %s", e.Line)
	}
	if ps := MailProviders(a, c); !ps[2].Enabled || !slices.Equal(ps[2].Servers, []string{id}) || len(ps[2].SetUp) != 1 {
		t.Errorf("the provider: %+v", ps[2])
	}

	const token = "fmu1-test-token-0123456789"
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: assistant.FastmailToken, Set: time.Now()}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	if e, _ := r.Entry(id); e.State != StateOn {
		t.Fatalf("with the token: %+v", e)
	}
	gr, _, done, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{SecretAccount(a.Name, assistant.FastmailToken): token})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if gr.Secrets[assistant.FastmailToken] != token || len(gr.ServerSecrets) != 0 {
		t.Errorf("the segment's secrets %v, server secrets %v", maskedKeys(gr.Secrets), maskedKeys(gr.ServerSecrets))
	}
	if ps := MailProviders(a, c); len(ps[2].SetUp) != 0 {
		t.Errorf("set up, the provider still waits: %+v", ps[2])
	}
}

// withBuiltinOffers has the built-ins' own offers for the test.
func withBuiltinOffers(t *testing.T) {
	t.Helper()
	old := offersOf
	offersOf = func(a Agent, c Config) []offer.Server { return builtinOffers(a, c) }
	t.Cleanup(func() { offersOf = old })
}

// TestResetKeepsOrRemovesTheFastmailToken: Reset agent keeps the token in
// the keychain and its name, switched off, with Fastmail disabled; Reset
// all removes it from the keychain.
func TestResetKeepsOrRemovesTheFastmailToken(t *testing.T) {
	withBuiltinOffers(t)
	const token = "fmu1-test-token-0123456789"
	account := SecretAccount("assistant", assistant.FastmailToken)
	id := ServerEntryID(assistant.Fastmail().Server)
	setUp := func(t *testing.T) (string, fakeSecrets) {
		t.Helper()
		home := shortHome(t)
		c := Config{Enabled: true, MailProviders: []string{assistant.MailFastmail},
			Secrets:  []StoredSecret{{Name: assistant.FastmailToken}},
			Switches: map[string]bool{id: true, SecretEntryID(assistant.FastmailToken): true}}
		if err := SaveConfig(home, "assistant", c); err != nil {
			t.Fatal(err)
		}
		keys := fakeSecrets{account: token}
		old := resetSecrets
		resetSecrets = keys
		t.Cleanup(func() { resetSecrets = old })
		return home, keys
	}

	home, keys := setUp(t)
	if err := ResetAgent(context.Background(), home, "assistant", &dockerx.Fake{}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if keys[account] != token || len(after.Secrets) != 1 || len(after.MailProviders) != 0 {
		t.Errorf("after Reset agent: keychain %v, config %+v", maskedKeys(keys), after)
	}
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := BuildRoster(a, after, nil).Entry(id); ok {
		t.Error("Fastmail is on the roster after Reset agent")
	}
	after.MailProviders = []string{assistant.MailFastmail}
	if e, _ := BuildRoster(a, after, nil).Entry(id); e.On {
		t.Errorf("Fastmail enabled again is on: %+v", e)
	}

	home, keys = setUp(t)
	if err := Reset(context.Background(), home, "assistant", &dockerx.Fake{}); err != nil {
		t.Fatal(err)
	}
	if _, kept := keys[account]; kept {
		t.Error("Reset kept the Fastmail token in the keychain")
	}
}

// TestAFastmailThatDidNotStartNamesTheFix: a start that failed because
// Fastmail's server did not start (its token refused) says to give the
// token again or switch the server off; another server's failure, or
// another failure, is left as it is.
func TestAFastmailThatDidNotStartNamesTheFix(t *testing.T) {
	withBuiltinOffers(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{MailProviders: []string{assistant.MailFastmail}}
	failed := func(ref string) error {
		return receipt.WithReason(errors.New("declared MCP server "+ref+" did not start; the gateway said: > Can't start …"), receipt.ReasonMCPStartFailed, "")
	}
	got := withServerFix(a, c, failed(assistant.Fastmail().Server.URL))
	if !strings.Contains(got.Error(), "agentry agent configure assistant --fastmail-token") || !strings.Contains(got.Error(), "agentry agent grant assistant server:url-api-fastmail-com-mcp off") || receipt.ReasonOf(got) == nil {
		t.Errorf("Fastmail's: %v", got)
	}
	for _, other := range []error{failed("@brave/brave-search-mcp-server@2.0.0"), errors.New("another failure")} {
		if got := withServerFix(a, c, other); got != other {
			t.Errorf("%v became %v", other, got)
		}
	}
}
