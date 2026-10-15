// Package origin compares URLs by origin: scheme, host and port. A bearer
// credential is sent only to the origin that issued it, so two spellings
// of one origin must compare equal and nothing else may: not the same host
// over another scheme, where the credential would travel in clear, and
// not the same host on another port, which can be another service.
package origin

import (
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
