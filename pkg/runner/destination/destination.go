// Package destination is where the person keeps the places a run's
// deliverables may be sent. A destination is saved by name: its kind and its
// non-secret fields in destinations.yaml under the CLI's configuration folder,
// and its credentials (the secret that signs a webhook's body, and the value
// of an Authorization header when the receiver wants one) in the system
// keychain, under accounts of the same service the CLI's other saved keys use,
// so the CLI and the desktop read and write the same entries.
//
// A destination is the person's alone: no manifest field, QuickAgent or
// schedule names one. A run delivers to the destinations it is given by name
// (--destination, a QuickAgent's default set the person chose, or a schedule
// that kept the names) and nowhere else.
package destination

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/runnerimage/send"
)

// File is the folder's destinations.yaml name.
const File = "destinations.yaml"

// KindWebhook is the one kind a destination can be so far.
const KindWebhook = send.KindWebhook

// MaxDestinations bounds how many a person keeps, and MaxPerRun how many one
// run delivers to.
const (
	MaxDestinations = 64
	MaxPerRun       = 8
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// agentRE is a QuickAgent as a default set names it: namespace/name.
var agentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName says whether name can name a destination: one to forty lower-case
// letters, digits, dots, hyphens and underscores, beginning with a letter or
// digit.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Destination is one saved destination's non-secret fields.
type Destination struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
	// URL is the webhook's address.
	URL string `yaml:"url" json:"url"`
	// OnMyNetwork lets the delivery reach an address on the person's own
	// network (private, shared or loopback space), and http as well as
	// https on any port. Without it a delivery reaches public addresses on
	// port 443 alone.
	OnMyNetwork bool `yaml:"onMyNetwork,omitempty" json:"onMyNetwork,omitempty"`
}

// Resolved is a destination with its credentials read, as the engine takes it.
// Its secrets are the engine's alone: they reach the delivery step on its stdin.
type Resolved struct {
	Destination
	// Secret signs the webhook's body; Authorization is the value of the
	// Authorization header, empty when the receiver wants none.
	Secret        string `json:"-" yaml:"-"`
	Authorization string `json:"-" yaml:"-"`
}

// Store is the destinations file as a program reads and writes it.
type Store struct {
	Destinations []Destination `yaml:"destinations,omitempty"`
	// Defaults is each QuickAgent's default set: the names a run of it
	// delivers to when the run names none. The key is namespace/name, so a
	// new version keeps it.
	Defaults map[string][]string `yaml:"defaults,omitempty"`
}

// Path is the destinations file.
func Path() (string, error) {
	dir, err := configdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, File), nil
}

