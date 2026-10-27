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
	"errors"
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
// since a proxy's network may have no IPv6 route; IP literals, the
// engines' own names, names that do not resolve here, and loopback or
// unspecified answers (which inside a container would name the container
// itself) are left to the container's DNS.
func Pins(ctx context.Context, allow []string, lookup func(context.Context, string) ([]net.IP, error)) []string {
	return ResolveModelHosts(ctx, allow, lookup, "", false).Pins
}

// HostGateway is the address an --add-host entry gives a name that means
// this computer. Docker and podman put the address a container reaches the
// engine's host at in its place; on Docker Desktop that address also
// reaches services the host has bound to its loopback.
const HostGateway = "host-gateway"

// LoopbackAlias is the name a plan gives a model endpoint on this
// computer's loopback when its harness runs in a container, where
// "localhost" would name the container and is never proxied. It is under
// .internal, which no public name uses, and only a local run pins it.
const LoopbackAlias = "agentry-host.internal"

// ModelHosts is what ResolveModelHosts found for a model allowlist.
type ModelHosts struct {
	// Pins are the proxy's --add-host entries: "name:ip" for an endpoint
	// elsewhere, "name:host-gateway" for one on this computer.
	Pins []string
	// Gateway are the hosts of Pins that name this computer.
	Gateway []string
	// Unresolved are the hosts this machine's resolver says do not exist,
	// so that the proxy can say so in plain words when one is dialled.
	Unresolved []string
}

// ResolveModelHosts resolves a model allowlist's hosts with lookup, as
// Pins does, and, for a run on this computer (local), pins every host
// that means this computer to HostGateway: localhost and *.localhost, a
// name with a loopback answer (a Mac answers its own .local name with
// 127.0.0.1 beside its LAN address, and many Linux hosts map their own
// name to 127.0.1.1), and the machine's own name (hostname, its first
// label, or that label with .local) when the lookup fails. Inside the
// proxy's container a loopback address names the container itself, so
// such an endpoint would otherwise be unreachable. A run whose caller says
// it is not local (a hosted run's) never reaches its host this way. A name the lookup reports as not
// existing (not one that timed out) is Unresolved. IP literals and the
// engines' own names (host.docker.internal, host.containers.internal) are
// left to the container.
func ResolveModelHosts(ctx context.Context, allow []string, lookup func(context.Context, string) ([]net.IP, error), hostname string, local bool) ModelHosts {
	var out ModelHosts
	seen := map[string]bool{}
	for _, entry := range allow {
		host := AllowHost(entry)
		if host == "" || seen[host] || net.ParseIP(host) != nil || engineName(host) || (host == LoopbackAlias && !local) {
			continue
		}
		seen[host] = true
		if local && (isLocalhost(host) || host == LoopbackAlias) {
			out.Gateway = append(out.Gateway, host)
			continue
		}
		ips, err := lookup(ctx, host)
		switch {
		case err != nil && local && namesThisComputer(host, hostname):
			out.Gateway = append(out.Gateway, host)
		case err != nil && unresolvable(host, err):
			out.Unresolved = append(out.Unresolved, host)
		case err != nil:
		case local && slices.ContainsFunc(ips, net.IP.IsLoopback):
			out.Gateway = append(out.Gateway, host)
		default:
			out.Pins = append(out.Pins, addressPins(host, ips)...)
		}
	}
	for _, host := range out.Gateway {
		out.Pins = append(out.Pins, host+":"+HostGateway)
	}
	sort.Strings(out.Pins)
	sort.Strings(out.Gateway)
	sort.Strings(out.Unresolved)
	return out
}

// addressPins are host's "name:ip" entries for its usable IPv4 answers.
func addressPins(host string, ips []net.IP) []string {
	var pins []string
	done := map[string]bool{}
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() || done[v4.String()] {
			continue
		}
		done[v4.String()] = true
		pins = append(pins, host+":"+v4.String())
	}
	return pins
}

// isLocalhost is true for localhost and the names under it (RFC 6761).
func isLocalhost(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// engineName is true for the names a container engine answers itself.
func engineName(host string) bool {
	return host == "host.containers.internal" || strings.HasSuffix(host, ".docker.internal")
}

// namesThisComputer is true when host is hostname, its first label, or
// that label with .local, the name a Mac or an Avahi host answers to.
func namesThisComputer(host, hostname string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "" {
		return false
	}
	label, _, _ := strings.Cut(hostname, ".")
	return host == hostname || host == label || host == label+".local"
}

// unresolvable is true when a failed lookup of host means the proxy's
// container cannot resolve it either: the resolver answered that the name
// does not exist, or host is a .local name, which only multicast DNS on
// this machine answers (a missing one times out or fails rather than
// being reported missing) and a container engine's DNS never does.
func unresolvable(host string, err error) bool {
	var dnsErr *net.DNSError
	return strings.HasSuffix(host, ".local") || (errors.As(err, &dnsErr) && dnsErr.IsNotFound)
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

// maxUpstreams is the most name servers a resolv.conf is read for.
const maxUpstreams = 3

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
// or unspecified, at most maxUpstreams.
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
		if rs = append(rs, ip.String()); len(rs) == maxUpstreams {
			break
		}
	}
	return rs
}
