package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// roleAndRelay are the messenger role's and its relay's specs, by name.
func roleAndRelay(f *dockerx.Fake) (relay, role dockerx.RunSpec) {
	for _, r := range runsOf(f) {
		switch {
		case strings.HasSuffix(r.Name, "-messenger-relay"):
			relay = r
		case strings.HasSuffix(r.Name, "-messenger"):
			role = r
		}
	}
	return relay, role
}

// scriptedRole is a docker Fake whose messenger container takes the start
// line and then writes lines, as a role that is not the real one does, and
// holds until its container is stopped.
func scriptedRole(lines ...string) *dockerx.Fake {
	return &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-messenger": {Handler: func(ctx context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			_, _ = bufio.NewReader(stdin).ReadString('\n')
			for _, l := range lines {
				_, _ = io.WriteString(stdout, l+"\n")
			}
			<-ctx.Done()
			return 0, nil
		}},
	}}
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
	// The role is started once: a second start line is refused.
	if err := l.Send(messenger.ToRole{Start: &messenger.Start{Kind: "telegram", Token: "other"}}); err == nil || !strings.Contains(err.Error(), "a messenger role is started once") {
		t.Errorf("a second start line: %v", err)
	}
	// Closing twice removes things once.
	l.Close(context.Background())
	l.Close(context.Background())
	if n := len(opsOf(f, "RemoveForce")); n != 2 {
		t.Errorf("two closes removed %d containers, want the role and the relay once each: %v", n, opsOf(f, "RemoveForce"))
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
	// The role talks to the supervisor over its standard streams, and each
	// container has a scratch folder of its own on a read-only root, bounded
	// as every run container is.
	if !role.Interactive || relay.Interactive {
		t.Errorf("interactive: role %v, relay %v", role.Interactive, relay.Interactive)
	}
	if !slices.Equal(role.Tmpfs, []string{"/tmp:size=16m,mode=1777"}) || !slices.Equal(relay.Tmpfs, []string{"/tmp"}) {
		t.Errorf("scratch: role %v, relay %v", role.Tmpfs, relay.Tmpfs)
	}
	lim := policy.Limits{}.WithDefaults()
	for _, r := range []dockerx.RunSpec{relay, role} {
		if lim.Memory == "" || r.Memory != lim.Memory || r.CPUs != lim.CPUs || r.PidsLimit != lim.Pids || !r.ReadOnly || !r.NoNewPrivileges || !slices.Equal(r.CapDrop, []string{"ALL"}) || len(r.CapAdd) != 0 {
			t.Errorf("%s's posture: %+v, limits %+v", r.Name, r, lim)
		}
	}
	// Everything carries the account's run label, so a sweep finds it.
	id := strings.TrimSuffix(strings.TrimPrefix(role.Name, "agentry-"), "-messenger")
	if relay.Labels[runLabel] != id || role.Labels[runLabel] != id || id == "" {
		t.Errorf("run labels: relay %v, role %v, id %q", relay.Labels, role.Labels, id)
	}
	if len(f.Networks) != 1 || f.Networks[0].Labels[runLabel] != id {
		t.Errorf("the network's labels: %+v", f.Networks)
	}
	l.Close(context.Background())
	l.Close(context.Background())
	removed := opsOf(f, "RemoveForce")
	if !slices.Contains(removed, role.Name) || !slices.Contains(removed, relay.Name) || len(opsOf(f, "NetworkRemove")) != 1 || len(removed) != 2 {
		t.Errorf("Close left something: removed %v, networks %v", removed, opsOf(f, "NetworkRemove"))
	}
	// The containers go before the network they are on, the role first.
	ops := f.Ops()
	at := func(op string) int { return slices.Index(ops, op) }
	if i, j, k := at("RemoveForce:"+role.Name), at("RemoveForce:"+relay.Name), slices.IndexFunc(ops, func(op string) bool { return strings.HasPrefix(op, "NetworkRemove:") }); i < 0 || i > j || j > k {
		t.Errorf("teardown order: %v", ops)
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
	pol.Docker.Limits = policy.Limits{Memory: "256m", CPUs: 3, Pids: 77}
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Policy: pol, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	relay, role := roleAndRelay(f)
	for _, r := range []dockerx.RunSpec{relay, role} {
		if r.Memory != "256m" || r.CPUs != 3 || r.PidsLimit != 77 {
			t.Errorf("%s is not bounded by the policy's limits: %q %d %d", r.Name, r.Memory, r.CPUs, r.PidsLimit)
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
	// The relay reads the host's name servers, from a file of the account's
	// own that the engine may relabel for it.
	resolv := filepath.Join(dir, "resolv.conf")
	if b, err := os.ReadFile(resolv); err != nil || !strings.Contains(string(b), "nameserver 192.168.1.1\n") {
		t.Errorf("resolv.conf = %q, %v", b, err)
	}
	if m := relay.Mounts[0]; m.Source != resolv || !m.Relabel {
		t.Errorf("the relay's resolv.conf mount: %+v", m)
	}
	ip, _ := f.ContainerIP(context.Background(), relay.Name, relay.Network)
	if want := []string{"messenger-relay:" + ip}; !slices.Equal(role.ExtraHosts, want) {
		t.Errorf("the role is given %v, want %v", role.ExtraHosts, want)
	}
	if nets := opsOf(f, "NetworkCreate"); len(nets) != 2 || !strings.HasSuffix(nets[0], "-msg(internal=true)") || !strings.HasSuffix(nets[1], "-msgout(internal=false)") {
		t.Errorf("networks = %v, want the internal one and the relay's leg out", nets)
	}

	// The policy's own resolvers are read in place of the machine's.
	f2 := fakeEngineWithRole(srv.URL)
	dir2 := t.TempDir()
	pol2 := policy.Default()
	pol2.Docker.Runtime = "runsc"
	pol2.Docker.Resolvers = []string{"10.9.8.7"}
	l2, err := StartMessenger(context.Background(), f2, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Policy: pol2, Dir: dir2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close(context.Background())
	if b, err := os.ReadFile(filepath.Join(dir2, "resolv.conf")); err != nil || string(b) != string(policy.ResolvConf([]string{"10.9.8.7"})) {
		t.Errorf("resolv.conf with the policy's resolvers = %q, %v", b, err)
	}

	// With no name server to name, the relay stays on the engine's default
	// runtime, is connected out after its start, and the role still runs
	// under the runtime; the start says so.
	lookupUpstreamsForMessenger = func() []string { return nil }
	f3 := fakeEngineWithRole(srv.URL)
	pol3 := policy.Default()
	pol3.Docker.Runtime = "runsc"
	var narration bytes.Buffer
	l3, err := StartMessenger(context.Background(), f3, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Policy: pol3, Dir: t.TempDir(), Log: &narration})
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close(context.Background())
	relay3, role3 := roleAndRelay(f3)
	if relay3.Runtime != "" || role3.Runtime != "runsc" || len(relay3.ExtraNetworks) != 0 || len(relay3.Mounts) != 0 {
		t.Errorf("without name servers: relay runtime %q, role runtime %q, legs %v, mounts %+v", relay3.Runtime, role3.Runtime, relay3.ExtraNetworks, relay3.Mounts)
	}
	if got := opsOf(f3, "NetworkConnect"); len(got) != 1 || got[0] != "bridge<-"+relay3.Name {
		t.Errorf("connects = %v, want the relay's leg out", got)
	}
	if !strings.Contains(narration.String(), "the messenger relay runs on the engine's default runtime, not runsc") {
		t.Errorf("the start said %q", narration.String())
	}
	if len(opsOf(f3, "NetworkCreate")) != 1 {
		t.Errorf("networks = %v, want the internal one alone", opsOf(f3, "NetworkCreate"))
	}
}

// emptyAddress is an engine that does not know a container's address.
type emptyAddress struct{ *dockerx.Fake }

func (emptyAddress) ContainerIP(context.Context, string, string) (string, error) { return "", nil }

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

	// Each start that fails says why, starts nothing it was not to, and
	// leaves nothing behind.
	params := func() MessengerParams {
		return MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Dir: t.TempDir()}
	}
	left := func(t *testing.T, f *dockerx.Fake) {
		t.Helper()
		if len(opsOf(f, "RemoveForce")) != 2 || len(opsOf(f, "NetworkRemove")) != 1 {
			t.Errorf("a failed start left containers or networks: %v %v", opsOf(f, "RemoveForce"), opsOf(f, "NetworkRemove"))
		}
	}
	t.Run("no credential", func(t *testing.T) {
		f := fakeEngineWithRole(srv.URL)
		p := params()
		p.Token = ""
		if _, err := StartMessenger(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "has no credential") || len(f.Calls) != 0 {
			t.Errorf("StartMessenger = %v, calls %v", err, f.Ops())
		}
	})
	t.Run("a kind with no host", func(t *testing.T) {
		f := fakeEngineWithRole(srv.URL)
		p := params()
		p.Kind.Hosts = nil
		if _, err := StartMessenger(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "declares no host") || len(f.Calls) != 0 {
			t.Errorf("StartMessenger = %v, calls %v", err, f.Ops())
		}
	})
	t.Run("a role that ends before it connects", func(t *testing.T) {
		f := scriptedRole()
		f.Foreground["-messenger"].Handler = func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			// What it fails with names the credential it was given.
			return 1, errors.New("dial failed: " + strings.TrimSpace(line))
		}
		p := params()
		var narration bytes.Buffer
		p.Log = &narration
		_, err := StartMessenger(context.Background(), f, p)
		if err == nil || !strings.Contains(err.Error(), "did not connect: it ended before it connected") || strings.Contains(err.Error(), messengerTestToken) {
			t.Errorf("StartMessenger = %v", err)
		}
		if !strings.Contains(narration.String(), "the messenger role ended") || strings.Contains(narration.String(), messengerTestToken) {
			t.Errorf("the role's log: %q", narration.String())
		}
		left(t, f)
	})
	t.Run("a service that does not answer", func(t *testing.T) {
		old := messengerReadyWait
		messengerReadyWait = time.Millisecond
		t.Cleanup(func() { messengerReadyWait = old })
		f := scriptedRole()
		_, err := StartMessenger(context.Background(), f, params())
		if err == nil || !strings.Contains(err.Error(), "did not answer the messenger role within 1ms") {
			t.Errorf("StartMessenger = %v", err)
		}
		left(t, f)
	})
	t.Run("a start that is canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		f := scriptedRole()
		f.Foreground["-messenger"].Handler = func(rctx context.Context, _ dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
			_, _ = bufio.NewReader(stdin).ReadString('\n')
			close(started)
			<-rctx.Done()
			return 0, nil
		}
		go func() {
			<-started
			cancel()
		}()
		if _, err := StartMessenger(ctx, f, params()); !errors.Is(err, context.Canceled) {
			t.Errorf("StartMessenger = %v", err)
		}
		left(t, f)
	})
	t.Run("an engine that knows no address for the relay", func(t *testing.T) {
		f := fakeEngineWithRole(srv.URL)
		p := params()
		p.Policy = policy.Default()
		p.Policy.Docker.Runtime = "runsc"
		p.Policy.Docker.Resolvers = []string{"10.9.8.7"}
		_, err := StartMessenger(context.Background(), emptyAddress{f}, p)
		if err == nil || !strings.Contains(err.Error(), "the messenger relay's address on its network") {
			t.Errorf("StartMessenger = %v", err)
		}
		if len(opsOf(f, "NetworkRemove")) != 2 || len(opsOf(f, "RemoveForce")) != 1 {
			t.Errorf("a failed start left something: %v %v", opsOf(f, "RemoveForce"), opsOf(f, "NetworkRemove"))
		}
	})
}

