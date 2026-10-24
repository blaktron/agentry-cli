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

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// withImageServers makes the Assistant's own image servers the agents' for
// the test.
func withImageServers(t *testing.T) {
	t.Helper()
	old := imageServersOf
	imageServersOf = func(a Agent) []offer.ImageServer { return a.Builtin.ImageServers(policy.Images{}) }
	t.Cleanup(func() { imageServersOf = old })
}

const gogPassword = "keyring-test-value-0123456789"

// TestTheAssistantOffersGoogleWorkspace: the Assistant's roster has
// Google Workspace off by default with its two switches, waits for a
// sign-in when switched on without one, and its offer holds to the image
// server's rules; the Coding agent and Main have none.
func TestTheAssistantOffersGoogleWorkspace(t *testing.T) {
	withImageServers(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	if err := offer.CheckImageServers(a.Builtin.ImageServers(policy.Images{})); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, Config{}, nil)
	for _, id := range []string{"server:gog", "server:gog:writes", "server:gog:send"} {
		e, ok := r.Entry(id)
		if !ok || e.On || e.GrantedBy != GrantedByBuiltin {
			t.Fatalf("%s: %+v (found %v), want off and the built-in's", id, e, ok)
		}
	}
	e, _ := r.Entry("server:gog")
	for _, want := range []string{"reaching oauth2.googleapis.com, gmail.googleapis.com, www.googleapis.com, docs.googleapis.com, sheets.googleapis.com alone", "a volume of its own", "read only unless you allow changes"} {
		if !strings.Contains(e.Line, want) {
			t.Errorf("its sentence lacks %q: %s", want, e.Line)
		}
	}
	on := Config{Switches: map[string]bool{"server:gog": true}}
	if e, _ := BuildRoster(a, on, nil).Entry("server:gog"); e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "--google-sign-in") {
		t.Fatalf("on with no sign-in: %+v", e)
	}
	on.SignIns = map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now()}}
	if e, _ := BuildRoster(a, on, nil).Entry("server:gog"); e.State != StateOn {
		t.Fatalf("on and signed in: %+v", e)
	}
	for _, name := range []string{"coder", "main"} {
		b, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := BuildRoster(b, Config{}, nil).Entry("server:gog"); ok {
			t.Errorf("the %s agent has Google Workspace", name)
		}
	}
}

