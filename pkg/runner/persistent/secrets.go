package persistent

// A persistent agent's stored secrets: values the person keeps in the system
// keychain for one agent, each a roster entry named by the environment
// variable it is given as. config.yaml holds the name and when the value was
// last set, never the value. A segment is given the values its roster has on:
// a server the person added that declares a secret of that name is given it,
// and the Coding agent's programs read it in their environment. One that is
// off is withheld: a server that declares it is given an empty value, never
// one from the runner policy's secret source. The values reach the segment's
// secrets file alone, so the engine's redactor masks each in the receipts,
// the transcript and the events, and the tools server masks it in its own
// tool results.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// KindSecret is a stored secret's roster entry.
const KindSecret EntryKind = "secret"

// secretPrefix begins a stored secret's roster id.
const secretPrefix = "secret:"

// The bounds of an agent's stored secrets. A value is at least
// MinSecretBytes so the engine's redactor, which leaves shorter values
// alone, masks it wherever it would appear.
const (
	MaxSecrets     = 32
	MinSecretBytes = 8
	MaxSecretBytes = 16 << 10
)

// StoredSecret is a secret the person stored for the agent: its name, and
// when its value was last set, so setting it again reaches the next turn.
type StoredSecret struct {
	Name string    `yaml:"name" json:"name"`
	Set  time.Time `yaml:"set" json:"set"`
}

// SecretEntryID is a stored secret's roster id.
func SecretEntryID(name string) string { return secretPrefix + name }

// SecretAccount is the keychain account a stored secret's value is kept
// under, apart from every other agent's and from the person's other keys.
func SecretAccount(agent, name string) string { return "agent-secret:" + agent + ":" + name }

// CheckSecretName refuses a name that is not an environment variable's
// ([A-Z][A-Z0-9_]{0,63}), or one the runner and the programs it runs set
// themselves (spec.CheckStoredSecretName).
func CheckSecretName(name string) error { return spec.CheckStoredSecretName(name) }

// CheckSecretValue refuses a value the segment cannot carry or mask: one
// shorter than MinSecretBytes or longer than MaxSecretBytes, one that is not
// UTF-8 text, and one holding a line break or another control character,
// which the gateway's secrets file cannot hold.
func CheckSecretValue(v string) error {
	switch {
	case len(v) < MinSecretBytes:
		return fmt.Errorf("a secret's value is at least %d characters, so it can be masked wherever it would appear", MinSecretBytes)
	case len(v) > MaxSecretBytes:
		return fmt.Errorf("a secret's value is at most %d KiB", MaxSecretBytes>>10)
	case !utf8.ValidString(v):
		return errors.New("a secret's value must be UTF-8 text")
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return errors.New("a secret's value is one line, with no control character")
		}
	}
	return nil
}

// Secrets is where stored secrets' values are kept.
type Secrets interface {
	Get(account string) (string, error)
	Set(account, value string) error
	Delete(account string) error
}

// KeychainSecrets is the system keychain under Agentry's service. A start
// with a sealed credential (a headless start a client prepared) reads from
// it alone, as the model key is read.
type KeychainSecrets struct{}

// Get implements Secrets.
func (KeychainSecrets) Get(account string) (string, error) {
	return modelkey.LookupAccount(account)
}

// Set implements Secrets.
func (KeychainSecrets) Set(account, value string) error {
	if modelkey.Disabled() {
		return fmt.Errorf("the system keychain is turned off here (%s), and a stored secret is kept there alone", modelkey.EnvNoKeyring)
	}
	if err := keyring.Set(modelkey.Service, account, value); err != nil {
		return fmt.Errorf("keep the secret in the system keychain: %w", err)
	}
	return nil
}

// Delete implements Secrets. A missing entry is not an error.
func (KeychainSecrets) Delete(account string) error {
	if modelkey.Disabled() {
		return nil
	}
	if err := keyring.Delete(modelkey.Service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the secret from the system keychain: %w", err)
	}
	return nil
}

