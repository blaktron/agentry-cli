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
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
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
	// Adapter is the harness adapter the run takes, one of ImageAdapters;
	// empty is the policy's default. A runhost runs no host-process harness,
	// which would run outside gVisor: the device sends Claude Code's and
	// Codex's image adapters for them (RunhostAdapter).
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

	// Secrets are the values of the release's declared secrets and its
	// servers', by name, resolved on the device as a local run resolves them
	// (its MCP profile, the intake's secret answers, then the policy's secret
	// source). On the runhost they are written to the job's own secrets file,
	// which is the run's secret source and is deleted when the run ends.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Intake are the intake's answers as --intake takes them, by question
	// id. A file answer names its file in the payload (IntakeFiles) instead.
	Intake      map[string]string `json:"intake,omitempty"`
	IntakeFiles map[string]string `json:"intakeFiles,omitempty"`
	// SourceTokens are the access tokens of the run's input sources, by
	// question id, resolved on the device; on the runhost each reaches the
	// run's environment alone.
	SourceTokens map[string]string `json:"sourceTokens,omitempty"`
	// Inputs, Knowledge, Voice and ReportDesign name the payload's members:
	// each input a --file, each pack a --knowledge, the Writing Voice and
	// the report design. ReportTheme is a built-in theme's name.
	Inputs       []string `json:"inputs,omitempty"`
	Knowledge    []string `json:"knowledge,omitempty"`
	Voice        string   `json:"voice,omitempty"`
	ReportDesign string   `json:"reportDesign,omitempty"`
	ReportTheme  string   `json:"reportTheme,omitempty"`
	// Connections are the access tokens of the release's servers that sign
	// in by OAuth, refreshed on the device; never a refresh token.
	Connections map[string]Connection `json:"connections,omitempty"`
	// Destinations are where the run sends its result once it succeeds,
	// with their credentials, delivered from the runhost.
	Destinations []Destination `json:"destinations,omitempty"`
	// TargetHosts are the release's target hosts, by id, each answered in
	// Intake: the sign-in the device resolved and the known_hosts lines of
	// the keys it has accepted for the host. The runhost resolves the
	// address and checks the key it is offered against those lines alone.
	TargetHosts map[string]TargetHost `json:"targetHosts,omitempty"`
	// HostsAllow and HostsDeny are the device policy's networks a target
	// host's address may and may never be in, which the runhost holds the
	// address it resolves to.
	HostsAllow []string `json:"hostsAllow,omitempty"`
	HostsDeny  []string `json:"hostsDeny,omitempty"`
	// AllowVulnerableMCP is the person's own --allow-vulnerable-mcp.
	AllowVulnerableMCP bool `json:"allowVulnerableMcp,omitempty"`
	// Session makes the job an interactive session, driven over the job's
	// own input by the session protocol's commands, its events coming back
	// as frames.
	Session bool `json:"session,omitempty"`
	// Payload is the tar of the run's files that follows the document (and
	// the release, when the runhost asks for it); nil when there are none.
	Payload *Payload `json:"payload,omitempty"`
	// Desktop is the device's binding to its Agentry Desktop, for a release
	// that declares applications on the person's computer: the device bound
	// the run before it sent the job, and the runhost's run reaches the
	// device's link service through the link session the device keeps open
	// for the job. nil for a release that declares none.
	Desktop *Desktop `json:"desktop,omitempty"`
	// AllowScreen is the person's own --allow-screen.
	AllowScreen bool `json:"allowScreen,omitempty"`
	// Managed is what the sending device's managed settings say of the run:
	// the runhost has its own sign-in, so the organization's locks on the
	// person's device travel with the job, and the runhost applies them
	// beside its own. They can only refuse. Nil when the device holds none.
	Managed *managed.Narrowing `json:"managed,omitempty"`
}

// Desktop is a device's binding to Agentry Desktop, carried to the runhost:
// the run id the device bound, the run token Agentry Desktop issued, and the
// bound servers' tools and computer use as Agentry Desktop answered the bind.
// The token lives in the job's own 0600 file on the runhost while the run
// lasts, and is deleted with it.
type Desktop struct {
	Run   string        `json:"run"`
	Bound desktop.Bound `json:"bound"`
}

// MaxDesktopToken bounds the run token a binding carries.
const MaxDesktopToken = 512

