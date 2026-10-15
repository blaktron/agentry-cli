package targethost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// Credentials. A target host is signed in to with a key file (and its
// passphrase, when it has one) or a password, and sudo may be given its own
// password. Each is taken, in order, from a flag, from the person's saved
// sign-in for that account and host in the system keychain, and from a prompt
// at a terminal that offers to save it. A flag names a file or an environment
// variable, never a value. A run with no one to ask and no sign-in refuses. The
// values reach the host link alone, as files bound into it (the engine's), and
// are masked wherever the run writes.

// Where a credential came from, as the receipt says it.
const (
	FromFlag     = "flag"
	FromKeychain = "keychain"
	FromPrompt   = "prompt"
)

// Credential is a target host's sign-in.
type Credential struct {
	// Auth is hostlink.AuthKey or hostlink.AuthPassword.
	Auth string
	// KeyFile is the key's path on this machine, for the receipt; Key is
	// its bytes, as the file holds them.
	KeyFile    string
	Key        []byte
	Passphrase string
	Password   string
	// Sudo is the password sudo is fed, "" for none.
	Sudo string
	// From and SudoFrom say where each came from (From*).
	From, SudoFrom string
}

// Values are the credential's secrets, for the run's redactor.
func (c Credential) Values() []string {
	var out []string
	for _, v := range []string{string(c.Key), c.Passphrase, c.Password, c.Sudo} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Words say the credential as the receipt does: its kind and where it came
// from, never a value.
func (c Credential) Words() string {
	var w string
	switch c.Auth {
	case hostlink.AuthKey:
		w = "a key file (" + c.KeyFile + ")"
		if c.Passphrase != "" {
			w += " with its passphrase"
		}
	default:
		w = "a password"
	}
	w += " " + fromWords(c.From)
	if c.Sudo != "" {
		w += "; a sudo password " + fromWords(c.SudoFrom)
	} else {
		w += "; no sudo password"
	}
	return w
}

func fromWords(from string) string {
	switch from {
	case FromFlag:
		return "given by a flag"
	case FromKeychain:
		return "saved in the system keychain"
	}
	return "typed at the run's start"
}

// Flags are the target-host flags of agentry run, by host id: a key file's
// path, and the environment variables a passphrase, a password and a sudo
// password are read from.
type Flags struct {
	Key, Passphrase, Password, Sudo map[string]string
}

// ParseFlags reads --host-key id=PATH, --host-passphrase id=ENV,
// --host-password id=ENV and --host-sudo-password id=ENV.
func ParseFlags(keys, passphrases, passwords, sudos []string) (Flags, error) {
	var f Flags
	var err error
	if f.Key, err = parseFlag("--host-key", keys, false); err != nil {
		return f, err
	}
	if f.Passphrase, err = parseFlag("--host-passphrase", passphrases, true); err != nil {
		return f, err
	}
	if f.Password, err = parseFlag("--host-password", passwords, true); err != nil {
		return f, err
	}
	if f.Sudo, err = parseFlag("--host-sudo-password", sudos, true); err != nil {
		return f, err
	}
	for id := range f.Password {
		if _, both := f.Key[id]; both {
			return f, fmt.Errorf("--host-key and --host-password both name %s: give one way to sign in", id)
		}
	}
	return f, nil
}

func parseFlag(name string, vals []string, env bool) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range vals {
		id, val, ok := strings.Cut(v, "=")
		switch {
		case !ok || !hostlink.ValidID(id) || val == "":
			if env {
				return nil, fmt.Errorf("%s %q: give id=ENV_NAME, the name of the variable that holds it", name, v)
			}
			return nil, fmt.Errorf("%s %q: give id=PATH", name, v)
		case env && strings.ContainsAny(val, "= \t"):
			return nil, fmt.Errorf("%s %s: give the name of an environment variable; the value itself never goes on the command line", name, id)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("%s names %s twice", name, id)
		}
		out[id] = val
	}
	return out, nil
}

