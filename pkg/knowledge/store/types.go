// Package store is the local knowledge store (plan knowledge.md §4.3;
// agentry-cli#319): the knowledge packs a person keeps on this machine,
// sealed, under the runner home the CLI and the desktop share, so that
// both clients list, open, attach and export the same packs.
//
// Each pack is one directory, <runner home>/knowledge/<pack id>/:
//
//	manifest.json  the pack's own knowledge/v1 manifest, in the clear
//	pack.sealed    the canonical tree tar (wiki/ and sources/), sealed with
//	               the core's pkg/packseal under a random data key per pack
//	key.json       that data key as it is wrapped: by the registry the
//	               person is signed in to (plan D4, D22), or, signed out, by
//	               this machine's key in the OS keychain (D20); and, once
//	               the pack is kept in Agentry, the registry's copy
//
// No page rests in a pack's directory in the clear. A pack is opened into
// a directory the caller names (an export), or into a 0700 directory inside
// the store that the caller removes when done (a run's attachment, a build
// adding to the pack; OpenScratch, Scratch); a download is checked in one
// before it is kept. Such directories are dot-named, so List skips them,
// and one a process that died left behind is swept after an hour. A data key
// the registry unwrapped is cached in the OS keychain (the modelkey
// pattern, DR-24's exposure noted), so a pack opens offline after its
// first opening on a machine. The registry holds what wraps the key, so a
// pack kept on one install opens on a fresh one after sign-in, with
// nothing carried between the two (D4).
//
// An organization's shared pack (Knowledge M8, agentry-cli#380; plan §4.7,
// D13) is kept the same way, its key.json naming the organization, with
// one difference: its data key is never cached. It is unwrapped through
// the organization's route every time the pack is opened, which the
// registry answers only while the person is an active member, so a member
// who is removed keeps sealed bytes they can no longer open, and a shared
// pack does not open offline.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The store's fixed names.
const (
	// Dir is the store's directory under the runner home.
	Dir = "knowledge"
	// SealedFile is a pack's sealed tree.
	SealedFile = "pack.sealed"
	// KeyFile is a pack's wrapped data key.
	KeyFile = "key.json"
	// KeySchema is key.json's format.
	KeySchema = "knowledge-key/v1"
)

// Wrapper is what a pack's data key is wrapped by.
type Wrapper string

const (
	// WrapperRegistry is a key the registry wrapped (POST
	// /knowledge/keys/wrap): the deployment's key on a core registry, the
	// account's certificate on SecureAgentry.
	WrapperRegistry Wrapper = "registry"
	// WrapperMachine is a key this machine wrapped, signed out.
	WrapperMachine Wrapper = "machine"
)

// WrappedKey is a data key as a wrapper sealed it: the registry's answer
// kept exactly as it came (the wrapper's kind, the recipient it names, the
// sealed bytes), or this machine's.
type WrappedKey struct {
	Kind      string `json:"kind"`
	Recipient string `json:"recipient"`
	Key       []byte `json:"key"`
}

// KeyRecord is key.json.
type KeyRecord struct {
	Schema string `json:"schema"`
	// Wrapper is WrapperRegistry or WrapperMachine.
	Wrapper Wrapper `json:"wrapper"`
	// Registry is the registry that wrapped the key (WrapperRegistry): the
	// one that can unwrap it, for the account signed in when it did.
	Registry string `json:"registry,omitempty"`
	// Org is the organization whose shared pack this is (Knowledge M8),
	// by its handle: its key is the organization's, unwrapped through the
	// organization's route at every open and never cached. Empty for the
	// person's own.
	Org        string     `json:"org,omitempty"`
	WrappedKey WrappedKey `json:"wrappedKey"`
	// Agentry is the registry's copy, once the pack is uploaded.
	Agentry *AgentryCopy `json:"agentry,omitempty"`
}

// AgentryCopy names the copy of a pack a registry keeps.
type AgentryCopy struct {
	Registry string `json:"registry"`
	// ID is the registry's own id for the pack.
	ID string `json:"id"`
	// Digest is the tree uploaded; a local digest that differs means the
	// pack changed here since.
	Digest string `json:"digest"`
}

// Remote is a pack as a registry describes it (GET /knowledge/packs).
type Remote struct {
	ID         string
	Name       string
	State      string
	Bytes      int64
	Files      int
	Pages      int
	Digest     string
	WrappedKey WrappedKey
	// Scan is the registry's scan report, as it wrote it; empty until one.
	Scan    json.RawMessage
	Created time.Time
	Updated time.Time
	// Shared, Org and Exportable describe an organization's shared pack
	// (Knowledge M8): the organization's handle, and whether its admins
	// marked it exportable by members (D26).
	Shared     bool
	Org        string
	Exportable bool
}

