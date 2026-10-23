package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const ghToken = "ghp_accounttoken0123456789abcdef"

// botAccount is an account of the Coding agent's own on github.com.
func botAccount() GitAccount {
	return GitAccount{Host: "github.com", Kind: spec.GitHostGitHub, Own: true, User: "coder-bot", Name: "Coder Bot", Email: "coder-bot@example.com",
		Set: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
}

func TestAGitHostAccountIsAnEntryWhoseTokenStaysInTheKeychain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	g := botAccount()
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g}); !errors.As(err, &ce) || len(ce.Entries) != 1 ||
		!strings.HasPrefix(ce.Entries[0].Line, "Signs in to GitHub as coder-bot, its own account: it reaches the repositories you share with that account") ||
		!strings.Contains(ce.Entries[0].Line, "the token reaches the programs it runs (git and gh sign in with it), which can read it and send it anywhere they reach") {
		t.Fatalf("an account unagreed: %v", err)
	}
	if len(c.Accounts) != 0 {
		t.Fatal("a refused change was kept")
	}
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err != nil {
		t.Fatal(err)
	}
	e, ok := BuildRoster(a, c, nil).Entry(AccountEntryID("github.com"))
	if !ok || e.Kind != KindAccount || !e.On || e.GrantedBy != GrantedByYou || e.Name != "GitHub as coder-bot (its own account)" {
		t.Fatalf("the entry %+v", e)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(Dir(home, a.Name), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "coder-bot") || strings.Contains(string(b), ghToken) || readVersion(t, home, a.Name) != ConfigVersion {
		t.Fatalf("config.yaml:\n%s", b)
	}
	// The person's own account says so, and asks again.
	yours := g
	yours.Own = false
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &yours}); !errors.As(err, &ce) ||
		!strings.HasPrefix(ce.Entries[0].Line, "Signs in to GitHub as coder-bot, your account: it can do whatever the token can do as you") {
		t.Fatalf("your account: %v", err)
	}
	if _, err := ApplyChange(home, a, &c, Change{RemoveAccount: "github.com"}); err != nil || len(c.Accounts) != 0 || c.Consented[AccountEntryID("github.com")] != "" {
		t.Fatalf("removed: %v %+v", err, c)
	}
}

// TestAnAccountIsRefusedWhereItCannotBeGiven: one out of shape, a kind that
// is not its host's, a second host whose CLI reads one token, more than the
// bound, and an agent other than the Coding agent.
func TestAnAccountIsRefusedWhereItCannotBeGiven(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	for name, mutate := range map[string]func(*GitAccount){
		"a host with a scheme":    func(g *GitAccount) { g.Host = "https://github.com" },
		"a host in capitals":      func(g *GitAccount) { g.Host = "GitHub.com" },
		"a user with a semicolon": func(g *GitAccount) { g.User = "bot;rm" },
		"a name of two lines":     func(g *GitAccount) { g.Name = "Coder\nBot" },
		"no email":                func(g *GitAccount) { g.Email = "" },
		"github.com as gitlab":    func(g *GitAccount) { g.Kind = spec.GitHostGitLab },
		"no kind":                 func(g *GitAccount) { g.Kind = "" },
	} {
		g := botAccount()
		mutate(&g)
		c := Config{Enabled: true}
		if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err == nil {
			t.Errorf("%s was given", name)
		}
	}
	c := Config{Enabled: true}
	for _, host := range []string{"gitlab.com", "gitlab.example.com"} {
		g := GitAccount{Host: host, Kind: spec.GitHostGitLab, User: "me", Name: "Me", Email: "me@example.com"}
		_, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true})
		if (err == nil) != (host == "gitlab.com") {
			t.Errorf("a GitLab account on %s: %v", host, err)
		}
	}
	c = Config{Enabled: true}
	for i := range spec.MaxCoderAccounts + 1 {
		g := GitAccount{Host: "git" + string(rune('a'+i)) + ".example.com", Kind: spec.GitHostGit, User: "me", Name: "Me", Email: "me@example.com"}
		_, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true})
		if (err == nil) != (i < spec.MaxCoderAccounts) {
			t.Errorf("account %d: %v", i+1, err)
		}
	}
	g := botAccount()
	if err := addAccount(assistantAgent(t), &Config{}, g); err == nil {
		t.Error("the Assistant was given an account")
	}
	acct := localrun.Account{CoderAccount: spec.CoderAccount{Host: g.Host, Kind: g.Kind, User: g.User, Name: g.Name, Email: g.Email}, Token: ghToken}
	if _, err := (localrun.Segment{Builtin: localrun.Assistant(), Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test",
		Grants: localrun.Grants{Accounts: []localrun.Account{acct}}}).Plan(context.Background(), nil); err == nil {
		t.Error("a segment of the Assistant was given an account")
	}
}

