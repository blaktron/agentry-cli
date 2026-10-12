package targethost

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// Host is a target host readied for a run: the person's answer, the
// address it resolved to, its checked key, and its sign-in.
type Host struct {
	ID         string
	Answer     Answer
	Address    netip.Addr
	HostKey    HostKey
	Credential Credential
}

// Options are what readying a target host reads besides its answer.
type Options struct {
	// Hosts is the policy's hosts lists.
	Hosts policy.HostsPolicy
	// KnownHosts are the files host keys are looked up in.
	KnownHosts KnownHostsFiles
	Flags      Flags
	Getenv     func(string) string
	// Ask is a person at a terminal; nil, no one is asked anything.
	Ask Asker
	// Say reports a warning or a note to the person.
	Say func(string)
}

// Prepare readies one target host. With a person at a terminal it asks for
// nothing a refusal would waste: the address (D8), the host key (D7), and
// only then the sign-in (D6). With no one to ask it takes the sign-in
// first: such a run has only its flags and its keychain, and one with
// neither (a hosted run, whose host has no keychain and passes no host
// flag) is refused before any name is resolved or any address dialled
// (agentry-cli#861, found live on dev; agentry-enterprise#534).
func Prepare(ctx context.Context, id string, a Answer, o Options) (*Host, error) {
	if o.Say == nil {
		o.Say = func(string) {}
	}
	if o.Getenv == nil {
		o.Getenv = func(string) string { return "" }
	}
	var cred Credential
	var err error
	if o.Ask == nil {
		if cred, err = ResolveCredential(id, a, o.Flags, o.Getenv, nil, o.Say); err != nil {
			return nil, err
		}
	}
	addr, err := ResolveAddress(ctx, id, a, o.Hosts)
	if err != nil {
		return nil, err
	}
	key, err := CheckHostKey(ctx, id, a, addr, o.KnownHosts, o.Ask, o.Say)
	if err != nil {
		return nil, err
	}
	if o.Ask != nil {
		if cred, err = ResolveCredential(id, a, o.Flags, o.Getenv, o.Ask, o.Say); err != nil {
			return nil, err
		}
	}
	return &Host{ID: id, Answer: a, Address: addr, HostKey: key, Credential: cred}, nil
}

// Target is what the host link is told of the host (target.json).
func (h *Host) Target() hostlink.Target {
	return hostlink.Target{
		ID: h.ID, Named: h.Answer.Host, Address: h.Address.String(), Port: h.Answer.Port, User: h.Answer.User,
		HostKeyAlgorithm: Algorithms(h.HostKey.Key), Auth: h.Credential.Auth,
		Passphrase: h.Credential.Passphrase != "", Sudo: h.Credential.Sudo != "",
	}
}

// RelayHost is the one address and port the host's relay may reach.
func (h *Host) RelayHost() string {
	return fmt.Sprintf("%s:%d", h.Address, h.Answer.Port)
}

// Document is the host's entry in intake.json: where the run reached and
// as whom, and the key it trusted, never a credential.
func (h *Host) Document() map[string]any {
	return map[string]any{"host": map[string]any{
		"named": h.Answer.Host, "address": h.Address.String(), "port": h.Answer.Port,
		"user": h.Answer.User, "hostKey": ssh256(h),
	}}
}

func ssh256(h *Host) string {
	if h.HostKey.Key == nil {
		return ""
	}
	return Fingerprint(h.HostKey.Key)
}

// ReceiptLine is the host's capabilities_enforced line (plan §8): who, the
// host as named and the address it resolved to, the port, the key and
// where its trust came from, the sign-in's kind and source, and what the
// run's boundary does not cover.
func (h *Host) ReceiptLine(calls, shells int) string {
	return h.receiptLine(calls, shells) + h.shortNote()
}

// shortNote says when a sign-in is too short to mask: the host link's
// scrub masks values of hostlink.MinScrub bytes and more, the run's files
// eight and more (D19).
func (h *Host) shortNote() string {
	for _, v := range h.Credential.Values() {
		if len(strings.TrimSpace(v)) < 8 {
			return fmt.Sprintf("; a sign-in shorter than 8 characters is not masked in the run's files (nor, under %d, in tool results)", hostlink.MinScrub)
		}
	}
	return ""
}

func (h *Host) receiptLine(calls, shells int) string {
	sessions := ""
	if shells > 0 {
		sessions = fmt.Sprintf("; %d shell sessions, their transcripts in receipts/%s", shells, strings.Replace(hostlink.ShellLogFile(h.ID, 1), "-1.log", "-<n>.log", 1))
	}
	return fmt.Sprintf("%s at %s (%s resolved by this machine), port %d; host key %s (%s); signed in with %s; reached only through the host link and one relay to that address and port, by the host tools (%d calls, receipts/%s%s); the account can run any command it may on that machine, and the machine's own network is outside the run",
		h.Answer.User, h.Answer.Host, h.Address, h.Answer.Port, ssh256(h), h.HostKey.From, h.Credential.Words(), calls, hostlink.LogFile(h.ID), sessions)
}
