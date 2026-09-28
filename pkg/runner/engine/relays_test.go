package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// TestRelaysStartedBy reads relay names out of the gateway's own lines
// (v0.43.3's l4 and l7 shapes), once each, and nothing out of other lines.
func TestRelaysStartedBy(t *testing.T) {
	logs := strings.Join([]string{
		"- Running proxy sidecars for hosts [{api.github.com 443 TCP}]",
		"starting l4 proxy docker-mcp-l4proxy-a1b2c3 for api.github.com, ports 443",
		"    - Starting l7 proxy docker-mcp-l7proxy-d4e5f6 for [api.example]",
		"starting l4 proxy docker-mcp-l4proxy-a1b2c3 for api.github.com, ports 443",
		"failed to remove proxy container docker-mcp-l4proxy-zzz: gone",
		"- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x",
		// A server's stderr reaches the log with the gateway's prefix on
		// every line; a server cannot name a relay for removal.
		"- mcp-npm-scope-server-x: starting l4 proxy docker-mcp-l4proxy-victim for api.github.com, ports 443",
		"- mcp-npm-scope-server-x:     - Starting l7 proxy docker-mcp-l7proxy-victim for [x]",
		"- mcp-npm-scope-server-x: text\rstarting l4 proxy docker-mcp-l4proxy-victim2 for x, ports 443",
	}, "\n")
	got := relaysStartedBy(logs)
	if strings.Join(got, " ") != "docker-mcp-l4proxy-a1b2c3 docker-mcp-l7proxy-d4e5f6" {
		t.Fatalf("relaysStartedBy = %v", got)
	}
	if got := relaysStartedBy("> 3 tools listed\n"); len(got) != 0 {
		t.Fatalf("no relays, got %v", got)
	}
}

// TestRunRefusesForeignRelays (agentry-cli#108): a plan whose declared
// server has egress hosts does not start while another gateway's relay
// sidecars are on the shared relay network — exit 13, the names in the
// error, no gateway started; a plan without such a server ignores them.
func TestRunRefusesForeignRelays(t *testing.T) {
	withoutFilter(t) // DR-22 holds where the filter does not run (agentry-cli#220)
	p, f := serverFixture(t)
	happyFake(f)
	f.ByLabel = map[string][]string{relayLabel: {"docker-mcp-l4proxy-theirs"}}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "docker-mcp-l4proxy-theirs") || !strings.Contains(err.Error(), "shared relay network") {
		t.Fatalf("want exit %d naming the relay, got %v", ExitGateway, err)
	}
	var netRemoved, proxyRemoved bool
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunDetached:agentry-cafe0000-gateway") || strings.HasPrefix(op, "RemoveForce:docker-mcp-l4proxy-theirs") {
			t.Errorf("refused run touched the gateway or the foreign relay: %s", op)
		}
		netRemoved = netRemoved || strings.HasPrefix(op, "NetworkRemove:agentry-cafe0000-net")
		proxyRemoved = proxyRemoved || strings.HasPrefix(op, "RemoveForce:agentry-cafe0000-proxy")
	}
	// What was built before the check is torn down like any failure.
	if !netRemoved || !proxyRemoved {
		t.Errorf("refusal left the run network or the proxy: %v", f.Ops())
	}

	// No declared egress at all, the agent's or a server's: the relays are
	// not this run's concern, and the run proceeds.
	p2, f2 := fixture(t)
	p2.Plan.Capabilities.EgressMode, p2.Plan.Capabilities.EgressHosts = "none", nil
	happyFake(f2)
	f2.ByLabel = map[string][]string{relayLabel: {"docker-mcp-l4proxy-theirs"}}
	if env, err := Run(context.Background(), f2, p2); err != nil || env.Status != "ok" {
		t.Fatalf("run without relays refused: %v", err)
	}
}