// desktopIDRE is a run's or an application server's id in a binding: one
// word a file name or an argument may carry.
var desktopIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validate checks that the binding names a run, carries a token one line
// can hold, and binds at least one server or computer use.
func (d *Desktop) validate() error {
	if !desktopIDRE.MatchString(d.Run) {
		return fmt.Errorf("the desktop binding's run %q is not a run's id", d.Run)
	}
	if t := d.Bound.Token; t == "" || len(t) > MaxDesktopToken || !SecretValueOK(t) {
		return errors.New("the desktop binding carries no run token a job can hold")
	}
	if len(d.Bound.Servers) == 0 && d.Bound.Computer == nil {
		return errors.New("the desktop binding binds no application and no computer use")
	}
	for id := range d.Bound.Servers {
		if !desktopIDRE.MatchString(id) {
			return fmt.Errorf("the desktop binding names a server %q that cannot be one", id)
		}
	}
	return nil
}

// Connection is one OAuth server's access token for the run.
type Connection struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Issuer      string    `json:"issuer,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
}

// Destination is one destination the run delivers to: a webhook or a
// bucket, at a public address, with its credentials.
type Destination struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	URL           string `json:"url,omitempty"`
	Bucket        string `json:"bucket,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
	Region        string `json:"region,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
	Secret        string `json:"secret"`
	Authorization string `json:"authorization,omitempty"`
	AccessKeyID   string `json:"accessKeyId,omitempty"`
}

// TargetHost is one target host's sign-in and the host keys the device has
// accepted for it.
type TargetHost struct {
	// Known are known_hosts lines naming the host, as the device keeps them.
	Known []string `json:"known"`
	// Key is a private key's file, PEM, when the sign-in is a key; Password
	// otherwise. Passphrase opens the key; Sudo is the account's sudo
	// password.
	Key        string `json:"key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Password   string `json:"password,omitempty"`
	Sudo       string `json:"sudo,omitempty"`
}

// MaxKnownLines bounds the known_hosts lines one target host carries.
const MaxKnownLines = 32

