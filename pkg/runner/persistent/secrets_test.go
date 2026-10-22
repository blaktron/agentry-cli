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
)

// fakeSecrets is a keychain in memory.
type fakeSecrets map[string]string

func (f fakeSecrets) Get(account string) (string, error) {
	v, ok := f[account]
	if !ok {
		return "", errors.New("no entry " + account)
	}
	return v, nil
}

func (f fakeSecrets) Set(account, value string) error { f[account] = value; return nil }

func (f fakeSecrets) Delete(account string) error { delete(f, account); return nil }

const ghValue = "stored-test-value-0123456789"

func TestCheckSecretNameAndValue(t *testing.T) {
	for _, n := range []string{"GITHUB_TOKEN", "A", "X1_2", "A" + strings.Repeat("B", 63)} {
		if err := CheckSecretName(n); err != nil {
			t.Errorf("%s refused: %v", n, err)
		}
	}
	for _, n := range []string{"", "github_token", "1TOKEN", "A-B", "A" + strings.Repeat("B", 64), "PATH", "HOME", "HTTPS_PROXY", "LD_PRELOAD", "AGENTRY_TOKEN", "LC_ALL", "MCP_GATEWAY_X"} {
		if err := CheckSecretName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if err := CheckSecretValue(ghValue); err != nil {
		t.Errorf("a token refused: %v", err)
	}
	for _, v := range []string{"", "short", "two\nlines", "tab\there-and-more", "nul\x00more-chars", "\xff\xfe-not-utf8", strings.Repeat("a", MaxSecretBytes+1)} {
		if err := CheckSecretValue(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestAStoredSecretIsAnEntryWhoseValueStaysInTheKeychain: storing a secret
// adds an entry that asks the person first, saying who is given it;
// config.yaml holds its name and never its value, at version 6.
func TestAStoredSecretIsAnEntryWhoseValueStaysInTheKeychain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	set := StoredSecret{Name: "GITHUB_TOKEN", Set: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		agent Agent
		want  string
	}{
		{coderAgent(t), "Gives your secret GITHUB_TOKEN to the programs it runs, in their environment, which can read it and send it anywhere they reach; its value stays in your keychain"},
		{assistantAgent(t), "Gives your secret GITHUB_TOKEN to an MCP server you add that declares it; no server it has declares it now; its value stays in your keychain"},
	} {
		a := tc.agent
		c := Config{Enabled: true}
		var ce *ConsentError
		if _, err := ApplyChange(home, a, &c, Change{SetSecret: &set}); !errors.As(err, &ce) || len(ce.Entries) != 1 || !strings.HasPrefix(ce.Entries[0].Line, tc.want) {
			t.Fatalf("%s: storing a secret unagreed: %v", a.Name, err)
		}
		if len(c.Secrets) != 0 {
			t.Fatalf("%s: a refused change was kept", a.Name)
		}
		if _, err := ApplyChange(home, a, &c, Change{SetSecret: &set, Consent: true}); err != nil {
			t.Fatal(err)
		}
		e, ok := BuildRoster(a, c, nil).Entry(SecretEntryID("GITHUB_TOKEN"))
		if !ok || e.Kind != KindSecret || !e.On || e.GrantedBy != GrantedByYou || e.Name != "Your secret GITHUB_TOKEN" {
			t.Fatalf("%s: the entry %+v", a.Name, e)
		}
		if err := SaveConfig(home, a.Name, c); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(Dir(home, a.Name), "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "GITHUB_TOKEN") || strings.Contains(string(b), ghValue) || readVersion(t, home, a.Name) != 6 {
			t.Fatalf("%s: config.yaml:\n%s", a.Name, b)
		}
		// A server that declares it changes who is given it, so it asks
		// again.
		srv := wordServer
		srv.Secrets = []string{"GITHUB_TOKEN"}
		if _, err := ApplyChange(home, a, &c, Change{AddServer: &srv}); !errors.As(err, &ce) ||
			!slices.ContainsFunc(ce.Entries, func(e RosterEntry) bool {
				return e.ID == SecretEntryID("GITHUB_TOKEN") && strings.Contains(e.Line, "to the MCP server pypi:office-word-mcp-server@1.1.11")
			}) {
			t.Fatalf("%s: a server declaring the secret: %v", a.Name, err)
		}
		if _, err := ApplyChange(home, a, &c, Change{RemoveSecret: "GITHUB_TOKEN"}); err != nil || len(c.Secrets) != 0 || c.Consented[SecretEntryID("GITHUB_TOKEN")] != "" {
			t.Fatalf("%s: removed: %v %+v", a.Name, err, c)
		}
	}
}

// TestASecretIsRefusedWhereItCannotBeKept: a bad name, more than
// MaxSecrets, and an agent that keeps none.
func TestASecretIsRefusedWhereItCannotBeKept(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "PATH"}, Consent: true}); err == nil {
		t.Error("PATH was stored")
	}
	for i := range MaxSecrets {
		name := "TOKEN_" + strings.Repeat("A", i+1)
		if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: name}, Consent: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "ONE_MORE"}, Consent: true}); err == nil {
		t.Error("a secret past the bound was stored")
	}
	// Setting one again is not one more.
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "TOKEN_A", Set: time.Now()}, Consent: true}); err != nil {
		t.Errorf("setting a secret again: %v", err)
	}
	main := Agent{Name: "main", Builtin: localrun.Main()}
	if err := addSecret(main, &Config{}, StoredSecret{Name: "GITHUB_TOKEN"}); err == nil {
		t.Error("Main was given a secret")
	}
	if _, err := (localrun.Segment{Builtin: localrun.Main(), Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test",
		Grants: localrun.Grants{Secrets: map[string]string{"GITHUB_TOKEN": ghValue}}}).Plan(context.Background(), nil); err == nil {
		t.Error("a segment of Main was given a secret")
	}
}