// The role's lines after its ready line reach the supervisor as frames, in
// order; a line that is not a frame is skipped, a second ready line is not
// passed on, and the frames end when the role does.
func TestAMessengerRolesFramesReachTheSupervisor(t *testing.T) {
	f := scriptedRole(
		`{"state":{"connected":false,"error":"warming up"}}`,
		`this is not a frame`,
		`{"ready":{"account":"7","name":"@scripted"}}`,
		`{"ready":{"account":"8"}}`,
		`{"state":{"connected":true}}`,
		`{"dropped":{"reason":"not paired"}}`,
	)
	l, err := StartMessenger(context.Background(), f, MessengerParams{Kind: telegramKind(t), Token: messengerTestToken, Image: "agentry-runner:test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Ready(); got.Account != "7" || got.Name != "@scripted" {
		t.Fatalf("Ready = %+v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var frames []messenger.FromRole
	for len(frames) < 2 {
		select {
		case fr, ok := <-l.Frames():
			if !ok {
				t.Fatalf("the frames ended after %+v", frames)
			}
			frames = append(frames, fr)
		case <-ctx.Done():
			t.Fatalf("only %+v arrived", frames)
		}
	}
	if frames[0].State == nil || !frames[0].State.Connected || frames[1].Dropped == nil || frames[1].Dropped.Reason != "not paired" {
		t.Errorf("frames = %+v", frames)
	}
	l.Close(context.Background())
	for fr := range l.Frames() {
		t.Errorf("a frame after the two: %+v", fr)
	}
}
