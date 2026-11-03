package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

const messengerTestToken = "424242:AAsecret-bot-token"

// fakeBotAPI answers getMe and an empty long poll, as Telegram does for a
// bot nobody has written to.
func fakeBotAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+messengerTestToken+"/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/bot"+messengerTestToken+"/") {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": 424242, "is_bot": true, "first_name": "Helper", "username": "helper_bot"}})
		default:
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeEngineWithRole is a docker Fake whose messenger container runs the
// real role against api.
func fakeEngineWithRole(api string) *dockerx.Fake {
	return &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-messenger": {Handler: func(ctx context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if err := messenger.Serve(ctx, stdin, stdout, stderr, messenger.Config{API: api}); err != nil {
				return 1, err
			}
			return 0, nil
		}},
	}}
}

func telegramKind(t *testing.T) messenger.Kind {
	t.Helper()
	k, ok := messenger.Lookup(messenger.KindTelegram)
	if !ok {
		t.Fatal("no telegram kind")
	}
	return k
}

func runsOf(f *dockerx.Fake) []dockerx.RunSpec {
	var out []dockerx.RunSpec
	for _, c := range f.Calls {
		if c.Op == "RunDetached" || c.Op == "RunForeground" || c.Op == "Create" {
			out = append(out, c.Run)
		}
	}
	return out
}

func opsOf(f *dockerx.Fake, op string) []string {
	var out []string
	for _, c := range f.Calls {
		if c.Op == op {
			out = append(out, c.Arg)
		}
	}
	return out
}

func TestMessengerTokenReachesTheRoleAlone(t *testing.T) {
	srv := fakeBotAPI(t)
	f := fakeEngineWithRole(srv.URL)
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	if got := l.Ready(); got.Account != "424242" || got.Name != "@helper_bot" {
		t.Fatalf("Ready = %+v", got)
	}
	for _, r := range runsOf(f) {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), messengerTestToken) {
			t.Fatalf("the credential is in %s's spec (argv, environment or mounts): %s", r.Name, b)
		}
		if len(r.Env) > 0 {
			t.Errorf("%s is given an environment: %v", r.Name, r.Env)
		}
	}
}

func TestMessengerRoleEgressIsItsHosts(t *testing.T) {
	srv := fakeBotAPI(t)
	f := fakeEngineWithRole(srv.URL)
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var relay, role dockerx.RunSpec
	for _, r := range runsOf(f) {
		switch {
		case strings.HasSuffix(r.Name, "-messenger-relay"):
			relay = r
		case strings.HasSuffix(r.Name, "-messenger"):
			role = r
		default:
			t.Errorf("an unexpected container: %s", r.Name)
		}
	}
	if want := []string{"proxy", "--allow", "api.telegram.org:443", "--public-only", "--addr", ":3128"}; !slices.Equal(relay.Argv, want) {
		t.Errorf("relay argv = %v, want %v", relay.Argv, want)
	}
	nets := opsOf(f, "NetworkCreate")
	if len(nets) != 1 || !strings.HasSuffix(nets[0], "-msg(internal=true)") {
		t.Fatalf("networks = %v, want one internal network", nets)
	}
	if role.Network != relay.Network || len(role.ExtraNetworks) > 0 || len(role.Publish) > 0 || len(role.Mounts) > 0 || len(role.Volumes) > 0 {
		t.Errorf("the role reaches more than its account's network: %+v", role)
	}
	if got := opsOf(f, "NetworkConnect"); len(got) != 1 || got[0] != "bridge<-"+relay.Name {
		t.Errorf("connects = %v, want the relay's leg out alone", got)
	}
	if role.User != "65534:65534" || !role.ReadOnly || !role.NoLog || !slices.Contains(role.CapDrop, "ALL") || !role.NoNewPrivileges {
		t.Errorf("the role's posture: %+v", role)
	}
	if want := []string{"messenger", "--proxy", "http://messenger-relay:3128"}; !slices.Equal(role.Argv, want) {
		t.Errorf("role argv = %v", role.Argv)
	}
	l.Close(context.Background())
	removed := opsOf(f, "RemoveForce")
	if !slices.Contains(removed, role.Name) || !slices.Contains(removed, relay.Name) || len(opsOf(f, "NetworkRemove")) != 1 {
		t.Errorf("Close left something: removed %v, networks %v", removed, opsOf(f, "NetworkRemove"))
	}
}

