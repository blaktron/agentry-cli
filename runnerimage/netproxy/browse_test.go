package netproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The browse proxy: it admits the run's sites with their
// subdomains, the declared hosts, or any public name; it resolves every
// name itself and dials only the address it checked, on 80 or 443 or a
// site's own port; private, loopback, link-local, CGNAT and metadata
// addresses are refused but a person-typed LAN site's; the SNI check
// holds; and every refusal is logged with its reason.

func TestScopeAdmits(t *testing.T) {
	s := &Scope{
		Sites: []string{"Shop.Example", "staging.lan:8080"},
		Hosts: []string{"fonts.googleapis.com", "*.cdnhost.net"},
	}
	for _, c := range []struct {
		host, port string
		want       bool
		why        string
	}{
		{"shop.example", "443", true, ""},
		{"SHOP.example.", "80", true, ""},
		{"cdn.shop.example", "443", true, ""},
		{"a.b.shop.example", "443", true, ""},
		{"evilshop.example", "443", false, "out of scope"},
		{"shop.example.evil", "443", false, "out of scope"},
		{"shop.example", "8443", false, "port 8443 is not 80 or 443"},
		{"staging.lan", "8080", true, ""},
		{"api.staging.lan", "8080", false, "port 8080 is not 80 or 443"},
		{"staging.lan", "443", true, ""},
		{"fonts.googleapis.com", "443", true, ""},
		{"maps.googleapis.com", "443", false, "out of scope"},
		{"js.cdnhost.net", "443", true, ""},
		{"cdnhost.net", "443", false, "out of scope"},
		{"evilcdnhost.net", "443", false, "out of scope"},
		{"accounts.google.com", "443", false, "out of scope"},
	} {
		got, why := s.inScope(c.host, c.port)
		if got != c.want || why != c.why {
			t.Errorf("inScope(%s:%s) = %v %q, want %v %q", c.host, c.port, got, why, c.want, c.why)
		}
	}
	pub := &Scope{Public: true}
	if ok, _ := pub.inScope("anything.example", "443"); !ok {
		t.Error("public scope refuses a name")
	}
	if ok, why := pub.inScope("anything.example", "22"); ok || why != "port 22 is not 80 or 443" {
		t.Errorf("public scope on port 22: %v %q", ok, why)
	}
}

// dialRecorder stands in for the internet: it records the address each
// upstream connection is opened to and connects it to to.
type dialRecorder struct {
	mu   sync.Mutex
	to   string
	seen []string
}

func (d *dialRecorder) dial(ctx context.Context, address string) (net.Conn, error) {
	d.mu.Lock()
	d.seen = append(d.seen, address)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.to)
}

func (d *dialRecorder) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// TestBrowseTunnelDialsTheAddressItChecked: a site in scope is tunnelled
// to the address the proxy resolved, and the ClientHello still names it.
func TestBrowseTunnelDialsTheAddressItChecked(t *testing.T) {
	echo := startTLSEcho(t)
	d := &dialRecorder{to: echo.Addr().String()}
	logs := &lockedBuf{}
	p := &Proxy{Scope: &Scope{Sites: []string{"shop.example"}}, Resolver: fakeResolver{"93.184.216.34"}, Logw: logs, dial: d.dial}
	addr := startProxy(t, p)
	if err := tlsThrough(openTunnel(t, addr, "cdn.shop.example:443"), "cdn.shop.example"); err != nil {
		t.Fatal(err)
	}
	if got := d.addresses(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %v, want the checked address", got)
	}
	if !strings.Contains(logs.String(), "allow cdn.shop.example:443") {
		t.Fatalf("log: %s", logs.String())
	}
	// A ClientHello for another name is refused, as for every tunnel.
	if err := tlsThrough(openTunnel(t, addr, "shop.example:443"), "other.example"); err == nil {
		t.Fatal("a mismatched server name passed")
	}
	if !strings.Contains(logs.String(), `deny sni "other.example" for shop.example:443`) {
		t.Fatalf("log: %s", logs.String())
	}
}

