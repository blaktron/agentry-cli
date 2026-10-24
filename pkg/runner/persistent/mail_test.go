package persistent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestTheAssistantsMailProvidersHoldToTheirRules: Google and Microsoft are
// the Assistant's providers, gog and ms365 their servers; the Coding agent and Main have none; a
// provider naming a server not offered, a server two providers name, or a
// key of the wrong shape is refused.
func TestTheAssistantsMailProvidersHoldToTheirRules(t *testing.T) {
	withImageServers(t)
	servers := assistant.ImageServers("img", "img")
	if err := offer.CheckMailProviders(assistant.MailProviders(), servers); err != nil {
		t.Fatal(err)
	}
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	if ps := MailProviders(a, Config{}); len(ps) != 2 || ps[0].Key != "google" || ps[0].Enabled || !slices.Equal(ps[0].Servers, []string{"server:gog"}) || len(ps[0].SetUp) != 1 ||
		ps[1].Key != "microsoft" || ps[1].Enabled || !slices.Equal(ps[1].Servers, []string{"server:ms365"}) || len(ps[1].SetUp) != 1 {
		t.Fatalf("the Assistant's providers: %+v", ps)
	}
	for _, name := range []string{"coder", "main"} {
		b, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if ps := MailProviders(b, Config{}); len(ps) != 0 {
			t.Errorf("%s has mail providers: %+v", name, ps)
		}
	}
	for _, bad := range [][]offer.MailProvider{
		{{Key: "google", Name: "Google", ImageServers: []string{"nope"}}},
		{{Key: "google", Name: "Google", ImageServers: []string{"gog"}}, {Key: "other", Name: "Other", ImageServers: []string{"gog"}}},
		{{Key: "Google", Name: "Google", ImageServers: []string{"gog"}}},
		{{Key: "google", Name: "Google"}},
		{{Key: "google", Name: "Google", ImageServers: []string{"gog"}}, {Key: "google", Name: "Google", ImageServers: []string{"gog"}}},
	} {
		if err := offer.CheckMailProviders(bad, servers); err == nil {
			t.Errorf("%+v was not refused", bad)
		}
	}
}

// TestAConfigurationFromBeforeMailProvidersIsMigrated: an Assistant whose
// gog is signed in, or has its client, comes up with Google enabled, at
// whichever version it was written; one with neither comes up with none,
// as a new one does; a configuration written with providers keeps the
// person's choice, Google disabled included.
func TestAConfigurationFromBeforeMailProvidersIsMigrated(t *testing.T) {
	home := shortHome(t)
	write := func(body string) {
		t.Helper()
		dir := Dir(home, "assistant")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"signed in, version 8", "version: 8\nenabled: true\nsignIns:\n  gog:\n    account: person@example.com\n", []string{"google"}},
		{"its client alone, version 2", "version: 2\nenabled: true\nsignIns:\n  gog:\n    client: 2026-10-20T10:00:00Z\n", []string{"google"}},
		{"nothing set up, version 6", "version: 6\nenabled: true\nswitches:\n  server:gog: true\n", nil},
		{"none yet", "", nil},
		{"disabled by the person, version 9", "version: 9\nenabled: true\nsignIns:\n  gog:\n    account: person@example.com\n", nil},
		{"enabled, version 9", "version: 9\nenabled: true\nmailProviders: [google]\n", []string{"google"}},
	} {
		if tc.body == "" {
			_ = os.Remove(filepath.Join(Dir(home, "assistant"), "config.yaml"))
		} else {
			write(tc.body)
		}
		c, err := LoadConfig(home, "assistant")
		if err != nil || !slices.Equal(c.MailProviders, tc.want) {
			t.Errorf("%s: providers %v, %v; want %v", tc.name, c.MailProviders, err, tc.want)
			continue
		}
		// Saved and read again, the migration holds, and a sign-in is
		// written at the providers' version.
		if err := SaveConfig(home, "assistant", c); err != nil {
			t.Fatal(err)
		}
		again, err := LoadConfig(home, "assistant")
		if err != nil || !slices.Equal(again.MailProviders, tc.want) {
			t.Errorf("%s, saved: providers %v, %v", tc.name, again.MailProviders, err)
		}
		if v := readVersion(t, home, "assistant"); len(c.SignIns) > 0 && v != ConfigVersion {
			t.Errorf("%s: a sign-in saved at version %d", tc.name, v)
		}
	}
}

