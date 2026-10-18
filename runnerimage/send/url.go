package send

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
)

// maxURLBytes bounds a destination's address.
const maxURLBytes = 2048

// Target is a destination's address checked: where the delivery goes and
// what the proxy in front of it must permit.
type Target struct {
	// URL is the address to post to, as given and parsed.
	URL *url.URL
	// Host is the host in lower case, Port its port (443 unless the person
	// named another on their own network), HostPort the pair as a proxy's
	// allowlist writes it.
	Host     string
	Port     string
	HostPort string
	// Plain says the address is http, which only a destination on the
	// person's own network may be.
	Plain bool
}

// CheckURL holds a destination's address to what a webhook may be. It is the
// one rule: the CLI applies it when a destination is saved and again before a
// delivery, and the delivery step applies it once more. https, a plain
// ASCII host name or an IPv4 address, no credentials in the address, no
// fragment, no space or control character; port 443 only, unless the
// destination was saved with "On my network" (onMyNetwork), which also admits
// http and any port. An address that names this machine's own loopback is
// refused, since in the delivery step that is the step's own container. A
// name that merely looks like a number (2130706433, 0x7f.1, 127.1) is
// refused: it is no host name, and some resolvers read it as an address. A
// literal address is judged here, public for an ordinary destination and
// public or on the person's own network for the other; a name is judged by
// the proxy, at the address it resolves to.
func CheckURL(raw string, onMyNetwork bool) (Target, error) {
	var t Target
	if raw == "" || len(raw) > maxURLBytes {
		return t, errors.New("the address is empty or longer than a destination's address can be")
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f || r > 0x7e {
			return t, errors.New("the address holds a space, a control character or a character outside ASCII (write an internationalized host name in its punycode form)")
		}
	}
	if strings.Contains(raw, "#") {
		return t, errors.New("the address carries a fragment, which is never sent")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return t, errors.New("the address is not a URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !onMyNetwork {
			return t, errors.New("the address is http: a destination must be https, unless it is on your own network")
		}
		t.Plain = true
	default:
		return t, errors.New("the address must begin with https://")
	}
	if u.User != nil {
		return t, errors.New("the address carries a user name or password: give an Authorization value instead")
	}
	if u.Opaque != "" || u.Host == "" {
		return t, errors.New("the address names no host")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return t, errors.New("the address names no host")
	}
	if strings.Contains(host, ":") || strings.Contains(u.Host, "[") {
		return t, errors.New("an IPv6 address is not taken: name the host")
	}
	if err := checkHost(host); err != nil {
		return t, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if t.Plain {
			port = "80"
		}
	}
	n, perr := strconv.Atoi(port)
	if perr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return t, errors.New("the address names a port that is not 1 to 65535")
	}
	if !onMyNetwork && port != "443" {
		return t, fmt.Errorf("the address names port %s: a destination is reached on port 443 alone, unless it is on your own network", port)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		switch {
		case a.IsLoopback() || a.IsUnspecified():
			return t, errors.New("the address is this machine's loopback, which in the delivery step is the step's own container: use this computer's address on your network")
		case onMyNetwork && !netaddr.Network(a):
			return t, fmt.Errorf("%s is not an address a delivery may reach (link-local space is never reached)", a)
		case !onMyNetwork && !netaddr.Public(a):
			return t, fmt.Errorf("%s is not a public address: tick On my network to deliver to one on your own network", a)
		}
	} else if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return t, errors.New("localhost is the delivery step's own container: use this computer's address or name on your network")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	t.URL, t.Host, t.Port = u, host, port
	t.HostPort = net.JoinHostPort(host, port)
	return t, nil
}

// checkHost holds a host to a DNS name or a canonical dotted IPv4 address.
func checkHost(host string) error {
	if len(host) > 253 || strings.HasSuffix(host, ".") {
		return errors.New("the host name is not one")
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("the host name is not one")
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return errors.New("the host name holds a character a host name does not")
			}
		}
	}
	if canonicalIPv4(labels) {
		return nil
	}
	last := labels[len(labels)-1]
	if !strings.ContainsAny(last, "abcdefghijklmnopqrstuvwxyz") || strings.HasPrefix(last, "0x") {
		return errors.New("the host is neither a name nor a dotted IPv4 address (a number written another way is refused)")
	}
	return nil
}

// canonicalIPv4 is true for four decimal labels, each 0 to 255 with no
// leading zero.
func canonicalIPv4(labels []string) bool {
	if len(labels) != 4 {
		return false
	}
	for _, l := range labels {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != l {
			return false
		}
	}
	return true
}
