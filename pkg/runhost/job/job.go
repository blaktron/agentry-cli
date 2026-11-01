// Package job is the document a device sends a remote runhost to start one
// run: the release it verified, what it decided about the run, and what the
// run needs from the device. It is typed and versioned, and it is the only
// thing the service reads from a device before it runs anything.
//
// On the wire a job is one length-prefixed JSON document at the start of the
// `job` verb's input. The service then answers whether it needs the
// release's archive (FrameRelease); when it does, the archive's bytes follow
// the document, exactly Release.Size of them.
package job

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// Version is the job document's version.
const Version = 1

// MaxDocument bounds a job document on the wire.
const MaxDocument = 1 << 20

// MaxArchive bounds a release archive a job carries: the CLI's own cap on a
// downloaded archive.
const MaxArchive = 512 << 20

// Attestation states a release may arrive in, as the device judged it.
const (
	StateVerified  = "verified"
	StateUnsigned  = "unsigned"
	StateUnscanned = "unscanned"
)

// Job is one run on a remote runhost.
type Job struct {
	V int `json:"v"`
	// ID names the job on the server: its job directory and its sealed
	// output. The device makes it.
	ID string `json:"id"`
	// Device is the id of the device that sent the job, one of the paired
	// devices.
	Device  string  `json:"device"`
	Release Release `json:"release"`
	Run     Run     `json:"run"`
}

// Release is the release to run, as the device resolved and verified it.
type Release struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	// Digest is the archive's sha256:… digest.
	Digest string `json:"digest"`
	// Size is the archive's length in bytes.
	Size int64 `json:"size"`
	// Registry is the registry the release came from.
	Registry string `json:"registry"`
	// State is the device's verdict: verified, unsigned or unscanned.
	State string `json:"state"`
	// Attestation is the registry's DSSE envelope for the release, as the
	// device fetched it, for a verified release; empty otherwise.
	Attestation json.RawMessage `json:"attestation,omitempty"`
	// Keys are the signing keys the device holds pinned for Registry. The
	// runhost pins them the first time a release from that registry
	// arrives, and verifies every later one against its own pins.
	Keys trust.KeySet `json:"keys"`
}

// Ref is the release's exact reference, ns/name@version.
func (r Release) Ref() string { return r.Namespace + "/" + r.Name + "@" + r.Version }

// Run is what the device decided about the run.
type Run struct {
	// Task is the run's task text; empty sends the agent its own
	// instruction.
	Task string `json:"task,omitempty"`
	// AllowUnsigned and AllowUnscanned are the person's own flags for this
	// run.
	AllowUnsigned  bool `json:"allowUnsigned,omitempty"`
	AllowUnscanned bool `json:"allowUnscanned,omitempty"`
	// Adapter is the harness adapter the person asked for. A runhost takes
	// only its default, the container harness: the host-process harnesses
	// would run outside gVisor.
	Adapter string `json:"adapter,omitempty"`
	// Timeout, MaxTurns and MaxCost are the person's overrides of the
	// policy's budgets; a negative MaxTurns or MaxCost and a zero Timeout
	// leave the policy's.
	Timeout  int     `json:"timeout,omitempty"`
	MaxTurns int     `json:"maxTurns"`
	MaxCost  float64 `json:"maxCost"`
	// Model is the device policy's model, which the run uses in place of
	// the runhost's.
	Model Model `json:"model"`
	// ModelKey is the model's key, read from the device's environment or
	// keychain. It lives in the service's memory and the run's environment
	// only, and never in a file.
	ModelKey string `json:"modelKey,omitempty"`
}

// Model is the model a run uses: the device policy's, less where its key
// comes from, which on the runhost is always the run's environment.
type Model struct {
	Provider   string   `json:"provider"`
	Name       string   `json:"name"`
	APIKeyEnv  string   `json:"apiKeyEnv"`
	APIKeyKind string   `json:"apiKeyKind,omitempty"`
	BaseURL    string   `json:"baseUrl,omitempty"`
	ProxyAllow []string `json:"proxyAllow"`
}

// ModelOf is the job's model for the device policy's m.
func ModelOf(m policy.Model) Model {
	return Model{Provider: m.Provider, Name: m.Name, APIKeyEnv: m.APIKeyEnv, APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ProxyAllow: m.ProxyAllow}
}

// Policy is the model as the runhost's policy takes it: the key from the
// run's environment.
func (m Model) Policy() policy.Model {
	return policy.Model{Provider: m.Provider, Name: m.Name, APIKeyEnv: m.APIKeyEnv, APIKeySource: "env", APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ProxyAllow: m.ProxyAllow}
}