// Payload names the tar of the run's files: its length and its digest.
type Payload struct {
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Model is the model a run uses: the device policy's, less where its key
// comes from, which on the runhost is always the run's environment.
type Model struct {
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	APIKeyEnv  string `json:"apiKeyEnv"`
	APIKeyKind string `json:"apiKeyKind,omitempty"`
	BaseURL    string `json:"baseUrl,omitempty"`
	// ContextWindow is the policy's model.contextWindow: the harness sizes a
	// model of the operator's own endpoint from it.
	ContextWindow int64    `json:"contextWindow,omitempty"`
	ProxyAllow    []string `json:"proxyAllow"`
}

// ModelOf is the job's model for the device policy's m.
func ModelOf(m policy.Model) Model {
	return Model{Provider: m.Provider, Name: m.Name, APIKeyEnv: m.APIKeyEnv, APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ContextWindow: m.ContextWindow, ProxyAllow: m.ProxyAllow}
}

// Policy is the model as the runhost's policy takes it: the key from the
// run's environment, or none for a model paid with Agentry credits or with
// a plan's sign-in the runhost keeps.
func (m Model) Policy() policy.Model {
	source := "env"
	if m.PaysWithCredits() || m.SignIn() {
		source = ""
	}
	return policy.Model{Provider: m.Provider, Name: m.Name, APIKeyEnv: m.APIKeyEnv, APIKeySource: source, APIKeyKind: m.APIKeyKind, BaseURL: m.BaseURL, ContextWindow: m.ContextWindow, ProxyAllow: m.ProxyAllow}
}

// SignIn says whether the model is paid by a plan's sign-in (a Claude
// subscription, a ChatGPT plan, a Copilot plan), which the runhost keeps
// itself: the job carries no key.
func (m Model) SignIn() bool { return policy.IsModelSignIn(m.APIKeyKind) }

// The adapters a run on a runhost may name: the default and the vendors'
// container harnesses, every one a container under gVisor.
const (
	AdapterDockerAgent = "docker-agent"
	AdapterClaudeImage = "claude-image"
	AdapterCodexImage  = "codex-image"
	AdapterCopilot     = "copilot-image"
)

// ImageAdapters are the adapters a runhost runs.
var ImageAdapters = []string{AdapterDockerAgent, AdapterClaudeImage, AdapterCodexImage, AdapterCopilot}

// RunhostAdapter is the adapter a runhost runs for the one a person asked
// for: Claude Code's and Codex's images for the installed claude and codex,
// which are host processes, and any image adapter as it is. mapped says the
// asked adapter was replaced.
func RunhostAdapter(asked string) (name string, mapped bool, err error) {
	switch asked {
	case "":
		return "", false, nil
	case "claude":
		return AdapterClaudeImage, true, nil
	case "codex":
		return AdapterCodexImage, true, nil
	}
	if slices.Contains(ImageAdapters, asked) {
		return asked, false, nil
	}
	return "", false, fmt.Errorf("a runhost runs its harness in a container under gVisor: %s, or claude and codex, which run as their images there; not %q", strings.Join(ImageAdapters, ", "), asked)
}

// PaysWithCredits says whether the model is paid with Agentry credits: the
// runhost opens its credits session with the token in its store, and the job
// carries no key.
func (m Model) PaysWithCredits() bool { return m.Provider == policy.ProviderAgentry }

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
	if err := policy.CheckContextWindow(j.Run.Model.ContextWindow); err != nil {
		return fmt.Errorf("the job's model: %w", err)
	}
	if j.Run.Model.PaysWithCredits() {
		if _, err := policy.AgentryRegistry(j.Run.Model.Policy()); err != nil {
			return fmt.Errorf("the job's credits model: %w", err)
		}
		if j.Run.Model.APIKeyEnv != "" || j.Run.Model.APIKeyKind != "" || j.Run.ModelKey != "" {
			return errors.New("a model paid with Agentry credits carries no key")
		}
	} else if j.Run.Model.SignIn() {
		if j.Run.Model.APIKeyEnv != "" || j.Run.ModelKey != "" {
			return errors.New("a model paid by a plan's sign-in carries no key: the runhost keeps the sign-in")
		}
		if want := policy.SignInProvider(j.Run.Model.APIKeyKind); j.Run.Model.Provider != want {
			return fmt.Errorf("a %s model's provider is %s, not %q", j.Run.Model.APIKeyKind, want, j.Run.Model.Provider)
		}
	} else {
		if !ValidKeyEnv(j.Run.Model.APIKeyEnv) {
			return fmt.Errorf("the model's key variable %q is not one a runhost takes: name it as the provider does, ending in _API_KEY, _KEY or _TOKEN, outside the AGENTRY_, DOCKER_, LD_, SSH_, GIT_ and XDG_ names", j.Run.Model.APIKeyEnv)
		}
		if j.Run.ModelKey == "" {
			return errors.New("the job carries no model key")
		}
	}
	if j.Run.Timeout < 0 {
		return errors.New("the job's timeout is negative")
	}
	if j.Run.Adapter != "" && !slices.Contains(ImageAdapters, j.Run.Adapter) {
		return fmt.Errorf("a runhost runs its harness in a container under gVisor (%s); it does not take the adapter %q", strings.Join(ImageAdapters, ", "), j.Run.Adapter)
	}
	return j.Run.validateCarried()
}

