package persistent

// The agent's mail providers: the services whose mail and calendar it
// reaches, each through its own image servers (offer.MailProvider). The
// person enables the ones they use, so the roster shows only theirs. A
// disabled provider's servers are off the roster and out of every segment;
// their sign-ins, keychain entries and volumes are kept, so enabling the
// provider again asks for no new set-up.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
)

// mailProvidersOf are the agent's mail providers: its built-in's, but in a
// test.
var mailProvidersOf = func(a Agent) []offer.MailProvider { return a.Builtin.MailProviders() }

// mailProviderOf is the provider whose image server key is, if any.
func mailProviderOf(a Agent, key string) (offer.MailProvider, bool) {
	for _, p := range mailProvidersOf(a) {
		if slices.Contains(p.ImageServers, key) {
			return p, true
		}
	}
	return offer.MailProvider{}, false
}

// imageServerShown says the image server key is on the agent's roster: it
// is no provider's, or its provider is enabled.
func imageServerShown(a Agent, c Config, key string) bool {
	p, ok := mailProviderOf(a, key)
	return !ok || slices.Contains(c.MailProviders, p.Key)
}

// migrateMailProviders enables, in a configuration from before mail
// providers, each provider one of whose servers has a sign-in or a client:
// what the person set up stays on their roster. One with neither stays
// disabled, as a new agent's providers are.
func migrateMailProviders(a Agent, c *Config) {
	for _, p := range mailProvidersOf(a) {
		for _, k := range p.ImageServers {
			if _, set := c.SignIns[k]; set && !slices.Contains(c.MailProviders, p.Key) {
				c.MailProviders = append(c.MailProviders, p.Key)
			}
		}
	}
}

// MailProviderState is one of the agent's mail providers as a client shows
// it: whether it is enabled, the roster ids of its servers, the account each
// signed-in server answers as, and what to do to set up one that is not.
type MailProviderState struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Servers are its servers' roster ids (server:<key>), which are on the
	// roster only while it is enabled.
	Servers []string `json:"servers"`
	// Accounts are the accounts its signed-in servers answer as.
	Accounts []string `json:"accounts,omitempty"`
	// SetUp is what each server not yet signed in needs, as its roster entry
	// says it.
	SetUp []string `json:"setUp,omitempty"`
}

// MailProviders are the agent's mail providers, in its built-in's order;
// none for an agent that reaches no mail.
func MailProviders(a Agent, c Config) []MailProviderState {
	var out []MailProviderState
	for _, p := range mailProvidersOf(a) {
		st := MailProviderState{Key: p.Key, Name: p.Name, Enabled: slices.Contains(c.MailProviders, p.Key)}
		for _, k := range p.ImageServers {
			st.Servers = append(st.Servers, ImageServerEntryID(k))
			if in := c.SignIns[k]; in.signedIn() {
				st.Accounts = append(st.Accounts, in.Account)
			} else if s, ok := imageServer(a, k); ok && s.SignIn != "" {
				st.SetUp = append(st.SetUp, s.SignIn)
			}
		}
		for _, o := range providerOffers(a, c) {
			if o.MailProvider != p.Key {
				continue
			}
			st.Servers = append(st.Servers, ServerEntryID(o.Server))
			if ok, reason := setUp(a, c, o); !ok {
				st.SetUp = append(st.SetUp, reason)
			}
		}
		out = append(out, st)
	}
	return out
}

// hiddenProvider is the disabled mail provider whose server or switch has
// the roster id id, if any: an image server's, or an offered server's.
func hiddenProvider(a Agent, c Config, id string) (offer.MailProvider, bool) {
	if !strings.HasPrefix(id, serverPrefix) {
		return offer.MailProvider{}, false
	}
	key, _, _ := strings.Cut(strings.TrimPrefix(id, serverPrefix), ":")
	if p, ok := mailProviderOf(a, key); ok && !slices.Contains(c.MailProviders, p.Key) {
		return p, true
	}
	for _, o := range providerOffers(a, c) {
		if (id == ServerEntryID(o.Server) || id == ServerWritesID(o.Server)) && !slices.Contains(c.MailProviders, o.MailProvider) {
			return mailProviderByKey(a, o.MailProvider)
		}
	}
	return offer.MailProvider{}, false
}

// providerOffers are the built-in's offered servers that are a mail
// provider's, whether or not it is enabled.
func providerOffers(a Agent, c Config) []offer.Server {
	all := c
	all.MailProviders = nil
	for _, p := range mailProvidersOf(a) {
		all.MailProviders = append(all.MailProviders, p.Key)
	}
	var out []offer.Server
	for _, o := range offersOf(a, all) {
		if o.MailProvider != "" {
			out = append(out, o)
		}
	}
	return out
}

// mailProviderByKey is the agent's mail provider key.
func mailProviderByKey(a Agent, key string) (offer.MailProvider, bool) {
	for _, p := range mailProvidersOf(a) {
		if p.Key == key {
			return p, true
		}
	}
	return offer.MailProvider{}, false
}

// mailProviderKeys names the agent's mail providers, for a refusal.
func mailProviderKeys(a Agent) string {
	var keys []string
	for _, p := range mailProvidersOf(a) {
		keys = append(keys, p.Key)
	}
	return strings.Join(keys, ", ")
}

// enableMailProvider enables the provider key in c, and says whether it
// was disabled.
func enableMailProvider(c *Config, key string) bool {
	if slices.Contains(c.MailProviders, key) {
		return false
	}
	c.MailProviders = append(c.MailProviders, key)
	return true
}

// SetMailProvider enables or disables one of a stopped agent's mail
// providers. Disabling keeps the provider's sign-in, its keychain entries
// and its volume, and its switches and agreements as they were; enabling
// it again puts its servers back on the roster as they were left. It is
// refused while the agent runs, whose segment holds the servers' volumes,
// and holds the agent's lock meanwhile so none starts.
func SetMailProvider(home string, a Agent, client, key string, on bool) error {
	if len(mailProvidersOf(a)) == 0 {
		return fmt.Errorf("the %s agent reaches no mail", a.Name)
	}
	if !slices.ContainsFunc(mailProvidersOf(a), func(p offer.MailProvider) bool { return p.Key == key }) {
		return fmt.Errorf("the %s agent has no mail provider %q (its providers are %s)", a.Name, key, mailProviderKeys(a))
	}
	owner, err := Acquire(home, a.Name, client)
	var held *HeldError
	if errors.As(err, &held) {
		return fmt.Errorf("%w: stop it first, since its servers hold their sign-ins while it runs", held)
	}
	if err != nil {
		return err
	}
	defer owner.Release()
	c, err := LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	switch {
	case on && !enableMailProvider(&c, key):
		return nil
	case !on && !slices.Contains(c.MailProviders, key):
		return nil
	case !on:
		c.MailProviders = slices.DeleteFunc(c.MailProviders, func(k string) bool { return k == key })
	}
	return SaveConfig(home, a.Name, c)
}
