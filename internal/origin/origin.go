// Package origin compares URLs by origin: scheme, host and port. A bearer
// credential is sent only to the origin that issued it, so two spellings
// of one origin must compare equal and nothing else may: not the same host
// over another scheme, where the credential would travel in clear, and
// not the same host on another port, which can be another service.
package origin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Host is host as a request resolves it: lower-cased, without trailing
// dots, and a non-ASCII name in its ASCII (punycode) form, so that two
// spellings of one host compare equal. ok is false for a name that has no
// such form.
func Host(host string) (string, bool) {
	h := strings.TrimRight(strings.ToLower(host), ".")
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			a, err := idna.Lookup.ToASCII(h)
			if err != nil {
				return "", false
			}
			return strings.TrimRight(a, "."), true
		}
	}
	return h, true
}

// Of is rawURL's scheme, host and port, lower-cased, with the scheme's
// default port dropped; "" for a URL with no host or a host with no ASCII
// form.
func Of(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host, ok := Host(u.Hostname())
	if !ok {
		return ""
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// Same reports whether a and b share an origin. A URL with no origin
// shares it with nothing.
func Same(a, b string) bool {
	oa := Of(a)
	return oa != "" && oa == Of(b)
}

// maxRedirects is the number of redirects net/http follows when a client
// sets no policy of its own.
const maxRedirects = 10

// Guard returns a copy of c that never carries an Authorization header
// across origins. net/http keeps that header on a redirect to the same
// host, or to a subdomain of it, whatever the scheme or the port, so a
// registry's answer pointing at http:// on its own host, at another port
// or at a sibling subdomain would take a bearer token with it. Guard
// compares every hop with the origin of the request the caller sent, so a
// hop back to that origin gets the header again and one away from it does
// not. A redirect policy c already has still decides whether the redirect
// is followed; with none, the default limit of ten applies. A nil c is
// http.DefaultClient.
func Guard(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	g := *c
	decide := c.CheckRedirect
	g.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && !Same(req.URL.String(), via[0].URL.String()) {
			req.Header.Del("Authorization")
		}
		if decide != nil {
			return decide(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &g
}