var (
	secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	intakeIDRE   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	themeRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// MaxSecret bounds one secret's value, and MaxSecrets their number.
const (
	MaxSecret  = 64 << 10
	MaxSecrets = 256
)

// SecretValueOK says whether v is a value the job's secrets file carries as
// it is: one line, with no NUL and no space at either end, which the file's
// reader would trim.
func SecretValueOK(v string) bool {
	return len(v) <= MaxSecret && !strings.ContainsAny(v, "\r\n\x00") && strings.TrimSpace(v) == v
}

// validateCarried checks what the run carries beside the release: every
// name is one a file name, a variable or an argument may carry, and every
// payload member it names is one the payload can hold.
func (r *Run) validateCarried() error {
	if err := r.Managed.Validate(); err != nil {
		return err
	}
	if r.Desktop != nil {
		if err := r.Desktop.validate(); err != nil {
			return err
		}
	}
	if len(r.Secrets) > MaxSecrets {
		return fmt.Errorf("the job carries %d secrets; a run takes at most %d", len(r.Secrets), MaxSecrets)
	}
	for name, v := range r.Secrets {
		if !secretNameRE.MatchString(name) || name == r.Model.APIKeyEnv {
			return errors.New("a secret's name is not one a run's secrets file carries")
		}
		if !SecretValueOK(v) {
			return fmt.Errorf("the secret %s is not one line a secrets file can carry as it is", name)
		}
	}
	for id, v := range r.Intake {
		if !intakeIDRE.MatchString(id) {
			return fmt.Errorf("the intake answer %q is not a question's id", id)
		}
		if strings.ContainsAny(v, "\x00") || len(v) > MaxDocument/4 {
			return fmt.Errorf("the intake answer %s is not one a run takes", id)
		}
		if _, dup := r.IntakeFiles[id]; dup {
			return fmt.Errorf("the intake question %s is answered twice", id)
		}
	}
	members := map[string]bool{}
	member := func(kind, name string) error {
		if !strings.HasPrefix(name, kind+"/") || !cleanMember(name) {
			return fmt.Errorf("%q is not a %s member of the run's files", name, kind)
		}
		if members[name] {
			return fmt.Errorf("the job names %s twice", name)
		}
		members[name] = true
		return nil
	}
	for id, name := range r.IntakeFiles {
		if !intakeIDRE.MatchString(id) {
			return fmt.Errorf("the intake answer %q is not a question's id", id)
		}
		if err := member("intake/"+id, name); err != nil {
			return err
		}
	}
	for id, tok := range r.SourceTokens {
		if !intakeIDRE.MatchString(id) || tok == "" || !SecretValueOK(tok) {
			return errors.New("an input source's token is not one a run takes")
		}
	}
	for _, name := range r.Inputs {
		if err := member("inputs", name); err != nil {
			return err
		}
		if strings.Count(name, "/") != 1 {
			return fmt.Errorf("the input %q is not one file or folder", name)
		}
	}
	for _, name := range r.Knowledge {
		if err := member("knowledge", name); err != nil {
			return err
		}
		if strings.Count(name, "/") != 1 {
			return fmt.Errorf("the pack %q is not one folder", name)
		}
	}
	if r.Voice != "" && r.Voice != "voice/VOICE.md" {
		return errors.New("the Writing Voice is not the payload's voice/VOICE.md")
	}
	if r.ReportDesign != "" && r.ReportDesign != "design/report-design.json" {
		return errors.New("the report design is not the payload's design/report-design.json")
	}
	if r.ReportTheme != "" && (!themeRE.MatchString(r.ReportTheme) || r.ReportDesign != "") {
		return fmt.Errorf("the report theme %q is not a built-in theme's name, or a design is given too", r.ReportTheme)
	}
	for server, c := range r.Connections {
		if server == "" || len(server) > 512 || strings.ContainsAny(server, "\x00\n") || c.AccessToken == "" || !SecretValueOK(c.AccessToken) {
			return errors.New("a connection's access token is not one a run can carry")
		}
	}
	for id, h := range r.TargetHosts {
		if !intakeIDRE.MatchString(id) {
			return fmt.Errorf("the target host %q is not a question's id", id)
		}
		if _, ok := r.Intake[id]; !ok {
			return fmt.Errorf("the target host %s is not answered", id)
		}
		if len(h.Known) == 0 || len(h.Known) > MaxKnownLines {
			return fmt.Errorf("the target host %s carries %d host keys; a run takes 1 to %d", id, len(h.Known), MaxKnownLines)
		}
		for _, l := range h.Known {
			if len(l) > 16<<10 || strings.ContainsAny(l, "\r\n\x00") {
				return fmt.Errorf("a host key of the target host %s is not one known_hosts line", id)
			}
		}
		if (h.Key == "") == (h.Password == "") {
			return fmt.Errorf("the target host %s carries no sign-in, or two", id)
		}
		if len(h.Key) > 64<<10 || strings.ContainsAny(h.Key, "\x00") {
			return fmt.Errorf("the target host %s's key is not a key file", id)
		}
		for _, v := range []string{h.Passphrase, h.Password, h.Sudo} {
			if !SecretValueOK(v) {
				return fmt.Errorf("a sign-in of the target host %s is not one line", id)
			}
		}
	}
	if len(r.Destinations) > 16 {
		return errors.New("a run delivers to at most 16 destinations")
	}
	for _, d := range r.Destinations {
		if d.Name == "" || d.Secret == "" || !SecretValueOK(d.Secret) || !SecretValueOK(d.Authorization) || !SecretValueOK(d.AccessKeyID) {
			return fmt.Errorf("the destination %q is not one a run can deliver to", d.Name)
		}
	}
	if len(members) > 0 && r.Payload == nil {
		return errors.New("the job names files and carries none")
	}
	if p := r.Payload; p != nil && (p.Size <= 0 || !digestRE.MatchString(p.Digest)) {
		return errors.New("the job's files are named by no size or digest")
	}
	return nil
}

// cleanMember says whether name is a clean relative slash path.
func cleanMember(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || len(name) > 4096 {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
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
