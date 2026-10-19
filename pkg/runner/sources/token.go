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
// step alone. It is taken, in order, from the environment variable an
// --intake-token flag names, from the person's saved key for the source's
// host in the system keychain, and from a prompt when there is a terminal;
// with none, the fetch is anonymous.

// keyPrefix names a saved source key's keychain account: one per host,
// under the clients' service (modelkey.Service), so the desktop reads and
// saves the same key.
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
	v, _, err := savedToken(host)
	return v, err
}

// savedToken is SavedToken and where it was read: a signed-out schedule's fire
// has its saved keys sealed beside it and no keychain, and reads them from that
// credential alone, as modelkey does; anywhere else, the keychain.
func savedToken(host string) (val, from string, err error) {
	v, sealed, err := modelkey.SealedAccount(KeyAccount(host))
	if sealed {
		return strings.TrimSpace(v), TokenFromSealed, err
	}
	if modelkey.Disabled() {
		return "", "", nil
	}
	v, err = keyringGet(modelkey.Service, KeyAccount(host))
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("read the saved key for %s from the system keychain: %w", host, err)
	}
	return strings.TrimSpace(v), TokenFromKeychain, nil
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

// TokenFlag is the run's flag that names a source's token by its question
// id; a build names one by its host (BuildTokenFlag).
const (
	TokenFlag      = "--intake-token"
	BuildTokenFlag = "--source-token"
)

// ParseTokenFlags reads --intake-token id=ENV_NAME flags.
func ParseTokenFlags(flags []string) (map[string]string, error) {
	return ParseTokenFlagsFor(TokenFlag, flags)
}

// ParseTokenFlagsFor reads flag's key=ENV_NAME values: a run's question
// ids, a build's hosts.
func ParseTokenFlagsFor(flag string, flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		id, env, ok := strings.Cut(f, "=")
		id, env = strings.TrimSpace(id), strings.TrimSpace(env)
		if !ok || id == "" || env == "" {
			return nil, fmt.Errorf("%s %q: want %s=ENV_NAME, the name of an environment variable that holds the token", flag, f, keyWord(flag))
		}
		if strings.ContainsAny(env, "= ") {
			return nil, fmt.Errorf("%s %s: %q is not an environment variable's name (the token itself never goes on the command line)", flag, id, env)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("%s %s given twice", flag, id)
		}
		out[id] = env
	}
	return out, nil
}

// keyWord is what a token flag's key names.
func keyWord(flag string) string {
	if flag == BuildTokenFlag {
		return "HOST"
	}
	return "id"
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
			return fmt.Errorf("%s %s: the QuickAgent asks for no input source with that id", TokenFlag, id)
		}
	}
	return resolveTokens(TokenFlag, srcs, func(s *Source) (string, bool) { env, ok := flags[s.ID]; return env, ok }, getenv, ask, warn)
}

// ResolveBuildTokens is ResolveTokens for a build's sources, whose flags
// name a host rather than a question: every source on that host takes the
// variable. A host no source is on is an error.
func ResolveBuildTokens(srcs []*Source, flags map[string]string, getenv func(string) string, ask TokenAsker, warn func(string)) error {
	known := map[string]bool{}
	for _, s := range srcs {
		known[s.Host] = true
	}
	for host := range flags {
		if !known[strings.ToLower(host)] {
			return fmt.Errorf("%s %s: no repository or address given is on that host", BuildTokenFlag, host)
		}
	}
	return resolveTokens(BuildTokenFlag, srcs, func(s *Source) (string, bool) {
		for host, env := range flags {
			if strings.EqualFold(host, s.Host) {
				return env, true
			}
		}
		return "", false
	}, getenv, ask, warn)
}

func resolveTokens(flag string, srcs []*Source, flagged func(*Source) (string, bool), getenv func(string) string, ask TokenAsker, warn func(string)) error {
	for _, s := range srcs {
		if env, ok := flagged(s); ok {
			v := strings.TrimSpace(getenv(env))
			if v == "" {
				key := s.ID
				if flag == BuildTokenFlag {
					key = s.Host
				}
				return fmt.Errorf("%s %s=%s: the environment variable %s is unset or empty", flag, key, env, env)
			}
			s.Token, s.TokenFrom = v, TokenFromFlag
			continue
		}
		saved, from, err := savedToken(s.Host)
		if err != nil {
			// A keychain that cannot be read is said, and the run goes on
			// to the prompt or anonymously.
			if warn != nil {
				warn(err.Error())
			}
		}
		if saved != "" {
			s.Token, s.TokenFrom = saved, from
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