// Unknown are the ids the flags name that are none of ids.
func (f Flags) Unknown(ids []string) []string {
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	var out []string
	for _, m := range []map[string]string{f.Key, f.Passphrase, f.Password, f.Sudo} {
		for id := range m {
			if !known[id] && !containsStr(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// saved is a target host's sign-in kept in the system keychain, under
// Answer.Account: a key file's path (never the key) and its passphrase, or
// a password, and a sudo password.
type saved struct {
	KeyFile      string `json:"keyFile,omitempty"`
	Passphrase   string `json:"passphrase,omitempty"`
	Password     string `json:"password,omitempty"`
	SudoPassword string `json:"sudoPassword,omitempty"`
}

var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
)

// readSaved is the sign-in saved for a. A schedule that runs while its person
// is signed out has its saved keys sealed beside it and no keychain, and reads
// the sign-in from that credential alone, as the model key and a source's token
// are; anywhere else, the keychain.
func readSaved(a Answer) (saved, bool, error) {
	v, sealed, err := modelkey.SealedAccount(a.Account())
	switch {
	case sealed && err != nil:
		return saved{}, false, err
	case sealed && v == "":
		return saved{}, false, nil
	case !sealed && modelkey.Disabled():
		return saved{}, false, nil
	case !sealed:
		v, err = keyringGet(modelkey.Service, a.Account())
		switch {
		case errors.Is(err, keyring.ErrNotFound):
			return saved{}, false, nil
		case err != nil:
			return saved{}, false, fmt.Errorf("read the saved sign-in for %s from the system keychain: %w", a, err)
		}
	}
	var s saved
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return saved{}, false, fmt.Errorf("the saved sign-in for %s in the system keychain is not one this version reads", a)
	}
	return s, true, nil
}

func writeSaved(a Answer, s saved) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := keyringSet(modelkey.Service, a.Account(), string(b)); err != nil {
		return fmt.Errorf("save the sign-in for %s in the system keychain: %w", a, err)
	}
	return nil
}

// HasSavedSignIn reports whether a sign-in for a is saved (in the keychain,
// or the sealed credential of a signed-out schedule's fire): what a
// scheduled run, with no flag and no one to ask, signs in with.
func HasSavedSignIn(a Answer) (bool, error) {
	s, ok, err := readSaved(a)
	return ok && (s.KeyFile != "" || s.Password != ""), err
}

// keepSignIn saves c for a in the keychain, whatever it came from: a schedule's
// runs read nothing else. A key is kept as its file's path, never its bytes.
func keepSignIn(a Answer, c Credential) error {
	if modelkey.Disabled() {
		return errors.New("the system keychain is turned off here (AGENTRY_NO_KEYRING), and a schedule's runs read a target host's sign-in from it alone")
	}
	s := saved{KeyFile: c.KeyFile, Passphrase: c.Passphrase, Password: c.Password, SudoPassword: c.Sudo}
	if c.Auth == hostlink.AuthKey {
		s.Password = ""
		if abs, err := filepath.Abs(c.KeyFile); err == nil {
			s.KeyFile = abs
		}
	}
	return writeSaved(a, s)
}

// ResolveCredential takes a target host's sign-in, in the order above. ask
// nil is a run with no terminal: what no flag and no saved sign-in gives
// is refused (the sign-in) or gone without (sudo). warn reports a keychain
// that could not be read, which the run then looks past.
func ResolveCredential(id string, a Answer, f Flags, getenv func(string) string, ask Asker, warn func(string)) (Credential, error) {
	return resolveCredential(id, a, f, getenv, ask, warn, false)
}

// resolveCredential is ResolveCredential; keep saves what it took in the
// keychain without asking, for a schedule (keepSignIn).
func resolveCredential(id string, a Answer, f Flags, getenv func(string) string, ask Asker, warn func(string), keep bool) (Credential, error) {
	var c Credential
	// The keychain is read only when a flag leaves something to take from
	// it: a machine with none (a headless host) is not warned about for
	// nothing.
	var sv saved
	haveSaved := false
	if f.Key[id] == "" && f.Password[id] == "" || f.Sudo[id] == "" {
		var err error
		if sv, haveSaved, err = readSaved(a); err != nil {
			warn(err.Error())
		}
	}
	var err error
	prompted := false
	envValue := func(flag, name string) (string, error) {
		v := getenv(name)
		if v == "" {
			return "", refuse(id, RefusedCredential, "%s %s=%s: the variable %s is not set", flag, id, name, name)
		}
		return v, nil
	}
	switch {
	case f.Key[id] != "":
		c.Auth, c.KeyFile, c.From = hostlink.AuthKey, f.Key[id], FromFlag
	case f.Password[id] != "":
		if c.Password, err = envValue("--host-password", f.Password[id]); err != nil {
			return c, err
		}
		c.Auth, c.From = hostlink.AuthPassword, FromFlag
	case haveSaved && sv.KeyFile != "":
		c.Auth, c.KeyFile, c.Passphrase, c.From = hostlink.AuthKey, sv.KeyFile, sv.Passphrase, FromKeychain
	case haveSaved && sv.Password != "":
		c.Auth, c.Password, c.From = hostlink.AuthPassword, sv.Password, FromKeychain
	case ask != nil:
		prompted = true
		c.From = FromPrompt
		path, err := ask.Ask(fmt.Sprintf("Sign in to %s with the path of a private key file, or leave empty to type a password", a), "")
		if err != nil {
			return c, err
		}
		if path = strings.TrimSpace(path); path != "" {
			c.Auth, c.KeyFile = hostlink.AuthKey, expandHome(path)
		} else {
			pw, err := ask.AskSecret(fmt.Sprintf("Password for %s", a))
			if err != nil {
				return c, err
			}
			if pw == "" {
				return c, refuse(id, RefusedCredential, "no key and no password given for %s", a)
			}
			c.Auth, c.Password = hostlink.AuthPassword, pw
		}
	default:
		return c, refuse(id, RefusedCredential,
			"no sign-in for %s and no one at a terminal to give one: pass --host-key %s=PATH or --host-password %s=ENV, or save a sign-in by running once at a terminal",
			a, id, id)
	}
	if c.Auth == hostlink.AuthKey {
		if err := c.readKey(id, a, f, getenv, ask, &prompted); err != nil {
			return c, err
		}
	}
	switch {
	case f.Sudo[id] != "":
		if c.Sudo, err = envValue("--host-sudo-password", f.Sudo[id]); err != nil {
			return c, err
		}
		c.SudoFrom = FromFlag
	case haveSaved && sv.SudoPassword != "":
		c.Sudo, c.SudoFrom = sv.SudoPassword, FromKeychain
	case ask != nil:
		s, err := ask.AskSecret(fmt.Sprintf("sudo password for %s (leave empty if the run needs no sudo, or sudo asks for none)", a))
		if err != nil {
			return c, err
		}
		if s != "" {
			c.Sudo, c.SudoFrom, prompted = s, FromPrompt, true
		}
	}
	if keep {
		if c.From == FromKeychain && (c.Sudo == "" || c.SudoFrom == FromKeychain) {
			return c, nil
		}
		if err := keepSignIn(a, c); err != nil {
			return c, refuse(id, RefusedCredential, "keep the sign-in for %s: %v", a, err)
		}
		warn(fmt.Sprintf("target host %s: the sign-in for %s is kept in the system keychain (account %s), where the schedule's runs read it", id, a, a.Account()))
		return c, nil
	}
	if prompted && ask != nil && !modelkey.Disabled() {
		ok, err := ask.Confirm(fmt.Sprintf("Save this sign-in for %s in the system keychain, for later runs?", a), false)
		if err == nil && ok {
			s := saved{KeyFile: c.KeyFile, Passphrase: c.Passphrase, Password: c.Password, SudoPassword: c.Sudo}
			if c.Auth == hostlink.AuthKey {
				s.Password = ""
			}
			if err := writeSaved(a, s); err != nil {
				warn(err.Error())
			}
		}
	}
	return c, nil
}

// readKey reads the key file and, when it is locked, its passphrase: from
// the flag's variable, what was saved, or a prompt, each checked against
// the key.
func (c *Credential) readKey(id string, a Answer, f Flags, getenv func(string) string, ask Asker, prompted *bool) error {
	b, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return refuse(id, RefusedCredential, "the key file %s: %v", c.KeyFile, err)
	}
	c.Key = b
	_, err = ssh.ParseRawPrivateKey(b)
	var missing *ssh.PassphraseMissingError
	switch {
	case err == nil:
		c.Passphrase = ""
		return nil
	case !errors.As(err, &missing):
		return refuse(id, RefusedCredential, "%s is not a private key this runner reads: %v", c.KeyFile, err)
	}
	if name := f.Passphrase[id]; name != "" {
		c.Passphrase = getenv(name)
		if c.Passphrase == "" {
			return refuse(id, RefusedCredential, "--host-passphrase %s=%s: the variable %s is not set", id, name, name)
		}
	}
	if c.Passphrase == "" && ask != nil {
		p, err := ask.AskSecret(fmt.Sprintf("Passphrase for the key %s", c.KeyFile))
		if err != nil {
			return err
		}
		c.Passphrase, *prompted = p, true
	}
	if c.Passphrase == "" {
		return refuse(id, RefusedCredential, "the key %s has a passphrase, and none was given: pass --host-passphrase %s=ENV", c.KeyFile, id)
	}
	if _, err := ssh.ParseRawPrivateKeyWithPassphrase(b, []byte(c.Passphrase)); err != nil {
		return refuse(id, RefusedCredential, "the passphrase does not unlock the key %s", c.KeyFile)
	}
	return nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}
