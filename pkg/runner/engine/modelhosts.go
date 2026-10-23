package engine

import (
	"context"
	"fmt"
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

// lookupModelHost resolves a model endpoint's host with this machine's
// resolver; a var so tests never touch DNS.
var lookupModelHost = hostpin.Lookup

// modelHostPins answers the proxy's --add-host entries ("name:ip") for the
// allowlist's hosts (hostpin.Pins).
func modelHostPins(ctx context.Context, allow []string) []string {
	return hostpin.Pins(ctx, allow, lookupModelHost)
}

// allowHost is the host of a proxyAllow entry (hostpin.AllowHost).
func allowHost(entry string) string { return hostpin.AllowHost(entry) }

// modelHostsLine is the receipt's account of the pins.
func modelHostsLine(pins []string) string {
	return fmt.Sprintf("the llm-proxy reaches %s at the address this machine resolves for it, pinned into the proxy's /etc/hosts; the allowlist is still enforced by name",
		strings.Join(pins, ", "))
}