// TestGoogleWorkspaceIsGivenSignedInWithItsSwitches: a segment is given
// Google Workspace only on and signed in, with its keyring password from
// the keychain in its slot and its volume; "allow changes" only with it,
// and "allow sending mail" only with changes. What is on and not given is
// not named as applied.
func TestGoogleWorkspaceIsGivenSignedInWithItsSwitches(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	keys := fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword}
	apply := func(c Config) ([]string, []string, map[string]string, string) {
		t.Helper()
		g, applied, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
		if err != nil {
			t.Fatal(err)
		}
		done()
		if len(g.ImageServers) == 0 {
			return nil, applied.On, g.ServerSecrets, ""
		}
		return g.ImageServers[0].Switches, applied.On, g.ServerSecrets, g.ImageServers[0].Volume
	}
	signedIn := map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now(), Granted: []string{"writes", "send"}}}

	if sw, on, slots, _ := apply(Config{Switches: map[string]bool{"server:gog": true, "server:gog:writes": true}}); sw != nil || slices.Contains(on, "server:gog") || slices.Contains(on, "server:gog:writes") || slots[assistant.GogKeyringSecret] != "" {
		t.Fatalf("not signed in: switches %v applied %v slots %v", sw, on, slots)
	}
	sw, on, slots, vol := apply(Config{SignIns: signedIn, Switches: map[string]bool{"server:gog": true}})
	if len(sw) != 0 || !slices.Contains(on, "server:gog") || slots[assistant.GogKeyringSecret] != gogPassword || vol != ImageVolumeIn(home, "assistant", "gog") {
		t.Fatalf("signed in, read only: switches %v applied %v volume %q", sw, on, vol)
	}
	if sw, on, _, _ := apply(Config{SignIns: signedIn, Switches: map[string]bool{"server:gog": true, "server:gog:send": true}}); len(sw) != 0 || slices.Contains(on, "server:gog:send") {
		t.Fatalf("sending without changes: switches %v applied %v", sw, on)
	}
	if sw, on, _, _ := apply(Config{SignIns: signedIn, Switches: map[string]bool{"server:gog": true, "server:gog:writes": true, "server:gog:send": true}}); !slices.Equal(sw, []string{"writes", "send"}) || !slices.Contains(on, "server:gog:send") {
		t.Fatalf("changes and sending: switches %v applied %v", sw, on)
	}
	if sw, on, _, _ := apply(Config{SignIns: signedIn, Switches: map[string]bool{"server:gog:writes": true}}); sw != nil || slices.Contains(on, "server:gog:writes") {
		t.Fatalf("changes with the server off: switches %v applied %v", sw, on)
	}
	// A read-only sign-in asked for no scope that changes need: the
	// server is given read-only, and its changes wait for a new sign-in.
	readOnly := map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now()}}
	ro := Config{SignIns: readOnly, Switches: map[string]bool{"server:gog": true, "server:gog:writes": true}}
	if sw, on, _, _ := apply(ro); len(sw) != 0 || !slices.Contains(on, "server:gog") || slices.Contains(on, "server:gog:writes") {
		t.Fatalf("changes on a read-only sign-in: switches %v applied %v", sw, on)
	}
	if e, _ := BuildRoster(a, ro, nil).Entry("server:gog:writes"); e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "sign in again") {
		t.Fatalf("changes on a read-only sign-in: %+v", e)
	}
	delete(keys, ImageKeyringAccount("assistant", "gog"))
	c := Config{SignIns: signedIn, Switches: map[string]bool{"server:gog": true}}
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys); err == nil || !strings.Contains(err.Error(), "sign it in again") {
		t.Fatalf("a password the keychain lost: %v", err)
	}
}