// TestBrowseRefusesWithAReason: out of scope, a port, an address that is
// not public, and Chrome's own services, each logged with why.
func TestBrowseRefusesWithAReason(t *testing.T) {
	for _, c := range []struct {
		name, target string
		answer       []string
		scope        Scope
		logged       string
	}{
		{"out of scope", "evil.example:443", []string{"93.184.216.34"}, Scope{Sites: []string{"shop.example"}},
			"deny evil.example:443 (out of scope)"},
		{"another port", "shop.example:22", []string{"93.184.216.34"}, Scope{Sites: []string{"shop.example"}},
			"deny shop.example:22 (port 22 is not 80 or 443)"},
		{"a private answer", "shop.example:443", []string{"10.1.2.3"}, Scope{Sites: []string{"shop.example"}},
			"deny shop.example:443 (it resolves to 10.1.2.3, which is not a public address)"},
		{"one private answer among public ones", "shop.example:443", []string{"93.184.216.34", "192.168.1.1"}, Scope{Sites: []string{"shop.example"}},
			"which is not a public address"},
		{"metadata in public scope", "meta.example:80", []string{"169.254.169.254"}, Scope{Public: true},
			"deny meta.example:80 (it resolves to 169.254.169.254, which is not a public address)"},
		{"a tailnet answer", "shop.example:443", []string{"100.101.102.103"}, Scope{Sites: []string{"shop.example"}},
			"which is not a public address"},
		{"an address literal", "127.0.0.1:443", nil, Scope{Public: true},
			"deny 127.0.0.1:443 (127.0.0.1 is not a public address)"},
		{"Chrome's own service", "update.googleapis.com:443", []string{"142.250.80.46"}, Scope{Sites: []string{"shop.example"}},
			"deny update.googleapis.com:443 (out of scope) [chrome service]"},
		{"a private site's loopback", "staging.lan:443", []string{"127.0.0.1"}, Scope{Sites: []string{"staging.lan"}, Private: []string{"staging.lan"}},
			"deny staging.lan:443 (it resolves to 127.0.0.1, which is not a public address)"},
		{"a private site's link-local", "staging.lan:443", []string{"169.254.169.254"}, Scope{Sites: []string{"staging.lan"}, Private: []string{"staging.lan"}},
			"which is not a public address"},
		{"a private site's subdomain", "api.staging.lan:443", []string{"192.168.1.20"}, Scope{Sites: []string{"staging.lan"}, Private: []string{"staging.lan"}},
			"which is not a public address"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &lockedBuf{}
			d := &dialRecorder{to: "127.0.0.1:1"}
			scope := c.scope
			p := &Proxy{Scope: &scope, Resolver: fakeResolver(c.answer), Logw: logs, dial: d.dial}
			_, _, status := dialProxy(t, startProxy(t, p), http.MethodConnect, c.target, c.target)
			if !strings.HasPrefix(status, "HTTP/1.1 403") {
				t.Fatalf("status %q, want 403", status)
			}
			if !strings.Contains(logs.String(), c.logged) {
				t.Fatalf("log %q lacks %q", logs.String(), c.logged)
			}
			if got := d.addresses(); len(got) != 0 {
				t.Fatalf("a refused connection was dialled: %v", got)
			}
		})
	}
}

// TestBrowseAPersonTypedLANSite: in a local run, the exact private
// host the person typed is reached on its own port, plain http included.
func TestBrowseAPersonTypedLANSite(t *testing.T) {
	var host string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		_, _ = w.Write([]byte("staging ok"))
	}))
	t.Cleanup(upstream.Close)
	d := &dialRecorder{to: strings.TrimPrefix(upstream.URL, "http://")}
	logs := &lockedBuf{}
	p := &Proxy{Scope: &Scope{Sites: []string{"staging.lan:8080"}, Private: []string{"staging.lan"}}, Resolver: fakeResolver{"192.168.1.20"}, Logw: logs, dial: d.dial}
	addr := startProxy(t, p)
	resp, err := plainClient(t, addr).Get("http://staging.lan:8080/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || host != "staging.lan:8080" {
		t.Fatalf("status %d, Host %q", resp.StatusCode, host)
	}
	if got := d.addresses(); len(got) != 1 || got[0] != "192.168.1.20:8080" {
		t.Fatalf("dialled %v", got)
	}
	// The same site, not typed by the person, is held to public addresses.
	p2 := &Proxy{Scope: &Scope{Sites: []string{"staging.lan:8080"}}, Resolver: fakeResolver{"192.168.1.20"}, Logw: logs, dial: d.dial}
	resp, err = plainClient(t, startProxy(t, p2)).Get("http://staging.lan:8080/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(logs.String(), "deny http staging.lan:8080 (it resolves to 192.168.1.20, which is not a public address)") {
		t.Fatalf("status %d, log %s", resp.StatusCode, logs.String())
	}
}

// TestBrowseForwardsAPagesPlainHTTP: an http:// page in scope is forwarded,
// and one out of scope is refused
// without a connection.
func TestBrowseForwardsAPagesPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Host + " " + r.URL.Path))
	}))
	t.Cleanup(upstream.Close)
	d := &dialRecorder{to: strings.TrimPrefix(upstream.URL, "http://")}
	logs := &lockedBuf{}
	p := &Proxy{Scope: &Scope{Sites: []string{"shop.example"}}, Resolver: fakeResolver{"93.184.216.34"}, Logw: logs, dial: d.dial}
	c := plainClient(t, startProxy(t, p))
	resp, err := c.Get("http://shop.example/about")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in scope: status %d", resp.StatusCode)
	}
	resp, err = c.Get("http://tracker.example/pixel")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(logs.String(), "deny http tracker.example:80 (out of scope)") {
		t.Fatalf("out of scope: status %d, log %s", resp.StatusCode, logs.String())
	}
	if got := d.addresses(); len(got) != 1 || got[0] != "93.184.216.34:80" {
		t.Fatalf("dialled %v", got)
	}
	// An origin-form request is not a proxy request.
	_, _, status := dialProxy(t, startProxy(t, p), http.MethodGet, "/", "shop.example")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("origin-form: %q", status)
	}
}