// TestRosterReachesEveryGateWithAStoredSecret: a stored secret on reaches
// the segment's values, the Coding agent's programs (the tools server's
// secret variables, its spec, the plan's enforcement and its System
// Prompt, by name) and the slot of a server that declares it; off, it is
// withheld and reaches none of them. No value is in the plan.
func TestRosterReachesEveryGateWithAStoredSecret(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	srv := wordServer
	srv.Secrets = []string{"GITHUB_TOKEN"}
	for _, a := range []Agent{coderAgent(t), assistantAgent(t)} {
		record(t, serversDir(home, a.Name), srv)
		for _, on := range []bool{true, false} {
			t.Run(a.Name+map[bool]string{true: " on", false: " off"}[on], func(t *testing.T) {
				keys := fakeSecrets{SecretAccount(a.Name, "GITHUB_TOKEN"): ghValue}
				c := Config{Enabled: true}
				for _, ch := range []Change{{AddServer: &srv}, {SetSecret: &StoredSecret{Name: "GITHUB_TOKEN", Set: time.Now()}}, {Switch: &Switch{ID: SecretEntryID("GITHUB_TOKEN"), On: on}}} {
					ch.Consent = true
					if _, err := ApplyChange(home, a, &c, ch); err != nil {
						t.Fatal(err)
					}
				}
				r := BuildRoster(a, c, nil)
				g, applied, done, err := Apply(context.Background(), home, a, c, r, nil, nil, keys)
				if err != nil {
					t.Fatal(err)
				}
				defer done()
				if (g.Secrets["GITHUB_TOKEN"] == ghValue) != on || slices.Contains(g.WithheldSecrets, "GITHUB_TOKEN") == on ||
					slices.Contains(applied.On, SecretEntryID("GITHUB_TOKEN")) != on {
					t.Fatalf("grants %+v %v, applied %v", g.Secrets, g.WithheldSecrets, applied.On)
				}
				sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}.Plan(context.Background(), nil)
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
				programs := on && a.Builtin.Codes()
				ts, cat := read("toolspec.json"), read("catalog.yaml")
				if strings.Contains(ts, `"secretEnv"`) != programs || strings.Contains(cat, "agentry-tools.github_token") != programs {
					t.Errorf("the tools server's secret variables: spec %v, catalogue %v, want %v", strings.Contains(ts, `"secretEnv"`), strings.Contains(cat, "agentry-tools.github_token"), programs)
				}
				if _, has := sp.Plan.Enforcement["persistent.secrets"]; has != programs {
					t.Errorf("the plan's secrets line: %v, want %v", has, programs)
				}
				if c := sp.Plan.Capabilities.Coder; (c != nil && slices.Equal(c.Secrets, []string{"GITHUB_TOKEN"})) != programs {
					t.Errorf("the plan's coder grant: %+v", c)
				}
				if !strings.Contains(cat, ".github_token") || len(sp.Plan.MCPServers) != 1 || !slices.Equal(sp.Plan.MCPServers[0].Secrets, []string{"GITHUB_TOKEN"}) {
					t.Errorf("the server's slot is not declared: %+v", sp.Plan.MCPServers)
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
					if strings.Contains(string(b), ghValue) {
						return errors.New(path + " holds the value")
					}
					prompted = prompted || strings.Contains(string(b), "**Secrets.** The programs you run have the person's secrets GITHUB_TOKEN")
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if prompted != programs {
					t.Errorf("the System Prompt names the programs' secrets: %v, want %v", prompted, programs)
				}
			})
		}
	}
}

