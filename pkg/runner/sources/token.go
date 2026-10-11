package sources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
)

// A source's access token is asked for at run start and used by the fetch
// step alone (D5). It is taken, in order, from the environment variable an
// --intake-token flag names, from the person's saved key for the source's
// host in the system keychain, and from a prompt when there is a terminal;
// with none, the fetch is anonymous.

// keyPrefix names a saved source key's keychain account: one per host,
// under the clients' service (modelkey.Service), so the desktop reads and
// saves the same key (M6).
const keyPrefix = "source:"

// KeyAccount is the keychain account of a host's saved key.
func KeyAccount(host string) string { return keyPrefix + strings.ToLower(host) }

var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
)

// SavedToken is the key saved for a host, "" when there is none or the
// keychain is off (AGENTRY_NO_KEYRING).
func SavedToken(host string) (string, error) {
	if modelkey.Disabled() {
		return "", nil
	}
	v, err := keyringGet(modelkey.Service, KeyAccount(host))
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the saved key for %s from the system keychain: %w", host, err)
	}
	return strings.TrimSpace(v), nil
}

// SaveToken saves a host's key in the system keychain.
func SaveToken(host, token string) error {
	if modelkey.Disabled() {
		return errors.New("the system keychain is off (AGENTRY_NO_KEYRING)")
	}
	if err := keyringSet(modelkey.Service, KeyAccount(host), token); err != nil {
		return fmt.Errorf("save the key for %s in the system keychain: %w", host, err)
	}
	return nil
}

// ParseTokenFlags reads --intake-token id=ENV_NAME flags.
func ParseTokenFlags(flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		id, env, ok := strings.Cut(f, "=")
		id, env = strings.TrimSpace(id), strings.TrimSpace(env)
		if !ok || id == "" || env == "" {
			return nil, fmt.Errorf("--intake-token %q: want id=ENV_NAME, the name of an environment variable that holds the token", f)
		}
		if strings.ContainsAny(env, "= ") {
			return nil, fmt.Errorf("--intake-token %s: %q is not an environment variable's name (the token itself never goes on the command line)", id, env)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("--intake-token %s given twice", id)
		}
		out[id] = env
	}
	return out, nil
}

// TokenAsker asks for a token on a terminal and offers to save it.
type TokenAsker interface {
	AskSecret(prompt string) (string, error)
	Confirm(prompt string, def bool) (bool, error)
}

// ResolveTokens fills each source's token in order: the flag's variable,
// the saved key, the terminal (nil: none). A flag naming a variable that
// is unset or empty is an error, never a quiet anonymous fetch.
func ResolveTokens(srcs []*Source, flags map[string]string, getenv func(string) string, ask TokenAsker, warn func(string)) error {
	known := map[string]bool{}
	for _, s := range srcs {
		known[s.ID] = true
	}
	for id := range flags {
		if !known[id] {
			return fmt.Errorf("--intake-token %s: the QuickAgent asks for no input source with that id", id)
		}
	}
	for _, s := range srcs {
		if env, ok := flags[s.ID]; ok {
			v := strings.TrimSpace(getenv(env))
			if v == "" {
				return fmt.Errorf("--intake-token %s=%s: the environment variable %s is unset or empty", s.ID, env, env)
			}
			s.Token, s.TokenFrom = v, TokenFromFlag
			continue
		}
		saved, err := SavedToken(s.Host)
		if err != nil {
			// A keychain that cannot be read is said, and the run goes on
			// to the prompt or anonymously.
			if warn != nil {
				warn(err.Error())
			}
		}
		if saved != "" {
			s.Token, s.TokenFrom = saved, TokenFromKeychain
			continue
		}
		if ask == nil {
			continue
		}
		v, err := ask.AskSecret(fmt.Sprintf("Access token for %s (leave empty for a public %s)", s.Host, nounOf(s.Kind)))
		if err != nil {
			return err
		}
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		s.Token, s.TokenFrom = v, TokenFromPrompt
		if modelkey.Disabled() {
			continue
		}
		if save, err := ask.Confirm(fmt.Sprintf("Save this token in the system keychain for %s, for later runs?", s.Host), false); err == nil && save {
			if err := SaveToken(s.Host, v); err != nil && warn != nil {
				warn(err.Error())
			}
		}
	}
	return nil
}

// jsonStrict decodes one JSON value, refusing unknown fields and trailing
// data.
func jsonStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
