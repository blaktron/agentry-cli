package netproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
)

// The browse proxy (agentry-notes plans/browser-testing.md §6, D1, D2,
// D13; agentry-cli#915): the only route out of a run's browser. Where an
// allowlisting proxy admits exact host:port pairs, it holds a scope: the
// sites the person named at run time with their subdomains, the hosts the
// release declares, or any public name. Every name is resolved here and
// only the address checked is dialled, on 80 or 443 (a site's own port
// where the person typed one), and a TLS tunnel still has its ClientHello
// held to its host. A page that browses outside the scope gets a refusal
// it sees as a failed request, which the agent can report.

// Scope is what a browse proxy admits.
type Scope struct {
	// Sites are the run's sites, host or host:port: each admits the host
	// and its subdomains, on 80 and 443, and the exact host on its own
	// port where one is given.
	Sites []string
	// Hosts are the release's fixed hosts, an exact name or a "*." suffix
	// (any subdomain of the name, not the name itself).
	Hosts []string
	// Public admits any name that resolves to public addresses only.
	Public bool
	// Private are a local run's sites the person typed whose addresses are
	// a LAN's or a tailnet's (plan D2): the exact host may resolve there.
	// Loopback, link-local (cloud metadata), multicast and unspecified
	// addresses are never dialled, for these either.
	Private []string
}

// browserOwn are the hosts Chrome reaches for its own services whatever a
// page does (update, sign-in, component and safe-browsing services; M1
// saw accounts.google.com, android.clients.google.com and
// update.googleapis.com with every policy tried). A refusal of one is
// logged as such, so a reader can tell it from a page's; a page may still
// use one, and browser_network_requests, which lists the page's own
// requests alone, is what the agent reports from.
var browserOwn = []string{
	"accounts.google.com", "android.clients.google.com", "update.googleapis.com",
	"clients2.google.com", "clientservices.googleapis.com", "optimizationguide-pa.googleapis.com",
	"content-autofill.googleapis.com", "safebrowsing.googleapis.com", "safebrowsingohttpgateway.googleapis.com",
	"redirector.gvt1.com", "edgedl.me.gvt1.com",
}

// lower is ASCII lower case, as host names compare (asciiEqualFold).
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// inScope says whether the scope admits host on port, before any address
// is looked at.
func (s *Scope) inScope(host, port string) (bool, string) {
	host = lower(strings.TrimSuffix(host, "."))
	standard := port == "80" || port == "443"
	for _, site := range s.Sites {
		sh, sp := splitEntry(lower(site), "")
		if host == sh && sp != "" && port == sp {
			return true, ""
		}
		if standard && (host == sh || strings.HasSuffix(host, "."+sh)) {
			return true, ""
		}
	}
	if !standard {
		return false, "port " + port + " is not 80 or 443"
	}
	for _, h := range s.Hosts {
		h = lower(h)
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true, ""
			}
			continue
		}
		if host == h {
			return true, ""
		}
	}
	if s.Public {
		return true, ""
	}
	return false, "out of scope"
}

// private says whether host is a site the person typed whose addresses may
// be a LAN's or a tailnet's.
func (s *Scope) private(host string) bool {
	host = lower(strings.TrimSuffix(host, "."))
	return slices.ContainsFunc(s.Private, func(h string) bool { return lower(h) == host })
}

// lanAddress is an address a person-typed private site may have (plan D2):
// a private range's (RFC 1918, IPv6 ULA) or shared space's (CGNAT, where a
// tailnet lives), never loopback, link-local, multicast or unspecified.
func lanAddress(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// scopedAddress judges one connection the browser asks for: the host in
// scope on the port, then the address it resolves to, which is what is
// dialled. reason is set when it is refused.
func (p *Proxy) scopedAddress(ctx context.Context, host, port string) (netip.Addr, string) {
	if ok, why := p.Scope.inScope(host, port); !ok {
		return netip.Addr{}, why
	}
	allow := netaddr.Public
	if p.Scope.private(host) {
		allow = func(a netip.Addr) bool { return netaddr.Public(a) || lanAddress(a) }
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !allow(a) {
			return netip.Addr{}, a.Unmap().String() + " is not a public address"
		}
		return a.Unmap(), ""
	}
	var res Resolver = net.DefaultResolver
	if p.Resolver != nil {
		res = p.Resolver
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	addrs, err := res.LookupIPAddr(ctx, host)
	if err != nil {
		return netip.Addr{}, "it does not resolve"
	}
	if len(addrs) == 0 {
		return netip.Addr{}, "it resolves to no address"
	}
	var first netip.Addr
	for i, ia := range addrs {
		a, ok := netip.AddrFromSlice(ia.IP)
		if !ok || !allow(a) {
			return netip.Addr{}, fmt.Sprintf("it resolves to %s, which is not a public address", ia.IP)
		}
		if i == 0 {
			first = a.Unmap()
		}
	}
	return first, ""
}

// denyBrowse logs a refusal with its host, port and reason, marking one of
// Chrome's own service hosts, and answers the browser with it.
func (p *Proxy) denyBrowse(w http.ResponseWriter, kind, host, port, why string) {
	mark := ""
	if slices.Contains(browserOwn, lower(host)) {
		mark = " [chrome service]"
	}
	p.logf("deny %s%s:%s (%s)%s", kind, host, port, why, mark)
	http.Error(w, fmt.Sprintf("forbidden: %s:%s: %s", host, port, why), http.StatusForbidden)
}

// forwardScoped relays one absolute-form http request a page made: the
// URL's host:port judged by the scope, the address checked dialled again
// at connect time (browseTransport), the Host header set to the URL's,
// no redirect followed (the browser asks again, and is judged again).
func (p *Proxy) forwardScoped(w http.ResponseWriter, r *http.Request) {
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
	}
	if host == "" {
		p.denyBrowse(w, "http ", host, port, "no host")
		return
	}
	if _, why := p.scopedAddress(r.Context(), host, port); why != "" {
		p.denyBrowse(w, "http ", host, port, why)
		return
	}
	p.logf("allow http %s:%s", host, port)
	p.relayPlain(w, r, p.browseTransport(), host, port)
}

// browseTransport is plain forwarding's transport for a scope: every
// connection it opens is judged again and dialled at the address checked,
// so no second answer from the name server slips in between.
func (p *Proxy) browseTransport() *http.Transport {
	p.tOnce.Do(func() {
		p.transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				a, why := p.scopedAddress(ctx, host, port)
				if why != "" {
					return nil, fmt.Errorf("%s: %s", host, why)
				}
				return p.dialUpstream(ctx, net.JoinHostPort(a.String(), port))
			},
			DisableCompression:    true,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}
	})
	return p.transport
}
