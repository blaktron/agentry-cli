// Package messenger is the messenger role of the runner image: the one
// container that holds a messenger account's credential and its connection
// to that messaging service, for a persistent agent the person can talk to
// from there. It reaches the service's own hosts through an allowlisting
// relay and nothing else, and it talks to the agent's supervisor over its
// standard input and output, which carry messages, typing and delivery
// reports and nothing else: no command reaches the agent through it.
//
// Part of the embedded runner-image build context: stdlib-only, enforced
// by a test in the parent package.
package messenger

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind is one messaging service a persistent agent can be reached on: what
// the consent says of it, the hosts its role reaches, how long one message
// may be and how an answer is written there. One table, read by the
// consent, the relay and the role.
type Kind struct {
	// Name is the kind's key: "telegram", as a roster id says it
	// (messenger:telegram) and the runner policy's messengerKinds name it.
	Name string
	// Title is how a person is shown it.
	Title string
	// Hosts are the host:port pairs the role reaches, through its relay,
	// and nothing else. A kind whose account names its own server (NamesServer)
	// has none: its relay reaches that server alone (HostsFor).
	Hosts []string
	// NamesServer says each account is on a server the person names (a
	// Matrix homeserver), which is the one host its role reaches.
	NamesServer bool
	// MaxMessage is the most characters one message there holds.
	MaxMessage int
	// EndToEnd says the service encrypts a conversation end to end, so it
	// cannot read the messages it carries.
	EndToEnd bool
	// Account is how the consent names the account the agent uses there.
	Account string
	// Format is how an answer is written there.
	Format Format
	// Typing says the service lets the account show that it is typing.
	Typing bool
}

// The kinds: Telegram, through a bot the person makes with BotFather;
// Discord, through a bot of an application the person makes in Discord's
// developer portal; Slack, through an app the person makes in their
// workspace from the manifest Agentry gives (SlackManifest); and Matrix,
// through an account on any homeserver.
const (
	KindTelegram = "telegram"
	KindDiscord  = "discord"
	KindSlack    = "slack"
	KindMatrix   = "matrix"
)

// kinds are the messengers this build knows, in the order a person is shown
// them.
var kinds = []Kind{
	{
		Name: KindTelegram, Title: "Telegram",
		Hosts: []string{"api.telegram.org:443"},
		// Telegram's sendMessage takes at most 4096 characters of text
		// after its entities are parsed.
		MaxMessage: 4096,
		Account:    "bot",
		Format:     FormatHTML,
		Typing:     true,
	},
	{
		Name: KindDiscord, Title: "Discord",
		// The REST API and the gateway's WebSocket. The role always opens
		// the gateway at this address and never resumes at another, since
		// the relay reaches no other.
		Hosts: []string{"discord.com:443", "gateway.discord.gg:443"},
		// A message's content holds at most 2000 characters.
		MaxMessage: 2000,
		Account:    "bot",
		Format:     FormatMarkdown,
		Typing:     true,
	},
	{
		Name: KindSlack, Title: "Slack",
		// The Web API, and the two addresses Socket Mode's connections are
		// opened at.
		Hosts: []string{"slack.com:443", "wss-primary.slack.com:443", "wss-backup.slack.com:443"},
		// Slack takes far longer text, but asks for at most 4000
		// characters a message.
		MaxMessage: 4000,
		Account:    "app",
		Format:     FormatSlack,
	},
	{
		Name: KindMatrix, Title: "Matrix",
		NamesServer: true,
		// An event is at most 64 KiB, and a message carries its text twice
		// (HTML and plain), each escaped in JSON.
		MaxMessage: 8000,
		Account:    "account",
		Format:     FormatHTML,
		Typing:     true,
	},
}

// hostName is a DNS name: labels of letters, digits and hyphens, joined by
// dots.
var hostName = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// HostsFor are the host:port pairs an account of k reaches: the kind's own,
// or the server the account names.
func HostsFor(k Kind, server string) ([]string, error) {
	if !k.NamesServer {
		return k.Hosts, nil
	}
	h, err := ServerHost(server)
	if err != nil {
		return nil, err
	}
	return []string{h}, nil
}

// ServerHost is the host:port of an account's own server, given as an
// https:// address with no path, query or user: a Matrix homeserver's
// client address (https://matrix.example.org). The role's relay dials
// public addresses alone, so an address that is not one is refused; http://
// is taken for this computer alone, as a test's server is.
func ServerHost(server string) (string, error) {
	if server == "" {
		return "", errors.New("the account names no server: give its https:// address")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || strings.ContainsAny(server, "?#") || strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("%q is not a server's address: give it as https://host or https://host:port", server)
	}
	host, port := u.Hostname(), u.Port()
	// The host becomes the relay's allow list as it is: a name of letters,
	// digits, hyphens and dots, or an address, and nothing that list reads
	// as a separator.
	if net.ParseIP(host) == nil && !hostName.MatchString(host) {
		return "", fmt.Errorf("%q is not a server's address: its host is not a name", server)
	}
	if p, err := strconv.Atoi(port); port != "" && (err != nil || p < 1 || p > 65535) {
		return "", fmt.Errorf("%q is not a server's address: its port is not one", server)
	}
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			return "", fmt.Errorf("%q is not a public address: the messenger's container reaches public addresses alone, so a homeserver on your own network is not reached", server)
		}
	case "http":
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("%q is not https://: a server is reached over TLS", server)
		}
		if port == "" {
			port = "80"
		}
	default:
		return "", fmt.Errorf("%q is not https://: a server is reached over TLS", server)
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

// Names are the keys of the messengers this build knows.
func Names() []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.Name)
	}
	return out
}

// Lookup is the kind named name, case not mattering.
func Lookup(name string) (Kind, bool) {
	for _, k := range kinds {
		if strings.EqualFold(k.Name, name) {
			return k, true
		}
	}
	return Kind{}, false
}

// MaxParts is the most messages one answer is sent as; an answer longer
// than that many ends by saying the rest is in Agentry.
const MaxParts = 5

// RestInAgentry ends the last part of an answer cut at MaxParts.
const RestInAgentry = "… The rest of this answer is in Agentry."
