package engine

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/hostpin"
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

// An endpoint on this computer itself (localhost, or a name this machine
// answers with a loopback address, as a Mac answers its own .local name)
// is pinned to the engine's host gateway instead, in a local run only,
// because inside the container a loopback address names the container. A
// name this machine says does not exist is passed to the proxy as
// unresolved, so that a request to it is answered in plain words instead
// of the container's own "no such host".

// lookupModelHost resolves a model endpoint's host with this machine's
// resolver; a var so tests never touch DNS.
var lookupModelHost = hostpin.Lookup

// thisHostname is this machine's name; a var so tests choose it.
var thisHostname = os.Hostname

// modelHosts answers the proxy's --add-host entries and unresolved hosts
// for the allowlist (hostpin.ResolveModelHosts); local says the run is
// this computer's own, not a hosted run's.
func modelHosts(ctx context.Context, allow []string, local bool) hostpin.ModelHosts {
	// Without a hostname only localhost and loopback answers name this
	// computer.
	name, _ := thisHostname()
	return hostpin.ResolveModelHosts(ctx, allow, lookupModelHost, name, local)
}

// allowHost is the host of a proxyAllow entry (hostpin.AllowHost).
func allowHost(entry string) string { return hostpin.AllowHost(entry) }

// modelHostsLine is the receipt's account of the pins and the unresolved
// hosts; "" when there are neither.
func modelHostsLine(mh hostpin.ModelHosts) string {
	var addressed, parts []string
	for _, pin := range mh.Pins {
		if !strings.HasSuffix(pin, ":"+hostpin.HostGateway) {
			addressed = append(addressed, pin)
		}
	}
	if len(addressed) > 0 {
		parts = append(parts, fmt.Sprintf("the llm-proxy reaches %s at the address this machine resolves for it", strings.Join(addressed, ", ")))
	}
	if len(mh.Gateway) > 0 {
		parts = append(parts, fmt.Sprintf("the llm-proxy reaches %s, which names this computer, at the container engine's host gateway", strings.Join(mh.Gateway, ", ")))
	}
	if len(parts) > 0 {
		parts[len(parts)-1] += ", pinned into the proxy's /etc/hosts; the allowlist is still enforced by name"
	}
	if len(mh.Unresolved) > 0 {
		parts = append(parts, fmt.Sprintf("%s does not resolve on this machine, and the proxy says so to a request for it", strings.Join(mh.Unresolved, ", ")))
	}
	return strings.Join(parts, "; ")
}