// secretEntries are the agent's stored secrets' entries.
func secretEntries(a Agent, c Config) []RosterEntry {
	out := make([]RosterEntry, 0, len(c.Secrets))
	for _, s := range c.Secrets {
		out = append(out, RosterEntry{ID: SecretEntryID(s.Name), Kind: KindSecret, Name: "Your secret " + s.Name, GrantedBy: GrantedByYou, Line: secretLine(a, c, s.Name)})
	}
	return out
}

// secretLine is a stored secret's consent sentence: who is given it. For
// the Coding agent, the programs it runs, which can read it and send it
// anywhere they reach; for either agent, each server the person added that
// declares it.
func secretLine(a Agent, c Config, name string) string {
	var servers []string
	for _, as := range agentServers(a, c) {
		if slices.Contains(as.Server.Secrets, name) {
			servers = append(servers, serverName(as.Server))
		}
	}
	line := "Gives your secret " + name
	switch {
	case a.Builtin.Codes():
		line += " to the programs it runs, in their environment, which can read it and send it anywhere they reach"
		if len(servers) > 0 {
			line += ", and to the MCP server " + strings.Join(servers, ", ")
		}
	case len(servers) > 0:
		line += " to the MCP server " + strings.Join(servers, ", ")
	default:
		line += " to an MCP server you add that declares it; no server it has declares it now"
	}
	return line + "; its value stays in your keychain, and Agentry masks it wherever it shows it whole"
}

// addSecret records a stored secret in c, on: a new one, or one whose
// value was set again. The value is kept in the keychain first.
func addSecret(a Agent, c *Config, s StoredSecret) error {
	if !a.Builtin.KeepsSecrets() {
		return fmt.Errorf("the %s agent keeps no secrets", a.Name)
	}
	if err := CheckSecretName(s.Name); err != nil {
		return err
	}
	if i := slices.IndexFunc(c.Secrets, func(x StoredSecret) bool { return x.Name == s.Name }); i >= 0 {
		c.Secrets = slices.Clone(c.Secrets)
		c.Secrets[i] = s
		return nil
	}
	if len(c.Secrets) >= MaxSecrets {
		return fmt.Errorf("the %s agent keeps at most %d secrets", a.Name, MaxSecrets)
	}
	c.Secrets = append(slices.Clone(c.Secrets), s)
	delete(c.Switches, SecretEntryID(s.Name))
	return nil
}

// removeSecret takes a stored secret out of c, by name.
func removeSecret(a Agent, c *Config, name string) error {
	i := slices.IndexFunc(c.Secrets, func(x StoredSecret) bool { return x.Name == name })
	if i < 0 {
		return fmt.Errorf("the %s agent has no secret %s", a.Name, name)
	}
	c.Secrets = slices.Delete(slices.Clone(c.Secrets), i, i+1)
	delete(c.Switches, SecretEntryID(name))
	delete(c.Consented, SecretEntryID(name))
	return nil
}

// secretGrants are the names of the stored secrets the roster has on, which
// the segment is given, and of those it has off, which are withheld.
func secretGrants(c Config, on func(id string) bool) (given, withheld []string) {
	for _, s := range c.Secrets {
		if on(SecretEntryID(s.Name)) {
			given = append(given, s.Name)
		} else {
			withheld = append(withheld, s.Name)
		}
	}
	return given, withheld
}

// ReadSecrets reads the values of the named secrets of the agent from
// keys: every one must be there.
func ReadSecrets(keys Secrets, agent string, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		if err := CheckSecretName(n); err != nil {
			return nil, fmt.Errorf("the %s agent's secret %s: %w (remove it: agentry agent secret remove %s %s)", agent, n, err, agent, n)
		}
		v, err := keys.Get(SecretAccount(agent, n))
		if err != nil {
			return nil, fmt.Errorf("the %s agent's secret %s: %w (set it again: agentry agent secret set %s %s)", agent, n, err, agent, n)
		}
		if err := CheckSecretValue(v); err != nil {
			return nil, fmt.Errorf("the %s agent's secret %s in the keychain: %w (set it again: agentry agent secret set %s %s)", agent, n, err, agent, n)
		}
		out[n] = v
	}
	return out, nil
}