func TestMessengerRoleRunsUnderTheRuntimeWithTheHostsNameServers(t *testing.T) {
	old := lookupUpstreamsForMessenger
	lookupUpstreamsForMessenger = func() []string { return []string{"192.168.1.1"} }
	t.Cleanup(func() { lookupUpstreamsForMessenger = old })
	srv := fakeBotAPI(t)
	f := fakeEngineWithRole(srv.URL)
	dir := t.TempDir()
	pol := policy.Default()
	pol.Docker.Runtime = "runsc"
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Policy: pol, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	var relay, role dockerx.RunSpec
	for _, r := range runsOf(f) {
		switch {
		case strings.HasSuffix(r.Name, "-messenger-relay"):
			relay = r
		case strings.HasSuffix(r.Name, "-messenger"):
			role = r
		}
	}
	if relay.Runtime != "runsc" || role.Runtime != "runsc" {
		t.Fatalf("runtimes: relay %q, role %q", relay.Runtime, role.Runtime)
	}
	if len(relay.ExtraNetworks) != 1 || !strings.HasSuffix(relay.ExtraNetworks[0], "-msgout") {
		t.Errorf("the relay's leg out = %v", relay.ExtraNetworks)
	}
	if len(opsOf(f, "NetworkConnect")) != 0 {
		t.Errorf("a sandboxed container was connected after its start: %v", opsOf(f, "NetworkConnect"))
	}
	if len(relay.Mounts) != 1 || relay.Mounts[0].Target != policy.ResolvConfTarget || !relay.Mounts[0].ReadOnly {
		t.Fatalf("relay mounts = %+v", relay.Mounts)
	}
	if len(role.ExtraHosts) != 1 || !strings.HasPrefix(role.ExtraHosts[0], "messenger-relay:") {
		t.Errorf("the role is not given the relay's address: %v", role.ExtraHosts)
	}
}

func TestARefusedMessengerTokenFailsTheStartAndRemovesEverything(t *testing.T) {
	srv := fakeBotAPI(t)
	f := fakeEngineWithRole(srv.URL)
	_, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: "999:not-the-token", Image: "agentry-runner:test", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), messenger.TokenRefused) || strings.Contains(err.Error(), "not-the-token") {
		t.Fatalf("StartMessenger = %v", err)
	}
	if len(opsOf(f, "RemoveForce")) != 2 || len(opsOf(f, "NetworkRemove")) != 1 {
		t.Errorf("a failed start left containers or networks: %v %v", opsOf(f, "RemoveForce"), opsOf(f, "NetworkRemove"))
	}
}

func TestAMatrixRoleReachesItsHomeserverAlone(t *testing.T) {
	var start messenger.Start
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-messenger": {Handler: func(ctx context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			sc := bufio.NewScanner(stdin)
			if !sc.Scan() {
				return 1, nil
			}
			fr, err := messenger.ReadToRole(sc.Bytes())
			if err != nil || fr.Start == nil {
				return 1, err
			}
			start = *fr.Start
			_ = messenger.NewWriter(stdout).Write(messenger.FromRole{Ready: &messenger.Ready{Account: "@agent:example.org"}})
			<-ctx.Done()
			return 0, nil
		}},
	}}
	k, _ := messenger.Lookup(messenger.KindMatrix)
	if _, err := StartMessenger(context.Background(), f, MessengerParams{Kind: k, Token: messengerTestToken, Image: "agentry-runner:test", Dir: t.TempDir()}); err == nil {
		t.Fatal("a Matrix role started with no homeserver")
	}
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: k, Token: messengerTestToken, Server: "https://matrix.example.org", Image: "agentry-runner:test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	for _, r := range runsOf(f) {
		if strings.HasSuffix(r.Name, "-messenger-relay") {
			if want := []string{"proxy", "--allow", "matrix.example.org:443", "--public-only", "--addr", ":3128"}; !slices.Equal(r.Argv, want) {
				t.Errorf("relay argv = %v, want %v", r.Argv, want)
			}
		}
	}
	if start.Kind != messenger.KindMatrix || start.Server != "https://matrix.example.org" {
		t.Fatalf("start = %+v, want the kind and its homeserver", start)
	}
}
