package engine

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// The llm-proxy dials the model endpoint from inside its container, where
// names go to docker's embedded DNS (127.0.0.11): no search domain, no
// mDNS, no /etc/hosts of the host, and on Linux no loopback name server
// such as systemd-resolved's, so no split DNS (Tailscale's MagicDNS). An
// endpoint the operator reaches from this machine by such a name failed at
// the first model call with "no such host" (a Tailscale short name on
// Docker Desktop). So the engine resolves each allowlisted host the way
// this machine does and pins the answer into the proxy's /etc/hosts. The
// allowlist is still enforced by name: a pin changes where an allowed name
// points, never what the proxy may reach.

// modelHostLookupTimeout bounds each lookup: a name the host cannot
// resolve quickly is left to the container's DNS.
const modelHostLookupTimeout = 2 * time.Second

// lookupModelHost resolves a model endpoint's host with this machine's
// resolver; a var so tests never touch DNS.
var lookupModelHost = func(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, modelHostLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// modelHostPins answers the proxy's --add-host entries ("name:ip") for the
// allowlist's hosts. Only IPv4 answers are pinned, since a run's bridge
// network may have no IPv6 route; IP literals, names that do not resolve
// here, and loopback or unspecified answers (which inside a container
// would name the container itself) are left to the container's DNS.
func modelHostPins(ctx context.Context, allow []string) []string {
	seen := map[string]bool{}
	var pins []string
	for _, entry := range allow {
		host := allowHost(entry)
		if host == "" || seen[host] || net.ParseIP(host) != nil {
			continue
		}
		seen[host] = true
		ips, err := lookupModelHost(ctx, host)
		if err != nil {
			continue
		}
		done := map[string]bool{}
		for _, ip := range ips {
			v4 := ip.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() || done[v4.String()] {
				continue
			}
			done[v4.String()] = true
			pins = append(pins, host+":"+v4.String())
		}
	}
	sort.Strings(pins)
	return pins
}

// allowHost is the host of a proxyAllow entry (host, host:port or
// [v6]:port), lower-cased as the proxy compares it.
func allowHost(entry string) string {
	entry = strings.TrimSpace(entry)
	if h, _, err := net.SplitHostPort(entry); err == nil {
		entry = h
	}
	return strings.ToLower(strings.Trim(entry, "[]"))
}

// modelHostsLine is the receipt's account of the pins.
func modelHostsLine(pins []string) string {
	return fmt.Sprintf("the llm-proxy reaches %s at the address this machine resolves for it, pinned into the proxy's /etc/hosts; the allowlist is still enforced by name",
		strings.Join(pins, ", "))
}