// TestRosterReachesEveryGateWithAnAccount: an account on, with the Coding
// agent's commands on, reaches the segment's tokens, the tools server's
// secret variables and accounts, the catalogue, the plan's enforcement and
// the System Prompt, by host and name; off, or with its commands off, it
// reaches none of them. No token is in the plan.
func TestRosterReachesEveryGateWithAnAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := coderAgent(t)
	for _, tc := range []struct {
		name               string
		account, commands  bool
		wantGiven, applied bool
	}{
		{"on", true, true, true, true},
		{"off", false, true, false, false},
		{"commands off", true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := botAccount()
			keys := fakeSecrets{AccountKeychainAccount("github.com", "coder-bot"): ghToken}
			c := Config{Enabled: true}
			for _, ch := range []Change{{SetAccount: &g}, {Switch: &Switch{ID: AccountEntryID("github.com"), On: tc.account}}, {Switch: &Switch{ID: EntryCommands, On: tc.commands}}} {
				ch.Consent = true
				if _, err := ApplyChange(home, a, &c, ch); err != nil {
					t.Fatal(err)
				}
			}
			gr, applied, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			if given := len(gr.Accounts) == 1 && gr.Accounts[0].Token == ghToken; given != tc.wantGiven {
				t.Fatalf("accounts %+v", gr.Accounts)
			}
			if slices.Contains(applied.On, AccountEntryID("github.com")) != tc.applied {
				t.Errorf("applied %v", applied.On)
			}
			sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			read := func(name string) string {
				b, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", name))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			if tc.commands {
				ts, cat := read("toolspec.json"), read("catalog.yaml")
				if strings.Contains(ts, `"AGENTRY_ACCOUNT_TOKEN_1"`) != tc.wantGiven || strings.Contains(ts, `"coder-bot"`) != tc.wantGiven ||
					strings.Contains(cat, "agentry-tools.agentry_account_token_1") != tc.wantGiven {
					t.Errorf("the tools server's account: spec %s\ncatalogue %s", ts, cat)
				}
			}
			if _, has := sp.Plan.Enforcement["persistent.accounts"]; has != tc.wantGiven {
				t.Errorf("the plan's accounts line: %v", has)
			}
			if co := sp.Plan.Capabilities.Coder; (co != nil && len(co.Accounts) == 1 && co.Accounts[0].TokenEnv == "AGENTRY_ACCOUNT_TOKEN_1") != tc.wantGiven {
				t.Errorf("the plan's coder grant: %+v", co)
			}
			prompted := false
			err = filepath.Walk(sp.Dir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				b, err := os.ReadFile(path) //nolint:gosec // the plan's own files
				if err != nil {
					return err
				}
				if strings.Contains(string(b), ghToken) {
					return errors.New(path + " holds the token")
				}
				prompted = prompted || strings.Contains(string(b), "**Git host accounts.** You sign in to github.com as coder-bot (your own account)")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if prompted != tc.wantGiven {
				t.Errorf("the System Prompt names the account: %v, want %v", prompted, tc.wantGiven)
			}
		})
	}
}

// TestAnAccountWithNoTokenRefusesTheSegment: an account on whose token the
// keychain does not hold refuses the start, saying how to set it, and a
// client with no keychain cannot start one.
func TestAnAccountWithNoTokenRefusesTheSegment(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	g := botAccount()
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	if _, _, _, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{}); err == nil || !strings.Contains(err.Error(), "agentry agent configure coder --account github.com") {
		t.Errorf("no token: %v", err)
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{AccountKeychainAccount("github.com", "coder-bot"): "short"}); err == nil {
		t.Error("a token the segment cannot mask was given")
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, r, nil, nil, nil); err == nil {
		t.Error("a client with no keychain gave an account")
	}
	// An account out of shape, written into config.yaml by hand, is refused
	// before the keychain is read.
	c.Accounts[0].User = "x; rm -rf /"
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{AccountKeychainAccount("github.com", "x; rm -rf /"): ghToken}); err == nil {
		t.Error("an account out of shape was given")
	}
}

// TestSettingAnAccountAgainRestartsTheSegment: its token set again, its
// switch, or its identity reaches the next turn.
func TestSettingAnAccountAgainRestartsTheSegment(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	g := botAccount()
	if restart, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err != nil || !restart {
		t.Fatalf("adding: restart %v, %v", restart, err)
	}
	again := g
	again.Set = g.Set.Add(time.Minute)
	if restart, err := ApplyChange(home, a, &c, Change{SetAccount: &again}); err != nil || !restart {
		t.Fatalf("its token set again: restart %v, %v", restart, err)
	}
	renamed := again
	renamed.Email = "other@example.com"
	if restart, err := ApplyChange(home, a, &c, Change{SetAccount: &renamed, Consent: true}); err != nil || !restart {
		t.Fatalf("its email: restart %v, %v", restart, err)
	}
	if restart, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: AccountEntryID("github.com"), On: false}}); err != nil || !restart {
		t.Fatalf("switched off: restart %v, %v", restart, err)
	}
}

// TestResetRemovesTheAccounts: Reset takes the accounts out of the
// configuration and their tokens out of the keychain.
func TestResetRemovesTheAccounts(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	keys := fakeSecrets{AccountKeychainAccount("github.com", "coder-bot"): ghToken, "provider:openai": "kept-0123456789"}
	old := resetSecrets
	resetSecrets = keys
	t.Cleanup(func() { resetSecrets = old })
	c := Config{Enabled: true}
	g := botAccount()
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	if err := Reset(context.Background(), home, a.Name, &dockerx.Fake{}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfig(home, a.Name)
	if err != nil || len(after.Accounts) != 0 {
		t.Fatalf("after Reset: %+v, %v", after, err)
	}
	if _, kept := keys[AccountKeychainAccount("github.com", "coder-bot")]; kept || len(keys) != 1 {
		t.Errorf("the keychain after Reset: %v", keys)
	}
}

func TestAccountGuidanceSaysWhyAndHow(t *testing.T) {
	own := AccountGuidance(spec.GitHostGitHub, true)
	for _, want := range []string{"recommended", "reaches only the repositories you share", "removing a collaborator", "one free machine account", "Collaborators"} {
		if !strings.Contains(own, want) {
			t.Errorf("its own account's guidance has no %q: %s", want, own)
		}
	}
	yours := AccountGuidance(spec.GitHostGitHub, false)
	for _, want := range []string{"whatever the token can do as you", "fine-grained token limited to the repositories you choose"} {
		if !strings.Contains(yours, want) {
			t.Errorf("your account's guidance has no %q: %s", want, yours)
		}
	}
}
