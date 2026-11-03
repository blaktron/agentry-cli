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

import "strings"

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
	// and nothing else.
	Hosts []string
	// MaxMessage is the most characters one message there holds.
	MaxMessage int
	// EndToEnd says the service encrypts a conversation end to end, so it
	// cannot read the messages it carries.
	EndToEnd bool
	// Account is how the consent names the account the agent uses there.
	Account string
}

// KindTelegram is Telegram, through a bot the person makes with BotFather.
const KindTelegram = "telegram"

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
	},
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