// TestResetRemovesGoogleWorkspace: Reset removes the server's volume and
// its keyring password, and the sign-in goes with the configuration.
func TestResetRemovesGoogleWorkspace(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	keys := fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword, "provider:openai": "kept-0123456789"}
	old := resetSecrets
	resetSecrets = keys
	t.Cleanup(func() { resetSecrets = old })
	if err := SaveConfig(home, a.Name, Config{Enabled: true, SignIns: map[string]ImageSignIn{"gog": {Account: "person@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	d := &dockerx.Fake{}
	if err := Reset(context.Background(), home, a.Name, d); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Calls, func(c dockerx.FakeCall) bool {
		return c.Op == "VolumeRemove" && c.Arg == ImageVolumeIn(home, "assistant", "gog")
	}) {
		t.Errorf("Reset left the server's volume: %v", d.Calls)
	}
	if _, kept := keys[ImageKeyringAccount("assistant", "gog")]; kept || len(keys) != 1 {
		t.Errorf("the keychain after Reset: %v", keys)
	}
	if after, err := LoadConfig(home, a.Name); err != nil || len(after.SignIns) != 0 {
		t.Errorf("after Reset: %+v, %v", after, err)
	}
}

// noKeychain is a keychain that is not there: every call fails as the
// Secret Service's absence does on a Linux host with none.
type noKeychain struct{}

func (noKeychain) Get(string) (string, error) { return "", errNoSecretService }
func (noKeychain) Set(string, string) error   { return errNoSecretService }
func (noKeychain) Delete(string) error        { return errNoSecretService }

var errNoSecretService = errors.New("remove the secret from the system keychain: The name is not activatable")

// TestResetFinishesWithNoKeychain: on a machine with no keychain, Reset of
// an Assistant whose Google Workspace was never set up finishes and
// rewrites the configuration; one whose set-up the configuration records
// stops on the password it could not remove.
func TestResetFinishesWithNoKeychain(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	old := resetSecrets
	resetSecrets = noKeychain{}
	t.Cleanup(func() { resetSecrets = old })
	if err := SaveConfig(home, a.Name, Config{Enabled: true, Share: "/srv/share", Switches: map[string]bool{"server:gog": true}}); err != nil {
		t.Fatal(err)
	}
	if err := Reset(context.Background(), home, a.Name, &dockerx.Fake{}); err != nil {
		t.Fatalf("Reset with no keychain: %v", err)
	}
	after, err := LoadConfig(home, a.Name)
	if err != nil || !after.Enabled || after.Share != "" || len(after.Switches) != 0 {
		t.Fatalf("the configuration after Reset: %+v, %v", after, err)
	}

	if err := SaveConfig(home, a.Name, Config{Enabled: true, SignIns: map[string]ImageSignIn{"gog": {Account: "person@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if err := Reset(context.Background(), home, a.Name, &dockerx.Fake{}); err == nil || !strings.Contains(err.Error(), "keyring password for") {
		t.Fatalf("Reset of a signed-in server with no keychain: %v", err)
	}
}

// TestRosterReachesEveryGateWithGoogleWorkspace: Google Workspace switched
// on, agreed to and signed in reaches the segment's plan as one image
// server: its catalog entry with its pinned image, its command with the
// switches' arguments, its volume and its hosts; its keyring slot among the
// plan's server secrets; the receipt's line. Switching it on, and each
// switch, asks for agreement first.
func TestRosterReachesEveryGateWithGoogleWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	fresh := Config{Enabled: true, Switches: map[string]bool{"server:gog": true, "server:gog:writes": true}}
	unagreed := Unconsented(BuildRoster(a, fresh, nil), fresh)
	for _, id := range []string{"server:gog", "server:gog:writes"} {
		if !slices.ContainsFunc(unagreed, func(e RosterEntry) bool { return e.ID == id }) {
			t.Errorf("%s asks no agreement", id)
		}
	}
	c := Config{Enabled: true, SignIns: map[string]ImageSignIn{"gog": {Account: "person@example.com", At: time.Now(), Granted: []string{"writes", "send"}}}}
	for _, id := range []string{"server:gog", "server:gog:writes"} {
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
			t.Fatal(err)
		}
	}
	keys := fakeSecrets{ImageKeyringAccount("assistant", "gog"): gogPassword}
	gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	done()
	sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	images := spec.ImageServers(sp.Plan.MCPServers)
	if len(images) != 1 {
		t.Fatalf("the plan's image servers: %+v", images)
	}
	srv := images[0]
	vol := ImageVolumeIn(home, "assistant", "gog")
	if srv.Image != policy.DefaultGogImage || !slices.Equal(srv.Command, []string{"mcp", "--allow-write"}) ||
		!slices.Equal(srv.Volumes, []string{vol + ":" + assistant.GogHome}) || !slices.Equal(srv.Secrets, []string{assistant.GogKeyringSecret}) {
		t.Fatalf("the plan's gog server: %+v", srv)
	}
	b, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if srv.Env["GOG_ACCOUNT"] != "person@example.com" {
		t.Errorf("the server is not told its account: %v", srv.Env)
	}
	for _, want := range []string{vol + ":" + assistant.GogHome, "gmail.googleapis.com:443/tcp", "--allow-write"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the catalog lacks %q", want)
		}
	}
	if gr.ServerSecrets[assistant.GogKeyringSecret] != gogPassword || strings.Contains(string(b), gogPassword) {
		t.Errorf("the keyring slot: given %v, in the catalog %v", gr.ServerSecrets[assistant.GogKeyringSecret] == gogPassword, strings.Contains(string(b), gogPassword))
	}
	if !strings.Contains(sp.Plan.Enforcement["mcpServers.image"], "mcp-image-gog") {
		t.Errorf("the receipt's line: %q", sp.Plan.Enforcement["mcpServers.image"])
	}
}