// TestTeardownReapsOwnRelaysByName (agentry-cli#106): the relays the
// gateway's log says it started — and the label confirms are relays —
// are removed at teardown, after the gateway's stop and before its
// removal: on the success path, on the path where a declared server
// failed to start, and on the path where the health probe failed before
// the log was snapshotted. A relay the log does not name, and a name the
// log carries that is not a relay, are left alone.
func TestTeardownReapsOwnRelaysByName(t *testing.T) {
	relayLine := "starting l4 proxy docker-mcp-l4proxy-ours for api.example, ports 443\nstarting l4 proxy docker-mcp-l4proxy-notlabelled for api.other, ports 443\n"
	// The gateway creates its relays as it starts: none on the host before
	// (or the run would refuse), ours and a foreign one after.
	gatewayCreatesRelays := func(f *dockerx.Fake) {
		f.ByLabel = map[string][]string{relayLabel: nil}
		f.OnDetached = func(r dockerx.RunSpec) {
			if strings.HasSuffix(r.Name, "-gateway") {
				f.ByLabel[relayLabel] = []string{"docker-mcp-l4proxy-ours", "docker-mcp-l4proxy-theirs"}
				// The filter labels this run's gateway's relays alone.
				f.ByLabel[gatewayOwnerLabel+"=cafe0000"] = []string{"docker-mcp-l4proxy-ours"}
			}
		}
	}
	check := func(t *testing.T, f *dockerx.Fake) {
		t.Helper()
		ops := f.Ops()
		stop, reap, remove := -1, -1, -1
		for i, op := range ops {
			switch {
			case strings.HasPrefix(op, "Stop:agentry-cafe0000-gateway"):
				stop = i
			case op == "RemoveForce:docker-mcp-l4proxy-ours":
				reap = i
			case strings.HasPrefix(op, "RemoveForce:agentry-cafe0000-gateway"):
				remove = i
			case strings.Contains(op, "docker-mcp-l4proxy-theirs") || strings.Contains(op, "docker-mcp-l4proxy-notlabelled"):
				t.Errorf("a relay the log did not name, or a name that is no relay, was touched: %s", op)
			}
		}
		if reap < 0 || stop < 0 || remove < 0 || stop >= reap || reap >= remove {
			t.Fatalf("want Stop(gateway) < RemoveForce(relay) < RemoveForce(gateway); got stop=%d reap=%d remove=%d in %v", stop, reap, remove, ops)
		}
	}
	t.Run("success path", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x\n" + relayLine + "> 4 tools listed in 1.2s\n"}
		gatewayCreatesRelays(f)
		if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
			t.Fatalf("run: %v", err)
		}
		check(t, f)
	})
	// A relay the gateway starts after its health probe — the l4 relay for
	// a declared server's egress host starts when the server first connects
	// — is not in the probe-time snapshot. Teardown reads the log again, so
	// it is reaped all the same (agentry-cli#184, found by runner-r2's
	// mcp-osv fixture).
	t.Run("relay started after the snapshot", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x\n" + relayLine + "> 4 tools listed in 1.2s\n"}
		gatewayCreatesRelays(f)
		f.OnForeground = func(r dockerx.RunSpec) {
			if strings.HasSuffix(r.Name, "-agent") {
				f.LogsByName["agentry-cafe0000-gateway"] += "starting l4 proxy docker-mcp-l4proxy-late for api.osv.dev, ports 443\n"
				f.ByLabel[relayLabel] = append(f.ByLabel[relayLabel], "docker-mcp-l4proxy-late")
				f.ByLabel[gatewayOwnerLabel+"=cafe0000"] = append(f.ByLabel[gatewayOwnerLabel+"=cafe0000"], "docker-mcp-l4proxy-late")
			}
		}
		if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
			t.Fatalf("run: %v", err)
		}
		check(t, f)
		late := false
		for _, op := range f.Ops() {
			if op == "RemoveForce:docker-mcp-l4proxy-late" {
				late = true
			}
		}
		if !late {
			t.Fatalf("the relay started after the snapshot survived teardown: %v", f.Ops())
		}
	})
	t.Run("server failed to start", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": relayLine + "  > Can't start mcp-npm-scope-server-x: failed to connect: calling \"initialize\": EOF\n"}
		gatewayCreatesRelays(f)
		if _, err := Run(context.Background(), f, p); err == nil {
			t.Fatal("a server that did not start let the run proceed")
		}
		check(t, f)
	})
	t.Run("health probe failed before the snapshot", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		f.Foreground["-probe"] = &dockerx.FakeForeground{ExitCode: 1}
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": relayLine + "bind: cannot listen\n"}
		gatewayCreatesRelays(f)
		if _, err := Run(context.Background(), f, p); err == nil {
			t.Fatal("a failed probe let the run proceed")
		}
		check(t, f)
	})
}

