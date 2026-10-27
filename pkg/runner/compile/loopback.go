package compile

import (
	"net"
	"net/url"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/hostpin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// LoopbackAlias is the name a model endpoint on this computer's loopback
// (localhost, 127.0.0.1, ::1) is given in the plan of a harness that runs
// in a container. An HTTP client never sends a request for localhost or a
// loopback address through its proxy, and the harnesses' NO_PROXY names
// localhost and so every name under it, so the harness would dial its own
// container's loopback and find nothing. The alias is proxied like any
// other name, and the engine pins it to the container engine's host
// gateway for a run on this computer. The proxy's allowlist names the
// alias in place of the loopback host, with the same port.
const LoopbackAlias = hostpin.LoopbackAlias

// withLoopbackAlias returns pol with every model endpoint on the loopback
// (model.baseUrl, each harness's baseUrl and their model.proxyAllow
// entries) renamed to LoopbackAlias; pol itself when none is.
func withLoopbackAlias(pol *policy.Policy) *policy.Policy {
	if pol == nil {
		return nil
	}
	changed := false
	model := pol.Model
	if u, ok := aliasURL(model.BaseURL); ok {
		model.BaseURL, changed = u, true
	}
	allow := make([]string, len(model.ProxyAllow))
	for i, entry := range model.ProxyAllow {
		allow[i] = entry
		if a, ok := aliasEntry(entry); ok {
			allow[i], changed = a, true
		}
	}
	model.ProxyAllow = allow
	var harness map[string]policy.HarnessPolicy
	if pol.Harness != nil {
		harness = make(map[string]policy.HarnessPolicy, len(pol.Harness))
		for name, hp := range pol.Harness {
			if u, ok := aliasURL(hp.BaseURL); ok {
				hp.BaseURL, changed = u, true
			}
			harness[name] = hp
		}
	}
	if !changed {
		return pol
	}
	out := *pol
	out.Model, out.Harness = model, harness
	return &out
}

// loopbackHost is true for localhost and a loopback address.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// aliasURL is raw with a loopback host renamed to LoopbackAlias.
func aliasURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || !loopbackHost(u.Hostname()) {
		return raw, false
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(LoopbackAlias, port)
	} else {
		u.Host = LoopbackAlias
	}
	return u.String(), true
}

// aliasEntry is a proxyAllow entry (host or host:port) with a loopback host
// renamed to LoopbackAlias.
func aliasEntry(entry string) (string, bool) {
	e := strings.TrimSpace(entry)
	if host, port, err := net.SplitHostPort(e); err == nil {
		if loopbackHost(host) {
			return net.JoinHostPort(LoopbackAlias, port), true
		}
		return entry, false
	}
	if loopbackHost(strings.Trim(e, "[]")) {
		return LoopbackAlias, true
	}
	return entry, false
}
