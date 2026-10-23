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
	"os"
	"slices"
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

// resolvConfs are where this machine names its name servers: the system's
// file, then systemd-resolved's list of its upstream servers, read where
// the first names only its local stub.
var resolvConfs = []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"}

// Upstreams are this machine's name servers as a container can reach them,
// for a proxy that may reach any public host, whose names cannot be pinned
// ahead: the IPv4 servers of the first file of resolvConfs that names any
// besides loopback ones, at most three, as a resolv.conf reads them. Nil
// when none does.
func Upstreams() []string {
	for _, f := range resolvConfs {
		b, err := os.ReadFile(f) //nolint:gosec // a fixed system path
		if err != nil {
			continue
		}
		if rs := nameservers(string(b)); len(rs) > 0 {
			return rs
		}
	}
	return nil
}

// nameservers are a resolv.conf's IPv4 name servers that are not loopback
// or unspecified, at most three.
func nameservers(conf string) []string {
	var rs []string
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(f[1]).To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || slices.Contains(rs, ip.String()) {
			continue
		}
		if rs = append(rs, ip.String()); len(rs) == 3 {
			break
		}
	}
	return rs
}
