package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
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

	// No declared server with egress: the relays are not this run's
	// concern, and the run proceeds.
	p2, f2 := fixture(t)
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
