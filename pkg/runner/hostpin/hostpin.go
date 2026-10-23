// Package hostpin pins the hosts a proxy may reach to the addresses this
// machine resolves for them, as --add-host entries for the proxy's
// /etc/hosts. A proxy dials from inside its container, where names go to
// the engine's DNS: on Docker the embedded server (127.0.0.11), which knows
// no search domain, no mDNS and no split DNS of the host; on rootless
// podman, a container on an internal network and the default one is told
// only the internal network's name server, which answers no outside name
// at all. A pin changes where an allowed name points, never what the proxy
// may reach: its allowlist is still enforced by name.
package hostpin

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"
)

// LookupTimeout bounds each lookup: a name the host cannot resolve quickly
// is left to the container's DNS.
const LookupTimeout = 2 * time.Second

// Lookup resolves a host with this machine's resolver, bounded by
// LookupTimeout.
func Lookup(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
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

// Pins answers the proxy's --add-host entries ("name:ip") for the
// allowlist's hosts, resolved with lookup. Only IPv4 answers are pinned,
// since a proxy's network may have no IPv6 route; IP literals, names that
// do not resolve here, and loopback or unspecified answers (which inside a
// container would name the container itself) are left to the container's
// DNS.
func Pins(ctx context.Context, allow []string, lookup func(context.Context, string) ([]net.IP, error)) []string {
	seen := map[string]bool{}
	var pins []string
	for _, entry := range allow {
		host := AllowHost(entry)
		if host == "" || seen[host] || net.ParseIP(host) != nil {
			continue
		}
		seen[host] = true
		ips, err := lookup(ctx, host)
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

// AllowHost is the host of an allowlist entry (host, host:port or
// [v6]:port), lower-cased as a proxy compares it.
func AllowHost(entry string) string {
	entry = strings.TrimSpace(entry)
	if h, _, err := net.SplitHostPort(entry); err == nil {
		entry = h
	}
	return strings.ToLower(strings.Trim(entry, "[]"))
}