// relayHost is the one docker host two runs share: the relay sidecars on
// the shared relay network are host state, not any one run's. listed is a
// barrier — each run's relay listing waits (up to a bound) until both runs
// have listed, the interleaving in which list-then-start is a race.
type relayHost struct {
	mu       sync.Mutex
	relays   map[string]bool
	arrived  int
	listed   chan struct{}
	beside   int // gateways started while another run's relay was up
	gateways int
}

func newRelayHost() *relayHost {
	return &relayHost{relays: map[string]bool{}, listed: make(chan struct{})}
}

// hostDocker is one run's view of the shared host: its own Fake records
// its calls, and the relay list and the relays its gateway starts are the
// host's.
type hostDocker struct {
	*dockerx.Fake
	host  *relayHost
	relay string
}

func (h hostDocker) PSByLabel(ctx context.Context, label string) ([]string, error) {
	if label != relayLabel {
		return h.Fake.PSByLabel(ctx, label)
	}
	_, _ = h.Fake.PSByLabel(ctx, label) // recorded on this run's fake
	// The listing answers with the network as it is now, and returns only
	// once both runs have listed (or a bound passes): neither gateway can
	// start between the two listings.
	h.host.mu.Lock()
	var names []string
	for n := range h.host.relays {
		names = append(names, n)
	}
	h.host.arrived++
	if h.host.arrived == 2 {
		close(h.host.listed)
	}
	h.host.mu.Unlock()
	select {
	case <-h.host.listed:
	case <-time.After(time.Second):
	}
	return names, nil
}

func (h hostDocker) RunDetached(ctx context.Context, r dockerx.RunSpec) error {
	if err := h.Fake.RunDetached(ctx, r); err != nil {
		return err
	}
	if strings.HasSuffix(r.Name, "-gateway") {
		h.host.mu.Lock()
		h.host.gateways++
		if len(h.host.relays) > 0 {
			h.host.beside++
		}
		h.host.relays[h.relay] = true
		h.host.mu.Unlock()
	}
	return nil
}

func (h hostDocker) RemoveForce(ctx context.Context, name string) error {
	h.host.mu.Lock()
	delete(h.host.relays, name)
	h.host.mu.Unlock()
	return h.Fake.RemoveForce(ctx, name)
}