// Load reads the destinations file; a missing one is an empty store.
func Load() (Store, error) {
	var s Store
	p, err := Path()
	if err != nil {
		return s, err
	}
	raw, err := os.ReadFile(p) //nolint:gosec // fixed, user-owned config path
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read %s: %w", p, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return Store{}, fmt.Errorf("parse %s: %w", p, err)
	}
	if err := s.check(); err != nil {
		return Store{}, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// check holds a store to its rules. A file the CLI never wrote (an entry
// with a bad name or address, a default naming no destination) is refused as
// a whole rather than half read.
func (s *Store) check() error {
	if len(s.Destinations) > MaxDestinations {
		return fmt.Errorf("more than %d destinations", MaxDestinations)
	}
	seen := map[string]bool{}
	for _, d := range s.Destinations {
		if err := d.Check(); err != nil {
			return fmt.Errorf("destination %q: %w", d.Name, err)
		}
		if seen[d.Name] {
			return fmt.Errorf("destination %q is listed twice", d.Name)
		}
		seen[d.Name] = true
	}
	for agent, names := range s.Defaults {
		if !agentRE.MatchString(agent) {
			return fmt.Errorf("default set for %q: not a namespace/name", agent)
		}
		if len(names) > MaxPerRun {
			return fmt.Errorf("default set for %s: more than %d destinations", agent, MaxPerRun)
		}
		for _, n := range names {
			if !seen[n] {
				return fmt.Errorf("default set for %s names %q, which is not saved", agent, n)
			}
		}
	}
	return nil
}

// Check holds a destination to its rules: a name, a kind this CLI sends to,
// and an address the delivery step's own rule accepts.
func (d Destination) Check() error {
	if !ValidName(d.Name) {
		return errors.New("the name must be 1 to 40 lower-case letters, digits, '.', '-' or '_', beginning with a letter or digit")
	}
	if d.Kind != KindWebhook {
		return fmt.Errorf("the kind %q is not one this CLI delivers to (webhook is)", d.Kind)
	}
	if _, err := send.CheckURL(d.URL, d.OnMyNetwork); err != nil {
		return err
	}
	return nil
}

// Save writes the store, atomically and for the person alone.
func (s Store) Save() error {
	if err := s.check(); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".destinations-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(name, 0o600)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Rename(name, p); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

// Find is the destination saved under name.
func (s Store) Find(name string) (Destination, bool) {
	for _, d := range s.Destinations {
		if d.Name == name {
			return d, true
		}
	}
	return Destination{}, false
}

// Upsert adds d or replaces the destination of its name, in name order.
func (s *Store) Upsert(d Destination) error {
	if err := d.Check(); err != nil {
		return err
	}
	for i := range s.Destinations {
		if s.Destinations[i].Name == d.Name {
			s.Destinations[i] = d
			return nil
		}
	}
	if len(s.Destinations) >= MaxDestinations {
		return fmt.Errorf("a person keeps at most %d destinations", MaxDestinations)
	}
	s.Destinations = append(s.Destinations, d)
	sort.Slice(s.Destinations, func(i, j int) bool { return s.Destinations[i].Name < s.Destinations[j].Name })
	return nil
}

// Delete removes the destination and every default set's mention of it.
func (s *Store) Delete(name string) bool {
	n := len(s.Destinations)
	s.Destinations = slices.DeleteFunc(s.Destinations, func(d Destination) bool { return d.Name == name })
	for agent, names := range s.Defaults {
		names = slices.DeleteFunc(names, func(x string) bool { return x == name })
		if len(names) == 0 {
			delete(s.Defaults, agent)
		} else {
			s.Defaults[agent] = names
		}
	}
	return len(s.Destinations) != n
}

// SetDefault sets the QuickAgent's default set to names, each saved and none
// twice; no names clears it.
func (s *Store) SetDefault(agent string, names []string) error {
	if !agentRE.MatchString(agent) {
		return fmt.Errorf("%q is not a QuickAgent as namespace/name", agent)
	}
	var set []string
	for _, n := range names {
		if _, ok := s.Find(n); !ok {
			return fmt.Errorf("no destination is saved under %q", n)
		}
		if !slices.Contains(set, n) {
			set = append(set, n)
		}
	}
	if len(set) > MaxPerRun {
		return fmt.Errorf("a run delivers to at most %d destinations", MaxPerRun)
	}
	if len(set) == 0 {
		delete(s.Defaults, agent)
		return nil
	}
	if s.Defaults == nil {
		s.Defaults = map[string][]string{}
	}
	s.Defaults[agent] = set
	return nil
}

// DefaultsFor are the names a run of the QuickAgent (a release ref,
// namespace/name@version, or namespace/name) delivers to when it names none.
func (s Store) DefaultsFor(ref string) []string {
	agent, _, _ := strings.Cut(ref, "@")
	return slices.Clone(s.Defaults[agent])
}

// Keychain accounts of a destination's credentials, under modelkey.Service.
const (
	secretPrefix = "destination-secret:"
	authPrefix   = "destination-authorization:"
)

// SecretAccount and AuthorizationAccount are the keychain accounts of a
// destination's signing secret and Authorization value.
func SecretAccount(name string) string        { return secretPrefix + name }
func AuthorizationAccount(name string) string { return authPrefix + name }

var (
	keyringGet    = keyring.Get
	keyringSet    = keyring.Set
	keyringDelete = keyring.Delete
)

// Secrets are a destination's credentials as saved: the signing secret, and
// the Authorization value when there is one.
func Secrets(name string) (secret, authorization string, err error) {
	secret, err = readAccount(SecretAccount(name))
	if err != nil {
		return "", "", err
	}
	authorization, err = readAccount(AuthorizationAccount(name))
	if err != nil {
		return "", "", err
	}
	return secret, authorization, nil
}

// readAccount reads one account: a signed-out schedule's fire reads the
// credential sealed beside it and never the keychain; anywhere else the
// keychain. A value that is not there is empty.
func readAccount(account string) (string, error) {
	v, sealed, err := modelkey.SealedAccount(account)
	if sealed {
		return strings.TrimSpace(v), err
	}
	if modelkey.Disabled() {
		return "", nil
	}
	v, err = keyringGet(modelkey.Service, account)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read %s from the system keychain: %w", account, err)
	}
	return strings.TrimSpace(v), nil
}

// SaveSecrets keeps a destination's credentials in the system keychain. An
// empty authorization removes any saved one, and an empty secret leaves the
// saved secret as it is.
func SaveSecrets(name, secret, authorization string) error {
	if modelkey.Disabled() {
		return errors.New("the system keychain is off (AGENTRY_NO_KEYRING): a destination's credentials are kept there alone")
	}
	for _, v := range []string{secret, authorization} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("a credential holds a line break or a control character")
		}
	}
	if secret != "" {
		if err := keyringSet(modelkey.Service, SecretAccount(name), secret); err != nil {
			return fmt.Errorf("save the signing secret in the system keychain: %w", err)
		}
	}
	if authorization != "" {
		if err := keyringSet(modelkey.Service, AuthorizationAccount(name), authorization); err != nil {
			return fmt.Errorf("save the Authorization value in the system keychain: %w", err)
		}
	} else if err := keyringDelete(modelkey.Service, AuthorizationAccount(name)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the saved Authorization value from the system keychain: %w", err)
	}
	return nil
}