// TestASecretWithNoValueRefusesTheSegment: a secret on whose value the
// keychain does not hold refuses the segment's start, saying how to set it,
// and a client with no keychain cannot start one.
func TestASecretWithNoValueRefusesTheSegment(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "GITHUB_TOKEN"}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{}); err == nil || !strings.Contains(err.Error(), "agentry agent secret set coder GITHUB_TOKEN") {
		t.Errorf("no value: %v", err)
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{SecretAccount("coder", "GITHUB_TOKEN"): "two\nlines-of-it"}); err == nil {
		t.Error("a value the segment cannot carry was given")
	}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, nil); err == nil {
		t.Error("a client with no keychain gave a secret")
	}
	// A name no one could store, written into config.yaml by hand, is
	// refused before the keychain is read.
	c.Secrets = []StoredSecret{{Name: "PATH"}}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{SecretAccount("coder", "PATH"): "/evil/bin:/usr/bin"}); err == nil {
		t.Error("a secret named PATH was given")
	}
}

// TestSettingASecretAgainRestartsTheSegment: a secret's value set again, or
// its switch, reaches the next turn; the value itself is never part of
// what decides it.
func TestSettingASecretAgainRestartsTheSegment(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	first := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if restart, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "GITHUB_TOKEN", Set: first}, Consent: true}); err != nil || !restart {
		t.Fatalf("storing a secret: restart %v, %v", restart, err)
	}
	if restart, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "GITHUB_TOKEN", Set: first.Add(time.Minute)}}); err != nil || !restart {
		t.Fatalf("setting it again: restart %v, %v", restart, err)
	}
	if restart, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: SecretEntryID("GITHUB_TOKEN"), On: false}}); err != nil || !restart {
		t.Fatalf("switching it off: restart %v, %v", restart, err)
	}
	if restart, err := ApplyChange(home, a, &c, Change{RemoveSecret: "GITHUB_TOKEN"}); err != nil || !restart {
		t.Fatalf("removing it while off: restart %v, %v", restart, err)
	}
}

// TestResetRemovesTheStoredSecrets: Reset takes the secrets out of the
// configuration and their values out of the keychain.
func TestResetRemovesTheStoredSecrets(t *testing.T) {
	home := shortHome(t)
	a := coderAgent(t)
	keys := fakeSecrets{SecretAccount("coder", "GITHUB_TOKEN"): ghValue, SecretAccount("assistant", "GITHUB_TOKEN"): ghValue}
	old := resetSecrets
	resetSecrets = keys
	t.Cleanup(func() { resetSecrets = old })
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "GITHUB_TOKEN"}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	if err := Reset(context.Background(), home, a.Name, &dockerx.Fake{}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfig(home, a.Name)
	if err != nil || len(after.Secrets) != 0 {
		t.Fatalf("after Reset: %+v, %v", after, err)
	}
	if _, kept := keys[SecretAccount("coder", "GITHUB_TOKEN")]; kept || len(keys) != 1 {
		t.Errorf("the keychain after Reset: %v", keys)
	}
}
