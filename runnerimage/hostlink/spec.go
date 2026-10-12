// Package hostlink is the runner's host link (plan runtime-hosts.md §4–§6,
// D3–D9, D19; agentry-cli#861): the MCP server through which an agent works
// a target host, a machine the person named when the run started. It runs
// in its own container, on the agentry-hostlink image (OpenSSH, DR-91) with
// this binary copied in, launched by the run's gateway like any declared
// server, behind one relay per target host to the address and port the
// person named. It holds each host's credential in files bound into it
// alone, and makes every SSH session itself: the agent never does, and
// neither the agent container nor the tools server gains a route out.
//
// Stdlib only (DR-2), like everything else in this binary.
package hostlink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

// SpecVersion is the hostspec.json version this server implements.
const SpecVersion = "1"

// Where the host link finds what the engine gives it, inside its container.
const (
	// SpecMount is the plan's hostspec.json, read-only.
	SpecMount = "/run/agentry/hostspec.json"
	// HostsMount is the run's cfg/hosts, read-only: one folder per target
	// host (its credential, its known_hosts, target.json) and passwd.
	HostsMount = "/run/agentry/hosts"
	// LogMount is the run's cfg/hostlog, writable: one log a target host,
	// LogFile(id), which the engine copies into the receipts.
	LogMount = "/run/agentry/hostlog"
	// FilesMount is the folder host_put copies from and host_get copies
	// into, writable; the tools server sees the same folder at FilesPath,
	// inside the agent's scratch folder.
	FilesMount = "/files"
	// FilesPath is that folder as the agent and the tools server name it.
	FilesPath = "/scratch/hosts"
	// PasswdFile is the passwd file in HostsMount that the engine binds
	// over /etc/passwd: ssh refuses to start for a uid with no entry
	// (DR-91), and the host link runs as the person's uid.
	PasswdFile = "passwd"
	// AskpassProgram is the name this binary answers to as OpenSSH's
	// SSH_ASKPASS (a link to it in the image): it prints the one file
	// AskpassFileEnv names, below HostsMount, and nothing else.
	AskpassProgram = "/usr/local/bin/agentry-askpass"
	AskpassFileEnv = "AGENTRY_ASKPASS_FILE"
)

// The files in a target host's folder (HostsMount/<id>/), each 0600.
const (
	TargetFile     = "target.json"
	KeyFile        = "key"
	PasswordFile   = "password"
	PassphraseFile = "passphrase"
	SudoFile       = "sudo"
	KnownHostsFile = "known_hosts"
)

// HostKeyAlias is the name every target host's key is kept under in its
// own known_hosts: ssh dials the relay, never the host, so the key is
// checked under this alias (HostKeyAlias), and each host's file holds only
// its own key.
const HostKeyAlias = "agentry-target"

// The ways a target host is signed in to.
const (
	AuthKey      = "key"
	AuthPassword = "password"
)

// The host link's tools (plan runtime-hosts.md §5): the one-shot five
// (agentry-cli#861) and the shell's four (D18, agentry-cli#862).
const (
	ToolRun   = "host_run"
	ToolRead  = "host_read"
	ToolWrite = "host_write"
	ToolPut   = "host_put"
	ToolGet   = "host_get"

	ToolShellOpen  = "host_shell_open"
	ToolShellSend  = "host_shell_send"
	ToolShellRead  = "host_shell_read"
	ToolShellClose = "host_shell_close"
)

// Tools are the host link's tools, in the order it lists them. Their
// results reach the agent unchanged (D9), past the gateway's secret block
// and the harness's scrub, so a client that scrubs tool output by name
// leaves these out.
var Tools = []string{ToolRun, ToolRead, ToolWrite, ToolPut, ToolGet, ToolShellOpen, ToolShellSend, ToolShellRead, ToolShellClose}

// ServerName is the host link's name in the gateway's catalog.
const ServerName = "agentry-hostlink"

// Spec is hostspec.json: what the compiled plan says of the host link. It
// names the target hosts by their intake question's id and holds no
// address, user, port or credential, which are the run's (Target).
type Spec struct {
	SpecVersion string `json:"specVersion"`
	// Hosts are the release's target hosts, by question id, in intake
	// order.
	Hosts []string `json:"hosts"`
	// MaxResultBytes and MaxResultLines are the harness's result limit,
	// as the tools server's (toolsrv.Limits).
	MaxResultBytes int64 `json:"maxResultBytes,omitempty"`
	MaxResultLines int   `json:"maxResultLines,omitempty"`
}

