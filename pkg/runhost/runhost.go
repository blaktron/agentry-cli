// Package runhost holds what a remote runhost is made of on both sides of
// the SSH connection that reaches it: where things live on the server, the
// device's record of each runhost, the device's keys, the line that installs
// a run key behind its forced command, and the frames the server's service
// answers in.
//
// A remote runhost is a Linux server the person owns, set up by the CLI or the
// desktop over SSH, that runs that person's runs. On the server a dedicated
// user, agentry, runs the `agentry runhost service` as a systemd user service.
// The service listens on an owner-only Unix socket and nothing else. A device
// reaches it with its own run key, which the server's authorized_keys holds
// with `restrict` and a forced `agentry runhost serve`: whatever command the
// device asks for, sshd runs serve, which relays the session to the socket
// carrying the asked verb. So SSH is the only way in, the run key opens no
// shell and forwards nothing, and the server opens no network port.
package runhost

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
)

// The server's layout. Everything the service owns lives under the agentry
// user's home, so the service can replace its own CLI and runsc without
// root.
const (
	// User is the server account that runs the service.
	User = "agentry"
	// Home is that account's home directory.
	Home = "/var/lib/agentry"
	// BinDir holds the links to the CLI and runsc in use.
	BinDir = Home + "/bin"
	// CLIPath is the CLI the service, serve and the login shell run. It is
	// also the account's login shell (see LoginShellCommand).
	CLIPath = BinDir + "/agentry"
	// RunscPath is the runsc the Docker daemon's runtimes name.
	RunscPath = BinDir + "/runsc"
	// CLIDir holds each installed CLI release, one directory per version.
	CLIDir = Home + "/cli"
	// GVisorDir holds each installed gVisor release.
	GVisorDir = Home + "/gvisor"
	// StateDir is the service's own state: the runhost's configuration and
	// the paired devices.
	StateDir = Home + "/runhost"
	// ConfigFile is the runhost's configuration (ServerConfig).
	ConfigFile = StateDir + "/config.json"
	// DevicesFile lists the paired devices (Device).
	DevicesFile = StateDir + "/devices.json"
	// AuthorizedKeys is where the run keys are installed.
	AuthorizedKeys = Home + "/.ssh/authorized_keys"
	// UnitName is the systemd user unit of the service.
	UnitName = "agentry-runhost.service"
	// UnitFile is that unit's file.
	UnitFile = Home + "/.config/systemd/user/" + UnitName
	// SocketName is the service's socket under the user's runtime directory.
	SocketName = "agentry-runhost.sock"
	// RunscRuntime and RunscUDSRuntime are the Docker daemon's two gVisor
	// registrations: every run container, and the gateway, which reaches a
	// host Unix socket and so needs --host-uds=open.
	RunscRuntime    = "runsc"
	RunscUDSRuntime = "runsc-uds"
)

// ServeCommand is the forced command of every run key.
const ServeCommand = CLIPath + " runhost serve"

// SocketPath is the service's socket for the agentry user's uid.
func SocketPath(uid int) string {
	return path.Join("/run/user", strconv.Itoa(uid), SocketName)
}

// CLIVersionDir is where release version's CLI is installed.
func CLIVersionDir(version string) string { return CLIDir + "/" + version }

// GVisorReleaseDir is where a gVisor release is installed.
func GVisorReleaseDir(release string) string { return GVisorDir + "/" + release }

// Verbs a run key may ask for. sshd hands serve the asked command in
// SSH_ORIGINAL_COMMAND, and serve relays only these.
var Verbs = []string{"version", "status", "doctor", "job", "attach", "cancel", "collect", "update", "store", "schedule", "agent", "sign-in"}

// IsVerb says whether v is one of Verbs.
func IsVerb(v string) bool {
	for _, w := range Verbs {
		if w == v {
			return true
		}
	}
	return false
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName says whether name can name a runhost: lower-case letters,
// digits and inner hyphens, at most 32 characters. It becomes part of
// keychain accounts and of the run setup's labels.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("runhost name %q: use 1 to 32 lower-case letters, digits and inner hyphens", name)
	}
	return nil
}

// ServerConfig is the runhost's configuration, kept by the service.
type ServerConfig struct {
	// Name is the name the setting-up device gave the runhost.
	Name string `json:"name"`
	// Owner is who the runhost belongs to.
	Owner Owner `json:"owner"`
	// UnsafeMode is whether runs here are compiled as Unsafe Mode runs. It is
	// changed only with the admin sign-in.
	UnsafeMode bool `json:"unsafeMode"`
	// InstalledDocker is whether setup installed Docker Engine, so that
	// remove may offer to take it away again.
	InstalledDocker bool `json:"installedDocker"`
	// GVisor is the gVisor release installed.
	GVisor string `json:"gvisor"`
	// SetUpAt is when setup finished, RFC 3339.
	SetUpAt string `json:"setUpAt"`
}

// Owner is who a runhost belongs to: the Agentry account the setting-up
// device was signed in to, or, set up signed out, the paired devices alone.
type Owner struct {
	// Kind is "account" or "devices".
	Kind string `json:"kind"`
	// Registry and Handle name the account, for Kind "account".
	Registry string `json:"registry,omitempty"`
	Handle   string `json:"handle,omitempty"`
}

// Owner kinds.
const (
	OwnerAccount = "account"
	OwnerDevices = "devices"
)

// String says the owner as a person reads it.
func (o Owner) String() string {
	if o.Kind == OwnerAccount {
		return o.Handle + " on " + o.Registry
	}
	return "the paired devices"
}

// Device is one paired device, as the server keeps it.
type Device struct {
	// ID is the device's own id, the run key's comment.
	ID string `json:"id"`
	// Label is the device's host name, for a person to tell devices apart.
	Label string `json:"label"`
	// RunKey is the run key's public half, in authorized_keys form.
	RunKey string `json:"runKey"`
	// SealKey is the sealing key's public half, base64.
	SealKey string `json:"sealKey"`
	// PairedAt is when the device was paired, RFC 3339.
	PairedAt string `json:"pairedAt"`
}

// Devices is the devices file.
type Devices struct {
	Devices []Device `json:"devices"`
}

// AuthorizedKeyLine is the authorized_keys line that installs a run key: the
// key may run nothing but serve, and restrict takes away the pty, port,
// agent and X11 forwarding and the user's rc file.
func AuthorizedKeyLine(publicKey, deviceID string) string {
	return `restrict,command="` + ServeCommand + `" ` + publicKey + " " + DeviceComment(deviceID)
}

// DeviceComment is the comment a device's run key carries.
func DeviceComment(deviceID string) string { return "agentry-runhost-device:" + deviceID }