// The states a registry's pack is in.
const (
	StateEmpty    = "empty"
	StateScanning = "scanning"
	StateReady    = "ready"
	StateFailed   = "failed"
)

// Registry is a registry's knowledge routes, for the account signed in to
// it (pkg/knowledge/remote is the client; a test fakes the routes).
type Registry interface {
	URL() string
	Wrap(ctx context.Context, dataKey []byte) (WrappedKey, error)
	Unwrap(ctx context.Context, wk WrappedKey) ([]byte, error)
	Create(ctx context.Context, name string, wk WrappedKey) (Remote, error)
	PutTree(ctx context.Context, id string, sealed []byte) (Remote, error)
	List(ctx context.Context) ([]Remote, error)
	GetTree(ctx context.Context, id string) (sealed []byte, digest string, err error)
	Delete(ctx context.Context, id string) error
}

// Registries resolves a client for the registry at url, signed in; an
// error wrapping ErrSignedOut when there is no credential for it.
type Registries func(url string) (Registry, error)

// Shared is a registry's routes for one organization's shared packs, for
// the account signed in, which the registry answers only while it is an
// active member of the organization (Knowledge M8; the core's
// /orgs/{org}/knowledge/, agentry#646).
type Shared interface {
	URL() string
	Unwrap(ctx context.Context, wk WrappedKey) ([]byte, error)
	List(ctx context.Context) ([]Remote, error)
	GetTree(ctx context.Context, id string) (sealed []byte, digest string, err error)
	// Export is the pack's tree decrypted by the registry, the canonical
	// tar, and its digest: an admin's always, a member's only once the
	// pack is marked exportable (ErrNotExportable otherwise, D26).
	Export(ctx context.Context, id string) (tree []byte, digest string, err error)
}

// SharedRegistries resolves a client for org's shared packs at the
// registry at url, signed in; an error wrapping ErrSignedOut when there is
// no credential for it.
type SharedRegistries func(url, org string) (Shared, error)

// Keychain is the OS keychain, as the store uses it (OSKeychain is the
// real one; a test passes a map).
type Keychain interface {
	// Get is an account's value; an error wrapping ErrNoEntry when there
	// is none.
	Get(account string) (string, error)
	Set(account, value string) error
	// Delete removes an account; one that is not there is not an error.
	Delete(account string) error
}

var (
	// ErrNotFound is a pack this store does not keep.
	ErrNotFound = errors.New("no knowledge pack of that name is kept on this computer")
	// ErrNameTaken is a name (or a pack) this store already keeps.
	ErrNameTaken = errors.New("a knowledge pack of that name is already kept on this computer")
	// ErrSignedOut is a registry this machine holds no credential for.
	ErrSignedOut = errors.New("not signed in")
	// ErrKeyNotYours is a wrapped key the registry would not unwrap for
	// the account signed in: wrapped for another account, under a key
	// since destroyed, or altered (404 knowledge-key-not-found).
	ErrKeyNotYours = errors.New("the wrapped key does not open for the account signed in")
	// ErrScanning is a registry's pack being scanned (409).
	ErrScanning = errors.New("the pack is being scanned in Agentry; it can be downloaded once the scan ends")
	// ErrRemoteNotFound is a pack the registry does not keep.
	ErrRemoteNotFound = errors.New("the registry keeps no such knowledge pack")
	// ErrRemoteNameTaken is a name the account already uses in Agentry.
	ErrRemoteNameTaken = errors.New("this account already keeps a knowledge pack of that name in Agentry")
	// ErrUnconfigured is a registry that keeps no knowledge (503).
	ErrUnconfigured = errors.New("this registry keeps no knowledge packs")
	// ErrNoKeychain is an OS keychain that is not there or is disabled.
	ErrNoKeychain = errors.New("the OS keychain is not available")
	// ErrNoEntry is a keychain account with no value.
	ErrNoEntry = errors.New("no such keychain entry")
	// ErrNotExportable is a member's export of an organization's shared
	// pack its admins have not marked exportable (403
	// knowledge-not-exportable, D26).
	ErrNotExportable = errors.New("only the organization's admins export this pack unencrypted, unless one of them marks it exportable")
	// ErrShared is a change to an organization's shared pack from this
	// computer: its admins write it in Agentry.
	ErrShared = errors.New("it is an organization's shared pack, which its admins change in Agentry, not from this computer")
	// ErrPastAgentryCap is a pack kept here that is larger than Agentry
	// keeps (knowledge.MaxBytes): a local build may make one up to
	// knowledge.MaxLocalBytes, and it stays on this computer (agentry-cli#429).
	ErrPastAgentryCap = errors.New("the pack is larger than Agentry keeps")
)