// keyEnvRE is the shape of a model key's variable: a provider's own name for
// its key (ANTHROPIC_API_KEY, HF_TOKEN).
var keyEnvRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}_(API_KEY|KEY|TOKEN)$`)

// reservedEnvPrefixes are variables the run's environment never takes from a
// device: the CLI's own (a hosted marker, a run grant, the runner home), the
// container engine's, the dynamic loader's, SSH's and git's.
var reservedEnvPrefixes = []string{"AGENTRY_", "SECUREAGENTRY_", "DOCKER_", "LD_", "SSH_", "GIT_", "XDG_", "CONTAINER_", "PODMAN_"}

// ValidKeyEnv says whether name may carry a model key into a run on a
// runhost: a key's name, and none the run's environment reserves.
func ValidKeyEnv(name string) bool {
	if !keyEnvRE.MatchString(name) {
		return false
	}
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// ValidID says whether id can name a job or a device.
func ValidID(id string) bool { return idRE.MatchString(id) }

var (
	idRE      = regexp.MustCompile(`^[0-9a-f]{16,64}$`)
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$`)
)

// Validate checks that the job is one this version reads, and that every
// name in it is one a path or an argument may carry.
func (j *Job) Validate() error {
	if j.V != Version {
		return fmt.Errorf("a job of version %d; this runhost reads version %d: update it", j.V, Version)
	}
	if !ValidID(j.ID) {
		return errors.New("the job's id is not a lower-case hex string")
	}
	if !ValidID(j.Device) {
		return errors.New("the job names no device")
	}
	r := j.Release
	if !segmentRE.MatchString(r.Namespace) || !segmentRE.MatchString(r.Name) {
		return fmt.Errorf("the release %q/%q is not a namespace and a name", r.Namespace, r.Name)
	}
	if !versionRE.MatchString(r.Version) {
		return fmt.Errorf("the release's version %q is not a version", r.Version)
	}
	if !digestRE.MatchString(r.Digest) {
		return fmt.Errorf("the release's digest %q is not a sha256 digest", r.Digest)
	}
	if r.Size <= 0 || r.Size > MaxArchive {
		return fmt.Errorf("the release's archive is %d bytes; it must be between 1 and %d", r.Size, MaxArchive)
	}
	if !strings.HasPrefix(r.Registry, "https://") && !strings.HasPrefix(r.Registry, "http://") {
		return fmt.Errorf("the release's registry %q is not an address", r.Registry)
	}
	switch r.State {
	case StateVerified:
		if len(r.Attestation) == 0 {
			return errors.New("a verified release arrived with no attestation")
		}
		if len(r.Keys.Keys) == 0 {
			return errors.New("a verified release arrived with no signing keys")
		}
	case StateUnsigned:
		if !j.Run.AllowUnsigned {
			return errors.New("the release is unsigned, and the run was not given --allow-unsigned")
		}
	case StateUnscanned:
		if !j.Run.AllowUnscanned {
			return errors.New("the release is unscanned, and the run was not given --allow-unscanned")
		}
	default:
		return fmt.Errorf("the release's state %q is not one a run takes", r.State)
	}
	if j.Run.Model.Provider == "" || j.Run.Model.Name == "" {
		return errors.New("the job names no model")
	}
	if !ValidKeyEnv(j.Run.Model.APIKeyEnv) {
		return fmt.Errorf("the model's key variable %q is not one a runhost takes: name it as the provider does, ending in _API_KEY, _KEY or _TOKEN, outside the AGENTRY_, DOCKER_, LD_, SSH_, GIT_ and XDG_ names", j.Run.Model.APIKeyEnv)
	}
	if j.Run.ModelKey == "" {
		return errors.New("the job carries no model key")
	}
	if j.Run.Timeout < 0 {
		return errors.New("the job's timeout is negative")
	}
	if j.Run.Adapter != "" {
		return fmt.Errorf("a runhost runs its own container harness; it does not take the adapter %q", j.Run.Adapter)
	}
	return nil
}

// Write writes the job document to w, length-prefixed.
func Write(w io.Writer, j *Job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(b) > MaxDocument {
		return fmt.Errorf("the job document is %d bytes, over the %d a runhost reads", len(b), MaxDocument)
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Read reads a job document written by Write and validates it.
func Read(r io.Reader) (*Job, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, fmt.Errorf("the job document did not arrive: %w", err)
	}
	if n == 0 || n > MaxDocument {
		return nil, fmt.Errorf("a job document of %d bytes; a runhost reads 1 to %d", n, MaxDocument)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("the job document was cut short: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var j Job
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("the job document is not readable: %w", err)
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return &j, nil
}