// TestConcurrentRelayRunsDoNotShareTheRelayNetwork (agentry-cli#268): two
// runs whose plans need relays, started together on one host, both list
// the relay network before either gateway is up. Listing and then starting
// with nothing held across the two let both see an empty network and both
// start, each gateway's relays beside the other's. The second must be
// refused.
func TestConcurrentRelayRunsDoNotShareTheRelayNetwork(t *testing.T) {
	withoutFilter(t) // DR-22 holds where the filter does not run (agentry-cli#220)
	host := newRelayHost()
	home := t.TempDir()
	type result struct {
		env *receipt.Envelope
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		p, f := serverFixture(t)
		happyFake(f)
		p.RunID = fmt.Sprintf("cafe000%d", i)
		p.RunnerHome = home
		relay := fmt.Sprintf("docker-mcp-l4proxy-run%d", i)
		f.LogsByName = map[string]string{"agentry-" + p.RunID + "-gateway": "starting l4 proxy " + relay + " for api.example, ports 443\n"}
		wg.Add(1)
		go func() {
			defer wg.Done()
			env, err := Run(context.Background(), hostDocker{Fake: f, host: host, relay: relay}, p)
			results[i] = result{env, err}
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, r := range results {
		var ce *ClassifiedError
		switch {
		case r.err == nil && r.env.Status == "ok":
			ok++
		case errors.As(r.err, &ce) && ce.Code == ExitGateway:
			refused++
		default:
			t.Errorf("unexpected outcome: %v", r.err)
		}
	}
	if host.beside != 0 || ok != 1 || refused != 1 {
		t.Fatalf("two relay runs at once: %d ran, %d refused, %d gateways of %d started beside another run's relays", ok, refused, host.beside, host.gateways)
	}
}

// TestRelayLockIsHeldForTheWholeRunAndReleasedOnEveryPath
// (agentry-cli#268): the lock is taken before the relay listing, held
// while the run is going — a relay can start late, after the gateway's
// probe — and released by teardown after the relays are reaped, on the
// success path and on a refusal alike; a run without relays never takes it.
func TestRelayLockIsHeldForTheWholeRunAndReleasedOnEveryPath(t *testing.T) {
	withoutFilter(t) // DR-22 holds where the filter does not run (agentry-cli#220)
	heldNow := func(home string) bool {
		release, err := holdRelayLock(home)
		if err != nil {
			return true
		}
		release()
		return false
	}
	t.Run("held during the run, released after the reap", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		p.RunnerHome = t.TempDir()
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "starting l4 proxy docker-mcp-l4proxy-ours for api.example, ports 443\n"}
		f.ByLabel = map[string][]string{relayLabel: nil}
		var duringAgent, atReap bool
		f.OnForeground = func(r dockerx.RunSpec) {
			if strings.HasSuffix(r.Name, "-agent") {
				duringAgent = heldNow(p.RunnerHome)
				f.ByLabel[relayLabel] = []string{"docker-mcp-l4proxy-ours"}
			}
		}
		d := &reapProbe{Fake: f, onRemove: func(name string) {
			if name == "docker-mcp-l4proxy-ours" {
				atReap = heldNow(p.RunnerHome)
			}
		}}
		if env, err := Run(context.Background(), d, p); err != nil || env.Status != "ok" {
			t.Fatalf("run: %v", err)
		}
		if !duringAgent || !atReap {
			t.Fatalf("lock held while the agent ran: %v, while the relays were reaped: %v", duringAgent, atReap)
		}
		if heldNow(p.RunnerHome) {
			t.Fatal("the lock outlived the run")
		}
	})
	t.Run("released when the relay listing refuses", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		p.RunnerHome = t.TempDir()
		f.ByLabel = map[string][]string{relayLabel: {"docker-mcp-l4proxy-theirs"}}
		if _, err := Run(context.Background(), f, p); err == nil {
			t.Fatal("foreign relays did not refuse the run")
		}
		if heldNow(p.RunnerHome) {
			t.Fatal("a refused run kept the lock")
		}
	})
	t.Run("a held lock refuses before anything is listed", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		p.RunnerHome = t.TempDir()
		release, err := holdRelayLock(p.RunnerHome)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		_, err = Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), RelayLockFile) {
			t.Fatalf("want exit %d naming the lock, got %v", ExitGateway, err)
		}
		for _, op := range f.Ops() {
			if strings.HasPrefix(op, "PSByLabel:"+relayLabel) || strings.HasPrefix(op, "RunDetached:agentry-cafe0000-gateway") {
				t.Errorf("a run refused at the lock went on to %s", op)
			}
		}
	})
	t.Run("a run without relays takes no lock", func(t *testing.T) {
		p, f := fixture(t)
		p.Plan.Capabilities.EgressMode, p.Plan.Capabilities.EgressHosts = "none", nil
		happyFake(f)
		p.RunnerHome = t.TempDir()
		release, err := holdRelayLock(p.RunnerHome)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
			t.Fatalf("a run with no relays was held by the relay lock: %v", err)
		}
	})
}

// reapProbe observes each forced removal as it happens.
type reapProbe struct {
	*dockerx.Fake
	onRemove func(name string)
}

func (r *reapProbe) RemoveForce(ctx context.Context, name string) error {
	r.onRemove(name)
	return r.Fake.RemoveForce(ctx, name)
}

// withoutFilter runs a test as on a platform without the gateway's docker
// filter, where the relay network is the host's one fixed network (DR-22).
func withoutFilter(t *testing.T) {
	t.Helper()
	withFilter(t, false)
}
