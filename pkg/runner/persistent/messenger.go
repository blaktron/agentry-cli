package persistent

// A persistent agent's messengers: a messaging service (Telegram, Discord,
// Slack, Matrix) the person adds to the agent so they can talk to it from
// there, each a roster entry messenger:<kind> of the person's own, never one
// a release declares. The account's credential (a bot's token, a Slack
// app's two tokens, a Matrix account's access token) is kept in the system
// keychain under messenger:<agent>:<kind>; config.yaml records the account's
// id and name on the service, its own server where it has one, and the
// person paired with it.
//
// Only the paired person reaches the agent. Pairing binds the person's own
// account: Agentry shows a code, valid for a few minutes, and the person
// sends it to the account from theirs; the supervisor alone holds the code's
// digest and judges it. Every other sender is ignored, never answered.
//
// The connection is held by the runner image's messenger role, which the
// engine starts beside the agent (engine.StartMessenger) and the supervisor
// keeps for as long as the agent runs, across its segments: switching a
// messenger on or off starts or stops its role and restarts nothing else.
// A message from the paired person becomes a turn of the agent's one
// conversation, framed as coming from the messenger, and the answer goes
// back there; a message that arrives during a turn waits, and the ones
// waiting when it ends go together as the next turn. "/stop" stops the turn
// in flight. Nothing the agent asks the person's agreement to is answered
// from a messenger: it waits in Agentry, and the messenger is told so.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

// KindMessenger is a messenger's roster entry.
const KindMessenger EntryKind = "messenger"

// messengerPrefix begins a messenger's roster id.
const messengerPrefix = "messenger:"

// messengersVersion is the version of config.yaml that added Messengers,
// and messengerKindsVersion the one that added every kind but Telegram and
// an account's own server: a build that knows only Telegram would drop the
// server at its next save, so it refuses the config instead.
const (
	messengersVersion     = 12
	messengerKindsVersion = 13
)