// TestADisabledProviderIsOffTheRosterAndOutOfTheSegment: Google disabled,
// gog and its switches are not on the roster, a segment is given nothing
// of it, and switching it names the fix; enabled again, it is back as it
// was left, signed in and agreed to, with no new set-up.
func TestADisabledProviderIsOffTheRosterAndOutOfTheSegment(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, MailProviders: googleOn, SignIns: map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now(), Granted: []string{"writes"}}}}
	for _, id := range []string{"server:gog", "server:gog:writes"} {
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
			t.Fatal(err)
		}
	}
	keys := fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword}
	given := func(c Config) (bool, string) {
		t.Helper()
		gr, applied, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
		if err != nil {
			t.Fatal(err)
		}
		done()
		return len(gr.ImageServers) == 1 && slices.Contains(applied.On, "server:gog:writes"), gr.ServerSecrets[assistant.GogKeyringSecret]
	}
	if ok, slot := given(c); !ok || slot != gogPassword {
		t.Fatalf("enabled: given %v, slot %q", ok, slot)
	}

	off := c
	off.MailProviders = nil
	r := BuildRoster(a, off, nil)
	for _, id := range []string{"server:gog", "server:gog:writes", "server:gog:send"} {
		if _, ok := r.Entry(id); ok {
			t.Errorf("%s is on the roster with Google disabled", id)
		}
	}
	if ok, slot := given(off); ok || slot != "" {
		t.Errorf("disabled: given %v, slot %q", ok, slot)
	}
	if _, err := ApplyChange(home, a, &off, Change{Switch: &Switch{ID: "server:gog:send", On: true}, Consent: true}); err == nil || !strings.Contains(err.Error(), "--mail-provider google") {
		t.Errorf("switching a disabled provider's server: %v", err)
	}
	if ps := MailProviders(a, off); ps[0].Enabled || !slices.Equal(ps[0].Accounts, []string{"person@example.com"}) {
		t.Errorf("disabled, the provider: %+v", ps)
	}

	back := off
	back.MailProviders = googleOn
	if slices.ContainsFunc(Unconsented(BuildRoster(a, back, nil), back), func(e RosterEntry) bool { return strings.HasPrefix(e.ID, "server:gog") }) {
		t.Error("enabled again, gog asks its agreement again")
	}
	if ok, slot := given(back); !ok || slot != gogPassword {
		t.Errorf("enabled again: given %v, slot %q", ok, slot)
	}
}

// TestSetMailProviderKeepsTheSignIn: disabling Google keeps gog's sign-in,
// switches and agreements in config.yaml and touches nothing else; an
// unknown provider, an agent with none and a running Assistant are refused.
func TestSetMailProviderKeepsTheSignIn(t *testing.T) {
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	signIns := map[string]ImageSignIn{"gog": {Account: "person@example.com"}}
	if err := SaveConfig(home, a.Name, Config{Enabled: true, MailProviders: googleOn, SignIns: signIns, Switches: map[string]bool{"server:gog": true}, Consented: map[string]string{"server:gog": "line"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetMailProvider(home, a, ClientCLI, "google", false); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(home, a.Name)
	if err != nil || len(c.MailProviders) != 0 || c.SignIns["gog"].Account != "person@example.com" || !c.Switches["server:gog"] || c.Consented["server:gog"] != "line" {
		t.Fatalf("disabled: %+v, %v", c, err)
	}
	if err := SetMailProvider(home, a, ClientCLI, "google", true); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(home, a.Name); err != nil || !slices.Equal(c.MailProviders, googleOn) {
		t.Fatalf("enabled again: %+v, %v", c.MailProviders, err)
	}
	if err := SetMailProvider(home, a, ClientCLI, "yahoo", true); err == nil || !strings.Contains(err.Error(), "google") {
		t.Errorf("an unknown provider: %v", err)
	}
	coder, err := Lookup("coder")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMailProvider(home, coder, ClientCLI, "google", true); err == nil || !strings.Contains(err.Error(), "reaches no mail") {
		t.Errorf("the Coding agent: %v", err)
	}

	running := shortHome(t)
	enable(t, running)
	sv := startAssistant(t, running, &starter{})
	defer func() { _, _ = sv.Stop(context.Background()) }()
	if err := SetMailProvider(running, a, ClientCLI, "google", true); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("a running Assistant: %v", err)
	}
}

// TestSettingGoogleUpEnablesIt: keeping the client enables Google, so a
// person who set gog up has it on their roster.
func TestSettingGoogleUpEnablesIt(t *testing.T) {
	r := newGoogleRig(t)
	if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(r.g.Home, "assistant")
	if err != nil || !slices.Equal(c.MailProviders, googleOn) {
		t.Fatalf("after the client: %v, %v", c.MailProviders, err)
	}
	if _, ok := BuildRoster(r.g.Agent, c, nil).Entry("server:gog"); !ok {
		t.Error("gog is not on the roster")
	}
}

// TestTheSegmentNamesGogsAccount: the plan's gog server carries the
// account it is signed in as, which the System Prompt names.
func TestTheSegmentNamesGogsAccount(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, MailProviders: googleOn, SignIns: map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now()}}}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "server:gog", On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword})
	if err != nil {
		t.Fatal(err)
	}
	done()
	sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if instr := agentInstruction(t, sp.Dir); !strings.Contains(instr, "mcp-image-gog (signed in as person@example.com)") {
		t.Errorf("the System Prompt does not name gog's account:\n%s", instr)
	}
}
