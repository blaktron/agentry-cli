// Package destination is where the person keeps the places a run's
// deliverables may be sent. A destination is saved by name: its kind and its
// non-secret fields in destinations.yaml under the CLI's configuration folder,
// and its credentials (the secret that signs a webhook's body, and the value
// of an Authorization header when the receiver wants one; a bucket's access key
// id and secret access key) in the system keychain, under accounts of the same service the CLI's other saved keys use,
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

// The kinds a destination can be: a webhook that receives one signed POST, and a
// bucket of an S3 API that receives each file as an object.
const (
	KindWebhook = send.KindWebhook
	KindS3      = send.KindS3
)

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
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Bucket, Prefix, Region and Endpoint are a bucket's fields: where its
	// objects go (bucket/prefix/run id/file), the signing region, and the
	// service's address when it is not Amazon's (R2, MinIO).
	Bucket   string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Prefix   string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Region   string `yaml:"region,omitempty" json:"region,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// OnMyNetwork lets the delivery reach an address on the person's own
	// network (private, shared or loopback space), and http as well as
	// https on any port. Without it a delivery reaches public addresses on
	// port 443 alone.
	OnMyNetwork bool `yaml:"onMyNetwork,omitempty" json:"onMyNetwork,omitempty"`
}

// S3Spec is a bucket destination's fields as the delivery step reads them.
func (d Destination) S3Spec() send.S3Spec {
	return send.S3Spec{Bucket: d.Bucket, Prefix: d.Prefix, Region: d.Region, Endpoint: d.Endpoint, OnMyNetwork: d.OnMyNetwork}
}

// Resolved is a destination with its credentials read, as the engine takes it.
// Its secrets are the engine's alone: they reach the delivery step on its stdin.
type Resolved struct {
	Destination
	// Secret signs the webhook's body, or is a bucket's secret access key;
	// Authorization is the value of the Authorization header, empty when the
	// receiver wants none; AccessKeyID is a bucket's access key id.
	Secret        string `json:"-" yaml:"-"`
	Authorization string `json:"-" yaml:"-"`
	AccessKeyID   string `json:"-" yaml:"-"`
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
// and an address the delivery step's own rule accepts. A webhook names an
// address and no bucket field; a bucket names its bucket and region, and no
// address but an optional endpoint.
func (d Destination) Check() error {
	if !ValidName(d.Name) {
		return errors.New("the name must be 1 to 40 lower-case letters, digits, '.', '-' or '_', beginning with a letter or digit")
	}
	switch d.Kind {
	case KindWebhook:
		if d.Bucket != "" || d.Prefix != "" || d.Region != "" || d.Endpoint != "" {
			return errors.New("a webhook has an address and no bucket, prefix, region or endpoint")
		}
		if _, err := send.CheckURL(d.URL, d.OnMyNetwork); err != nil {
			return err
		}
	case KindS3:
		if d.URL != "" {
			return errors.New("a bucket has an endpoint, not an address of its own")
		}
		if _, err := send.CheckS3(d.S3Spec()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the kind %q is not one this CLI delivers to (webhook and s3 are)", d.Kind)
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
	secretPrefix    = "destination-secret:"
	authPrefix      = "destination-authorization:"
	accessKeyPrefix = "destination-access-key:"
)

// SecretAccount, AuthorizationAccount and AccessKeyAccount are the keychain
// accounts of a destination's secret (a webhook's signing secret, a bucket's
// secret access key), its Authorization value and a bucket's access key id.
func SecretAccount(name string) string        { return secretPrefix + name }
func AuthorizationAccount(name string) string { return authPrefix + name }
func AccessKeyAccount(name string) string     { return accessKeyPrefix + name }

// SealAccounts are the keychain accounts a destination's deliveries read, each
// with whether it must be there: what a signed-out schedule seals so that a fire
// sends from it alone. A name that is not saved has the webhook's.
func (s Store) SealAccounts(name string) (required, optional []string) {
	if d, ok := s.Find(name); ok && d.Kind == KindS3 {
		return []string{SecretAccount(name), AccessKeyAccount(name)}, nil
	}
	return []string{SecretAccount(name)}, []string{AuthorizationAccount(name)}
}

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

// ValidSecretValues says whether a signing secret (empty: none given) and an
// Authorization value (empty: none) are values the delivery step can use.
func ValidSecretValues(secret, authorization string) bool {
	return (secret == "" || send.ValidSecret(secret)) && send.ValidHeaderValue(authorization)
}

// SaveSecrets keeps a destination's credentials in the system keychain. An
// empty authorization removes any saved one, and an empty secret leaves the
// saved secret as it is.
func SaveSecrets(name, secret, authorization string) error {
	if modelkey.Disabled() {
		return errors.New("the system keychain is off (AGENTRY_NO_KEYRING): a destination's credentials are kept there alone")
	}
	if !ValidSecretValues(secret, authorization) {
		return errors.New("a credential holds a control character or is not valid text")
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
	// A destination that was a bucket and is a webhook now keeps no key id.
	if err := keyringDelete(modelkey.Service, AccessKeyAccount(name)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the saved access key id from the system keychain: %w", err)
	}
	return nil
}

// Saved is a destination's credentials as they were saved, to put back when a
// change that wrote some of them cannot be finished.
type Saved struct {
	Secret, Authorization, AccessKeyID string
}

// Snapshot reads the destination's saved credentials.
func Snapshot(name string) Saved {
	var sv Saved
	sv.Secret, sv.Authorization, _ = Secrets(name)
	sv.AccessKeyID, _ = AccessKeyID(name)
	return sv
}

// Restore puts the credentials of a snapshot back, removing any the snapshot did
// not hold. Best effort: it is for the path of a failed save.
func Restore(name string, sv Saved) {
	if modelkey.Disabled() {
		return
	}
	for acct, v := range map[string]string{SecretAccount(name): sv.Secret, AuthorizationAccount(name): sv.Authorization, AccessKeyAccount(name): sv.AccessKeyID} {
		if v != "" {
			_ = keyringSet(modelkey.Service, acct, v)
		} else {
			_ = keyringDelete(modelkey.Service, acct)
		}
	}
}

// ValidS3Credentials says whether an access key id and a secret access key (an
// empty one: not given) are values the delivery step can sign with.
func ValidS3Credentials(accessKeyID, secret string) bool {
	return (accessKeyID == "" || send.ValidAccessKeyID(accessKeyID)) && (secret == "" || send.ValidSecret(secret))
}

// SaveS3Credentials keeps a bucket destination's access key id and secret access
// key in the system keychain; an empty one leaves the saved value as it is.
func SaveS3Credentials(name, accessKeyID, secret string) error {
	if modelkey.Disabled() {
		return errors.New("the system keychain is off (AGENTRY_NO_KEYRING): a destination's credentials are kept there alone")
	}
	if !ValidS3Credentials(accessKeyID, secret) {
		return errors.New("the access key id or secret access key is not one a request can be signed with")
	}
	// Both or neither: a pair of one service's id and another's secret signs
	// nothing and would be sent to a service that is not either's.
	before := Snapshot(name)
	if accessKeyID != "" {
		if err := keyringSet(modelkey.Service, AccessKeyAccount(name), accessKeyID); err != nil {
			return fmt.Errorf("save the access key id in the system keychain: %w", err)
		}
	}
	if secret != "" {
		if err := keyringSet(modelkey.Service, SecretAccount(name), secret); err != nil {
			Restore(name, before)
			return fmt.Errorf("save the secret access key in the system keychain: %w", err)
		}
	}
	// A destination that was a webhook and is a bucket now keeps no
	// Authorization value.
	if err := keyringDelete(modelkey.Service, AuthorizationAccount(name)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the saved Authorization value from the system keychain: %w", err)
	}
	return nil
}

// AccessKeyID is a bucket destination's saved access key id.
func AccessKeyID(name string) (string, error) { return readAccount(AccessKeyAccount(name)) }

// DeleteSecrets removes a destination's credentials from the keychain.
func DeleteSecrets(name string) error {
	if modelkey.Disabled() {
		return nil
	}
	var errs []error
	for _, a := range []string{SecretAccount(name), AuthorizationAccount(name), AccessKeyAccount(name)} {
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

// HasAccessKey says whether an access key id is saved for the destination.
func HasAccessKey(name string) bool {
	v, err := readAccount(AccessKeyAccount(name))
	return err == nil && v != ""
}

// HasAuthorization says whether an Authorization value is saved for it.
func HasAuthorization(name string) bool {
	v, err := readAccount(AuthorizationAccount(name))
	return err == nil && v != ""
}

// Resolve reads the destinations a run was given by name, with their
// credentials, in the order named and each once. A name that is not saved, or
// a destination with no signing secret (a bucket: no key pair), refuses the run
// before it starts: a delivery never silently goes unsigned or nowhere.
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
		if d.Kind == KindS3 {
			id, err := AccessKeyID(n)
			if err != nil {
				return nil, err
			}
			if secret == "" || id == "" {
				return nil, fmt.Errorf("the destination %q has no access key id and secret access key saved: add it again with agentry destinations add", n)
			}
			out = append(out, Resolved{Destination: d, Secret: secret, AccessKeyID: id})
			continue
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
// receiver's own token: the scheme, host and port the delivery reaches. A bucket
// is s3://bucket/prefix and the service's host and port.
func (d Destination) Where() string {
	if d.Kind == KindS3 {
		t, err := send.CheckS3(d.S3Spec())
		if err != nil {
			return "(bucket not valid)"
		}
		at := "s3://" + d.Bucket
		if d.Prefix != "" {
			at += "/" + d.Prefix
		}
		return at + " at " + t.Scheme + "://" + t.HostPort
	}
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