// DeleteSecrets removes a destination's credentials from the keychain.
func DeleteSecrets(name string) error {
	if modelkey.Disabled() {
		return nil
	}
	var errs []error
	for _, a := range []string{SecretAccount(name), AuthorizationAccount(name)} {
		if err := keyringDelete(modelkey.Service, a); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, fmt.Errorf("remove %s from the system keychain: %w", a, err))
		}
	}
	return errors.Join(errs...)
}

// HasSecret says whether a signing secret is saved for the destination.
func HasSecret(name string) bool {
	v, err := readAccount(SecretAccount(name))
	return err == nil && v != ""
}

// HasAuthorization says whether an Authorization value is saved for it.
func HasAuthorization(name string) bool {
	v, err := readAccount(AuthorizationAccount(name))
	return err == nil && v != ""
}

// Resolve reads the destinations a run was given by name, with their
// credentials, in the order named and each once. A name that is not saved, or
// a destination with no signing secret, refuses the run before it starts: a
// delivery never silently goes unsigned or nowhere.
func Resolve(names []string) ([]Resolved, error) {
	if len(names) == 0 {
		return nil, nil
	}
	s, err := Load()
	if err != nil {
		return nil, err
	}
	var out []Resolved
	for _, n := range names {
		if slices.ContainsFunc(out, func(r Resolved) bool { return r.Name == n }) {
			continue
		}
		d, ok := s.Find(n)
		if !ok {
			return nil, fmt.Errorf("no destination is saved under %q (agentry destinations list)", n)
		}
		secret, auth, err := Secrets(n)
		if err != nil {
			return nil, err
		}
		if secret == "" {
			return nil, fmt.Errorf("the destination %q has no signing secret saved: add it again with agentry destinations add", n)
		}
		out = append(out, Resolved{Destination: d, Secret: secret, Authorization: auth})
	}
	if len(out) > MaxPerRun {
		return nil, fmt.Errorf("a run delivers to at most %d destinations", MaxPerRun)
	}
	return out, nil
}

// Where describes a destination without its path or query, which may carry a
// receiver's own token: the scheme, host and port the delivery reaches.
func (d Destination) Where() string {
	t, err := send.CheckURL(d.URL, d.OnMyNetwork)
	if err != nil {
		return "(address not valid)"
	}
	scheme := t.URL.Scheme
	if t.URL.Port() == "" {
		return scheme + "://" + t.Host
	}
	return scheme + "://" + t.HostPort
}
