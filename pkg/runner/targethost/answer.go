// Package targethost readies a run's target hosts before anything starts
// (agentry-notes plans/runtime-hosts.md §4, §6, D6–D8; agentry-cli#861): a
// target host is a machine the person names when a run starts, as the
// answer to an intake question of type host, and the runner reaches it
// over SSH from the host link (runnerimage/hostlink), never from the
// agent's containers. Here the answer is read, its address resolved and
// checked, its host key checked against the person's known_hosts and the
// runner's own, and its credential taken from a flag, the keychain or a
// prompt. Nothing here connects to the host but to read its key.
package targethost

import (
	"fmt"
	"net"
	"net/netip"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// DefaultPort is SSH's.
const DefaultPort = 22

// Answer is a host question's answer, [user@]host[:port] (an IPv6 address
// in brackets), as the person wrote it.
type Answer struct {
	User string `json:"user"`
	// Host is the name or the address as written, without brackets.
	Host string `json:"host"`
	Port int    `json:"port"`
}

var hostNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$`)

// localUser is the account an answer without one signs in as, as ssh's
// own default: this machine's user name. A variable for tests.
var localUser = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	name := u.Username
	if runtime.GOOS == "windows" {
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
	}
	return name
}

// ParseAnswer reads a host question's answer. The user defaults to this
// machine's user name and the port to 22; neither ever carries a
// credential, and an answer that holds a password (user:password@host) is
// refused, saying so.
func ParseAnswer(s string) (Answer, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Answer{}, fmt.Errorf("no host given: answer as [user@]host[:port]")
	}
	if strings.ContainsAny(s, " \t\r\n/\\?#") || strings.Contains(s, "://") {
		return Answer{}, fmt.Errorf("%q is not a host: answer as [user@]host[:port], as ssh takes it", s)
	}
	var a Answer
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		a.User, s = s[:i], s[i+1:]
		if strings.Contains(a.User, ":") {
			return Answer{}, fmt.Errorf("the answer holds a password: give the host as user@host, and the password when the run asks for it, or with --host-password")
		}
		if !hostlink.ValidUser(a.User) {
			return Answer{}, fmt.Errorf("%q is not an account name", a.User)
		}
	}
	host, port := s, ""
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return Answer{}, fmt.Errorf("%q: an IPv6 address in brackets has no closing bracket", s)
		}
		host, port = s[1:end], strings.TrimPrefix(s[end+1:], ":")
		if rest := s[end+1:]; rest != "" && !strings.HasPrefix(rest, ":") {
			return Answer{}, fmt.Errorf("%q: after the brackets, only :port", s)
		}
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() {
			return Answer{}, fmt.Errorf("%q is not an IPv6 address", host)
		}
	case strings.Count(s, ":") == 1:
		host, port, _ = strings.Cut(s, ":")
	case strings.Count(s, ":") > 1:
		return Answer{}, fmt.Errorf("%q: write an IPv6 address in brackets, as [2001:db8::1]:22", s)
	}
	if _, err := netip.ParseAddr(host); err != nil && (!hostNameRE.MatchString(host) || len(host) > 253) {
		return Answer{}, fmt.Errorf("%q is not a host name or an address", host)
	}
	a.Host = strings.TrimSuffix(host, ".")
	a.Port = DefaultPort
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return Answer{}, fmt.Errorf("%q is not a port", port)
		}
		a.Port = p
	}
	if a.User == "" {
		a.User = localUser()
		if !hostlink.ValidUser(a.User) {
			return Answer{}, fmt.Errorf("no account named, and this machine's user name %q is not one to sign in with: answer as user@host", a.User)
		}
	}
	return a, nil
}

// String is the answer as ssh would take it: user@host, :port when not
// 22, an IPv6 address in brackets.
func (a Answer) String() string {
	h := a.Host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if a.Port != DefaultPort {
		h += ":" + strconv.Itoa(a.Port)
	}
	return a.User + "@" + h
}

// KnownHostsName is the name the host's key is looked up and kept under
// in a known_hosts file, as OpenSSH writes it: the host as named, in
// brackets with its port when the port is not 22.
func (a Answer) KnownHostsName() string {
	return knownhosts.Normalize(net.JoinHostPort(a.Host, strconv.Itoa(a.Port)))
}

// Account is the system keychain account a target host's saved sign-in is
// kept under (service agentry): a name, never a secret.
func (a Answer) Account() string {
	return fmt.Sprintf("host:%s@%s:%d", a.User, strings.ToLower(a.Host), a.Port)
}