// Target is target.json: one target host as the engine resolved and
// checked it at run start.
type Target struct {
	// ID is the intake question's id.
	ID string `json:"id"`
	// Named is the host as the person wrote it (a name or an address).
	Named string `json:"named"`
	// Address is the IPv4 address the person's machine resolved it to,
	// which the relay dials, and the relay's own alias on the host link's
	// network.
	Address string `json:"address"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	// HostKeyAlgorithm is what ssh offers to accept for the one key
	// known_hosts holds: its type (ssh-ed25519, ecdsa-sha2-nistp256), or
	// for an RSA key "rsa-sha2-512,rsa-sha2-256".
	HostKeyAlgorithm string `json:"hostKeyAlgorithm"`
	// Auth is AuthKey or AuthPassword.
	Auth string `json:"auth"`
	// Passphrase says the key has one, in PassphraseFile.
	Passphrase bool `json:"passphrase,omitempty"`
	// Sudo says the person gave a sudo password, in SudoFile.
	Sudo bool `json:"sudo,omitempty"`
}

var (
	idRE       = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	userRE     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	hostAlgoRE = regexp.MustCompile(`^[a-z0-9@.-]{1,64}(,[a-z0-9@.-]{1,64}){0,3}$`)
)

// ValidID says whether id is an intake question's id, the shape a target
// host's folder and log are named by.
func ValidID(id string) bool { return idRE.MatchString(id) }

// ValidUser says whether user is an account name the host link passes to
// ssh: a name, never an option or a path.
func ValidUser(user string) bool { return userRE.MatchString(user) }

// Check refuses a target the engine should never have written: every value
// reaches ssh's arguments, so each is held to its form here as well.
func (t Target) Check() error {
	if !ValidID(t.ID) {
		return fmt.Errorf("target host id %q is not a question id", t.ID)
	}
	a, err := netip.ParseAddr(t.Address)
	if err != nil || !a.Is4() {
		return fmt.Errorf("target host %s: address %q is not an IPv4 address", t.ID, t.Address)
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("target host %s: port %d", t.ID, t.Port)
	}
	if !ValidUser(t.User) {
		return fmt.Errorf("target host %s: user %q is not an account name", t.ID, t.User)
	}
	if !hostAlgoRE.MatchString(t.HostKeyAlgorithm) {
		return fmt.Errorf("target host %s: host key type %q", t.ID, t.HostKeyAlgorithm)
	}
	if t.Auth != AuthKey && t.Auth != AuthPassword {
		return fmt.Errorf("target host %s: sign-in %q is neither a key nor a password", t.ID, t.Auth)
	}
	if t.Passphrase && t.Auth != AuthKey {
		return fmt.Errorf("target host %s: a passphrase without a key", t.ID)
	}
	return nil
}

// LogFile is a target host's log under LogMount: one JSON line a call.
func LogFile(id string) string { return "host-" + id + ".log" }

// ShellLogFile is the transcript of a target host's shell session n under
// LogMount: what was typed and what the shell printed, as text, masked of
// the run's own sign-ins (plan runtime-hosts.md invariant 7, D19).
func ShellLogFile(id string, n int) string { return fmt.Sprintf("host-%s-shell-%d.log", id, n) }

// ShellLogRE matches a shell transcript's name, giving the host's id.
var ShellLogRE = regexp.MustCompile(`^host-([a-z0-9_]{1,64})-shell-([1-9][0-9]{0,5})\.log$`)

// LogEntry is one line of a target host's log (plan runtime-hosts.md
// invariant 7): the tool, the command or the paths, the exit status and
// the byte counts. The engine masks it with the run's redactor before it
// is a receipt.
type LogEntry struct {
	Time    string `json:"time"`
	Tool    string `json:"tool"`
	Command string `json:"command,omitempty"`
	Sudo    bool   `json:"sudo,omitempty"`
	Path    string `json:"path,omitempty"`
	To      string `json:"to,omitempty"`
	Exit    *int   `json:"exit,omitempty"`
	Stdout  int64  `json:"stdoutBytes,omitempty"`
	Stderr  int64  `json:"stderrBytes,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	// Session is a shell call's session, and Keys the keys it typed after
	// its text (Command), by name: the sudo password is "sudo-password".
	Session int      `json:"session,omitempty"`
	Keys    []string `json:"keys,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ParseSpec reads hostspec.json, refusing a version it does not implement,
// an unknown field, or a host id not in the question id form.
func ParseSpec(b []byte) (*Spec, error) {
	var s Spec
	if err := strictJSON(b, &s); err != nil {
		return nil, fmt.Errorf("host link spec: %w", err)
	}
	if s.SpecVersion != SpecVersion {
		return nil, fmt.Errorf("host link spec version %q not supported (this server implements %q)", s.SpecVersion, SpecVersion)
	}
	if len(s.Hosts) == 0 {
		return nil, errors.New("host link spec names no target host")
	}
	seen := map[string]bool{}
	for _, id := range s.Hosts {
		if !ValidID(id) || seen[id] {
			return nil, fmt.Errorf("host link spec: target host id %q", id)
		}
		seen[id] = true
	}
	return &s, nil
}

// ParseTarget reads target.json the same way.
func ParseTarget(b []byte) (Target, error) {
	var t Target
	if err := strictJSON(b, &t); err != nil {
		return Target{}, err
	}
	return t, t.Check()
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one value")
	}
	return nil
}
