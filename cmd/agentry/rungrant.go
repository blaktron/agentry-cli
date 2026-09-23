package main

import (
	"net/http"
	"os"
	"sync"
)

// A run-scoped registry grant (agentry-cli#232, agentry-enterprise#92). A
// hosted run of a private release reaches it through a prefix of the
// registry that answers for that one run: the driver points --registry at
// the prefix and hands the run's grant in AGENTRY_RUN_GRANT, and every
// request to the registry's host carries it in its own header. The grant is
// never in a URL, so no access log, receipt or pin file name holds it; it
// never goes to another host (a CDN, npm); and nothing the CLI prints
// includes a request's headers.

const (
	envRunGrant    = "AGENTRY_RUN_GRANT"
	runGrantHeader = "X-Agentry-Run-Grant"
)

// installRunGrant wraps the default transport, which every client the CLI
// builds falls through to, when a grant is set; it is a no-op otherwise.
func installRunGrant() {
	grant := os.Getenv(envRunGrant)
	if grant == "" {
		return
	}
	http.DefaultTransport = &grantTransport{next: http.DefaultTransport, grant: grant, registry: grantHost}
}

// grantHost is the --registry host, read once the flags are parsed.
var grantHost = sync.OnceValue(func() string {
	host, _ := registryHost(registryURL())
	return host
})

// grantTransport adds the grant to requests bound for the registry's host.
type grantTransport struct {
	next     http.RoundTripper
	grant    string
	registry func() string
}

func (t *grantTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if host := t.registry(); host != "" && req.URL.Host == host {
		req = req.Clone(req.Context())
		req.Header.Set(runGrantHeader, t.grant)
	}
	return t.next.RoundTrip(req)
}
