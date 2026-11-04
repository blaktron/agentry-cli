package persistent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
)

// TestMicrosoftSettingsReachARunningAgent: the signoff and the Entra client
// given to a running Assistant, its Microsoft 365 signed in and on, are
// pending and reach the server at the next turn.
func TestMicrosoftSettingsReachARunningAgent(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	c, _ := LoadConfig(home, a.Name)
	c.MailProviders = []string{"microsoft"}
	c.SignIns = map[string]ImageSignIn{"ms365": {ClientID: testEntraClient, Tenant: TenantOrganizations, Account: "person@contoso.com", At: time.Now()}}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "server:ms365", On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, fakeSecrets{ImageKeyringAccount("assistant", "ms365"): "c2VhbGluZy1rZXk="}, nil, Contents{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	m := Microsoft{Home: home, Agent: a}
	if err := m.SetSignoff("Sent by my assistant"); err != nil {
		t.Fatalf("a signoff on a running agent: %v", err)
	}
	if e, _ := sv.Roster().Entry("server:ms365"); e.State != StatePending || !sv.Status().Pending {
		t.Fatalf("the signoff is not pending: %+v", e)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	g := st.segments[len(st.segments)-1].Grants.ImageServers
	if len(st.segments) != 2 || len(g) != 1 || !strings.Contains(strings.Join(g[0].Args, " "), "Sent by my assistant") {
		t.Fatalf("segments %d; the server was given %+v", len(st.segments), g)
	}
	other := "00000000-0000-4000-8000-0000000000aa"
	if err := m.SetClient(other, ""); err != nil {
		t.Fatalf("an Entra client on a running agent: %v", err)
	}
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadConfig(home, a.Name); len(st.segments) != 3 || c.SignIns["ms365"].ClientID != other {
		t.Errorf("segments %d; the client kept %q", len(st.segments), c.SignIns["ms365"].ClientID)
	}
}

// TestDisablingARunningAgentStopsIt: disabling a running agent stops it,
// then turns it off; enabling a running one changes nothing.
func TestDisablingARunningAgentStopsIt(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	sv := startAssistant(t, home, &starter{resumed: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := SetEnabled(ctx, home, a, ClientCLI, true); err != nil {
		t.Fatalf("enabling a running agent: %v", err)
	}
	if err := SetEnabled(ctx, home, a, ClientCLI, false); err != nil {
		t.Fatalf("disabling a running agent: %v", err)
	}
	select {
	case <-sv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept running")
	}
	if c, _ := LoadConfig(home, a.Name); c.Enabled {
		t.Error("the agent is still enabled")
	}
}

// TestAFastmailTokenAndACalendarFeedReachARunningAgent: the Fastmail token
// with its provider, as one change, and a calendar feed, given to a running
// Assistant, each start the next turn's segment with them.
func TestAFastmailTokenAndACalendarFeedReachARunningAgent(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	keys := fakeSecrets{
		SecretAccount("assistant", assistant.FastmailToken): "fmu1-test-token",
		CalendarKeychainAccount("assistant", "work"):        "https://calendar.example.com/feed.ics",
	}
	st := &starter{resumed: true}
	sv := startKeyed(t, home, st, keys, nil, Contents{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]Change{
		"the Fastmail token": {SetSecret: &StoredSecret{Name: assistant.FastmailToken, Set: time.Now()}, MailProvider: &MailProviderSwitch{Key: assistant.MailFastmail, On: true}, Consent: true},
		"a calendar feed":    {SetCalendarFeed: &CalendarFeed{Name: "work", Host: "calendar.example.com", Set: time.Now()}, Consent: true},
	} {
		before := len(st.segments)
		if _, err := sv.Change(ch); err != nil {
			t.Fatalf("%s on a running agent: %v", name, err)
		}
		if !sv.Status().Pending {
			t.Fatalf("%s is not pending", name)
		}
		if err := sv.Send(ctx, "next"); err != nil {
			t.Fatal(err)
		}
		if len(st.segments) != before+1 {
			t.Errorf("%s: %d segments, want %d", name, len(st.segments), before+1)
		}
	}
	c, _ := LoadConfig(home, "assistant")
	if !slices.Contains(c.MailProviders, assistant.MailFastmail) || len(c.CalendarFeeds) != 1 {
		t.Errorf("the config: providers %v, feeds %v", c.MailProviders, c.CalendarFeeds)
	}
}

// TestEnablingAnAgentAnotherHolderHoldsIsRefused: enabling a disabled agent
// whose lock something other than a supervisor holds (a sign-in) is
// refused naming the holder, rather than reported done.
func TestEnablingAnAgentAnotherHolderHoldsIsRefused(t *testing.T) {
	home := shortHome(t)
	a := assistantAgent(t)
	owner, err := Acquire(home, a.Name, ClientDesktop)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	var held *HeldError
	if err := SetEnabled(context.Background(), home, a, ClientCLI, true); !errors.As(err, &held) {
		t.Fatalf("enabling a held, disabled agent: %v", err)
	}
}