// MessengerAccount is one messenger account the person added to the agent,
// as config.yaml keeps it: never its credential.
type MessengerAccount struct {
	// Kind names the messenger (messenger.KindTelegram).
	Kind string `yaml:"kind" json:"kind"`
	// Account is the account's id on the service (a Telegram bot's id), by
	// which one account is kept to one agent; Name is its name there
	// (@helper_bot).
	Account string `yaml:"account" json:"account"`
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`
	// Server is the account's own server, for a kind whose accounts name
	// one: a Matrix homeserver's https:// address.
	Server string `yaml:"server,omitempty" json:"server,omitempty"`
	// Set is when its credential was last set, so setting it again starts
	// its role again.
	Set time.Time `yaml:"set" json:"set"`
	// Peer is the person paired with the agent there, if any.
	Peer *MessengerPeer `yaml:"peer,omitempty" json:"peer,omitempty"`
	// Pairing is the pairing open now, if any.
	Pairing *MessengerPairing `yaml:"pairing,omitempty" json:"pairing,omitempty"`
}

// MessengerPeer is the person's own account on the messenger: the sender
// whose messages reach the agent, and the chat its answers go to.
type MessengerPeer struct {
	Sender string    `yaml:"sender" json:"sender"`
	Chat   string    `yaml:"chat" json:"chat"`
	Paired time.Time `yaml:"paired" json:"paired"`
}

// MessengerPairing is an open pairing: the digest of the code Agentry showed
// and when it stops being taken.
type MessengerPairing struct {
	CodeDigest string    `yaml:"codeDigest" json:"-"`
	Until      time.Time `yaml:"until" json:"until"`
	// Tries counts the codes it refused; at MaxPairingTries it closes.
	Tries int `yaml:"tries,omitempty" json:"-"`
}

// MaxPairingTries is how many wrong codes close a pairing: a guess has at
// most that many chances at a code of about 40 bits.
const MaxPairingTries = 5

// PairingChange opens a pairing over the owner socket: the messenger's kind,
// the digest of the code the client showed and the pairing's end. The code
// itself never leaves the client that shows it.
type PairingChange struct {
	Kind       string    `json:"kind"`
	CodeDigest string    `json:"codeDigest"`
	Until      time.Time `json:"until"`
}

// PairingChangeFor is the change that opens a pairing p on kind.
func PairingChangeFor(kind string, p MessengerPairing) Change {
	return Change{OpenPairing: &PairingChange{Kind: kind, CodeDigest: p.CodeDigest, Until: p.Until}}
}

// PairingValidFor is how long a pairing code is taken.
const PairingValidFor = 10 * time.Minute

// MessengerEntryID is a messenger's roster id.
func MessengerEntryID(kind string) string { return messengerPrefix + kind }

// isMessengerEntry says id is a messenger's roster id.
func isMessengerEntry(id string) bool { return strings.HasPrefix(id, messengerPrefix) }

// MessengerKeychainAccount is the keychain account a messenger account's
// credential is kept under.
func MessengerKeychainAccount(agent, kind string) string {
	return "messenger:" + agent + ":" + kind
}

// TakesMessengers says the agent can be reached on a messenger: every
// persistent agent but the one that runs only in Agentry Desktop's window,
// whose roster is fixed.
func (a Agent) TakesMessengers() bool { return !a.DesktopOnly }

// MessengerOf is kind's account in c, if c has one.
func MessengerOf(c Config, kind string) (MessengerAccount, bool) {
	if i := findMessenger(c.Messengers, kind); i >= 0 {
		return c.Messengers[i], true
	}
	return MessengerAccount{}, false
}

// findMessenger is the index of kind's account in accounts, or -1.
func findMessenger(accounts []MessengerAccount, kind string) int {
	return slices.IndexFunc(accounts, func(m MessengerAccount) bool { return m.Kind == kind })
}

// messengerEntries are the agent's messengers' entries.
func messengerEntries(c Config) []RosterEntry {
	out := make([]RosterEntry, 0, len(c.Messengers))
	for _, m := range c.Messengers {
		k, ok := messenger.Lookup(m.Kind)
		if !ok {
			continue
		}
		name := k.Title
		if m.Name != "" {
			name += " (" + m.Name + ")"
		}
		out = append(out, RosterEntry{ID: MessengerEntryID(m.Kind), Kind: KindMessenger, Name: name, GrantedBy: GrantedByYou, Line: messengerLine(k, m)})
	}
	return out
}

// messengerLine is a messenger's consent sentence: who carries the
// messages, whether they can read them, who reaches the agent there, and
// where its credential is.
func messengerLine(k messenger.Kind, m MessengerAccount) string {
	account := "your " + k.Account
	if m.Name != "" {
		account += " " + m.Name
	}
	carrier, reader := k.Title, k.Title
	hosts, _ := messenger.HostsFor(k, m.Server)
	if k.NamesServer {
		carrier, reader = "your homeserver, "+strings.Join(hostNames(hosts), ", ")+",", "it"
	}
	carried := carrier + " carries them and does not encrypt them end to end, so " + reader + " can read what you and the agent write there"
	if k.EndToEnd {
		carried = carrier + " carries them, encrypted end to end"
	}
	return "Takes your messages on " + k.Title + ", through " + account + ", as turns of its conversation, and answers you there: " + carried +
		". Only your own " + k.Title + " account reaches it, once you pair it with a code Agentry shows; anyone else who writes to it is ignored, never answered. " +
		"Nothing is approved from " + k.Title + ": what the agent asks your agreement to waits for you in Agentry. " +
		"The " + k.Account + "'s credential stays in " + KeptIn + ", and Agentry hands it to one container alone, of its own, which reaches " + strings.Join(hostNames(hosts), ", ") + " and nothing else; the agent is never given it"
}

// hostNames are host:port pairs without their ports.
func hostNames(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		host, _, err := net.SplitHostPort(h)
		if err != nil {
			host = h
		}
		out = append(out, host)
	}
	return out
}

// addMessenger records a messenger account in c, on: a new one, or one whose
// credential was set again, which keeps its pairing when the account is the
// same and drops it when it is another.
func addMessenger(a Agent, c *Config, m MessengerAccount) error {
	if !a.TakesMessengers() {
		return fmt.Errorf("%s is reached in Agentry Desktop's window alone: it takes no messenger", a.Title)
	}
	k, ok := messenger.Lookup(m.Kind)
	if !ok {
		return fmt.Errorf("%q is not a messenger this build knows (%s)", m.Kind, strings.Join(messenger.Names(), ", "))
	}
	if k.NamesServer {
		if _, err := messenger.ServerHost(m.Server); err != nil {
			return err
		}
		if !strings.HasPrefix(m.Server, "https://") {
			return fmt.Errorf("%q is not https://: a homeserver is reached over TLS", m.Server)
		}
	} else {
		m.Server = ""
	}
	if m.Account == "" {
		return errors.New("a messenger account needs its id on the service")
	}
	next := slices.Clone(c.Messengers)
	m.Peer, m.Pairing = nil, nil
	if i := findMessenger(next, m.Kind); i >= 0 {
		if next[i].Account == m.Account {
			m.Peer = next[i].Peer
		}
		next[i] = m
	} else {
		next = append(next, m)
	}
	c.Messengers = next
	delete(c.Switches, MessengerEntryID(m.Kind))
	return nil
}

// removeMessenger takes a messenger out of c, by kind.
func removeMessenger(a Agent, c *Config, kind string) error {
	i := findMessenger(c.Messengers, kind)
	if i < 0 {
		return fmt.Errorf("the %s agent has no %s messenger", a.Name, kind)
	}
	c.Messengers = slices.Delete(slices.Clone(c.Messengers), i, i+1)
	delete(c.Switches, MessengerEntryID(kind))
	delete(c.Consented, MessengerEntryID(kind))
	return nil
}

// openPairing records an open pairing for kind's account in c.
func openPairing(a Agent, c *Config, kind string, p MessengerPairing) error {
	i := findMessenger(c.Messengers, kind)
	if i < 0 {
		return fmt.Errorf("the %s agent has no %s messenger: add it first (agentry agent messenger add %s %s)", a.Name, kind, a.Name, kind)
	}
	if len(p.CodeDigest) != sha256.Size*2 || p.Until.IsZero() {
		return errors.New("a pairing needs its code's digest and its end")
	}
	// No pairing outlives the time a code is valid, whatever the client asked.
	if latest := time.Now().Add(PairingValidFor + time.Minute).UTC(); p.Until.After(latest) {
		p.Until = latest
	}
	p.Tries = 0
	next := slices.Clone(c.Messengers)
	next[i].Pairing = &p
	c.Messengers = next
	return nil
}

// NewPairingCode is a fresh pairing code, as Agentry shows it, and the
// pairing that takes it for PairingValidFor from now.
func NewPairingCode(now time.Time) (string, MessengerPairing, error) {
	b := make([]byte, messenger.CodeLength)
	if _, err := rand.Read(b); err != nil {
		return "", MessengerPairing{}, err
	}
	code := make([]byte, messenger.CodeLength)
	for i, v := range b {
		// 256 is not a multiple of the alphabet's 31 letters: the bias is
		// under one in eight per character, of no use to a guess, which has
		// MaxPairingTries chances before the pairing closes.
		code[i] = messenger.CodeAlphabet[int(v)%len(messenger.CodeAlphabet)]
	}
	return string(code), MessengerPairing{CodeDigest: codeDigest(string(code)), Until: now.Add(PairingValidFor).UTC()}, nil
}

// codeDigest is a pairing code's digest, as config.yaml keeps it.
func codeDigest(code string) string {
	sum := sha256.Sum256([]byte("agentry-messenger-pairing:" + code))
	return hex.EncodeToString(sum[:])
}

// takes says the pairing takes code at now.
func (p *MessengerPairing) takes(code string, now time.Time) bool {
	if p == nil || now.After(p.Until) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(codeDigest(code)), []byte(p.CodeDigest)) == 1
}

// FormatPairingCode is a code as a person reads it: two groups of four.
func FormatPairingCode(code string) string {
	if len(code) != messenger.CodeLength {
		return code
	}
	return code[:4] + " " + code[4:]
}

// MessengerAccountOf finds which agent of the runner home holds a messenger
// account: the name of the agent whose config has kind's account with that
// id, or "".
func MessengerAccountOf(home, kind, account string) string {
	for _, a := range AgentsIn(home) {
		c, err := LoadConfig(home, a.Name)
		if err != nil {
			continue
		}
		if i := findMessenger(c.Messengers, kind); i >= 0 && c.Messengers[i].Account == account {
			return a.Name
		}
	}
	return ""
}

// MessengerState is one messenger as a client shows it: never its
// credential.
type MessengerState struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Account string `json:"account,omitempty"`
	Name    string `json:"name,omitempty"`
	// Server is the account's own server, where its kind has one.
	Server string `json:"server,omitempty"`
	On     bool   `json:"on"`
	// Paired says a person is paired; Pairing says a code is taken until
	// PairingUntil.
	Paired       bool      `json:"paired"`
	PairedAt     time.Time `json:"pairedAt,omitzero"`
	Pairing      bool      `json:"pairing,omitempty"`
	PairingUntil time.Time `json:"pairingUntil,omitzero"`
	// Connection is the role's state while the agent runs: connecting,
	// connected, or why it is not; empty while the agent is stopped.
	Connection string `json:"connection,omitempty"`
	// Refused is why the runner policy does not let it run.
	Refused string `json:"refused,omitempty"`
	Entry   string `json:"entry"`
}

// The role's connection states.
const (
	MessengerConnecting = "connecting"
	MessengerConnected  = "connected"
)

// Messengers are the agent's messengers as a client shows them, from its
// config; a running agent's supervisor adds each one's connection
// (Supervisor.Status).
func Messengers(a Agent, c Config, allowed func(kind string) (bool, string)) []MessengerState {
	r := BuildRoster(a, c, nil)
	out := make([]MessengerState, 0, len(c.Messengers))
	now := time.Now()
	for _, m := range c.Messengers {
		k, ok := messenger.Lookup(m.Kind)
		if !ok {
			continue
		}
		e, _ := r.Entry(MessengerEntryID(m.Kind))
		st := MessengerState{Kind: m.Kind, Title: k.Title, Account: m.Account, Name: m.Name, Server: m.Server, On: e.On, Entry: e.ID}
		if m.Peer != nil {
			st.Paired, st.PairedAt = true, m.Peer.Paired
		}
		if m.Pairing != nil && now.Before(m.Pairing.Until) {
			st.Pairing, st.PairingUntil = true, m.Pairing.Until
		}
		if allowed != nil {
			if ok, why := allowed(m.Kind); !ok {
				st.Refused = why
			}
		}
		out = append(out, st)
	}
	return out
}
