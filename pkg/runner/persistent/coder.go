package persistent

// The Coding agent's roster: beside its folders, the parts of its grant,
// each a switch, on by default, that the person agrees to before its first
// segment: any program in its toolchain image, open egress, the browser on
// any public site, the dev server they saved for it, and the ports they
// publish from its tools server on this computer. Apply turns them
// into the segment's spec.CoderGrant, readying the dev server as it does.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/blaktron/agentry-cli/pkg/runner/osvdb"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// The Coding agent's entries.
const (
	EntryCommands = "commands"
	EntryEgress   = "egress"
	EntryBrowser  = "browser"
	EntryDevHost  = "devhost"
	// EntryDependencyScan is dependency_scan, OSV-Scanner offline.
	EntryDependencyScan = "dependency-scan"
)

// The kinds of the Coding agent's entries.
const (
	KindTools EntryKind = "tools"
	KindHost  EntryKind = "host"
	KindPort  EntryKind = "port"
)

// Port is one of the Coding agent's published ports, as config.yaml keeps
// it: a name the person gives it and the number its programs serve on,
// published on this computer at 127.0.0.1 and the same number.
type Port struct {
	Name string `yaml:"name" json:"name"`
	Port int    `yaml:"port" json:"port"`
}

// MaxPorts bounds the Coding agent's published ports.
const MaxPorts = 8

var portNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// check refuses a port that is not one: a name that is not lower-case
// letters, digits and hyphens, or a number a program cannot serve on
// without privilege.
func (p Port) check() error {
	if !portNameRE.MatchString(p.Name) {
		return fmt.Errorf("the port name %q is not 1 to 32 lower-case letters, digits and hyphens", p.Name)
	}
	if p.Port < 1024 || p.Port > 65535 {
		return fmt.Errorf("the port %d is not from 1024 to 65535", p.Port)
	}
	return nil
}

// Address is where the port is published on this computer.
func (p Port) Address() string { return fmt.Sprintf("127.0.0.1:%d", p.Port) }

// PortEntryID is the roster id of a published port.
func PortEntryID(name string) string { return "port:" + name }

// DevHost is the Coding agent's dev server, as config.yaml keeps it: where it
// is and as whom, and the host key the person trusted. Its sign-in is in the
// system keychain, saved when the person configured it, never here.
type DevHost struct {
	// Host is user@host[:port].
	Host string `yaml:"host" json:"host"`
	// HostKey is the SHA256 fingerprint of the host key the person
	// trusted: a segment that finds another refuses.
	HostKey string `yaml:"hostKey" json:"hostKey"`
}

var fingerprintRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// check refuses a dev server that is not one: a host that is not
// user@host[:port], or a fingerprint that is not ssh's SHA256 form.
func (d DevHost) check() error {
	a, err := targethost.ParseAnswer(d.Host)
	if err != nil {
		return fmt.Errorf("the dev server: %w", err)
	}
	if a.String() != d.Host {
		return fmt.Errorf("the dev server %q is not written as user@host[:port] (%s)", d.Host, a)
	}
	if !fingerprintRE.MatchString(d.HostKey) {
		return fmt.Errorf("the dev server's host key %q is not a SHA256 fingerprint", d.HostKey)
	}
	return nil
}

// coderEntries are the Coding agent's grant entries for its config.
func coderEntries(c Config) []RosterEntry {
	out := []RosterEntry{
		{ID: EntryCommands, Kind: KindTools, Name: "Programs", GrantedBy: GrantedByBuiltin,
			Line: "Runs any program it chooses in its toolchain image (build tools, Git, curl, Python 3, Go, Node and npm), in its folders, as you: it builds and runs software, which is riskier than an agent that only reads and writes"},
		{ID: EntryEgress, Kind: KindTools, Name: "Open egress", GrantedBy: GrantedByBuiltin,
			Line: "Reaches any address from its tools and the programs it runs (open egress): the internet, and also your local network and this computer's own services"},
		{ID: EntryBrowser, Kind: KindTools, Name: "Browser", GrantedBy: GrantedByBuiltin,
			Line: "Drives a headless browser to any public website, through the runner's browse proxy"},
		{ID: EntryDependencyScan, Kind: KindTools, Name: "Dependency scan (OSV-Scanner)", GrantedBy: GrantedByBuiltin,
			Line: "Scans a project's dependencies for known vulnerabilities with OSV-Scanner, offline, against OSV's databases, which Agentry downloads to this computer once a day from " + osvdb.Host + " (about 300 MB), while its programs are on"},
	}
	if d := c.DevHost; d != nil {
		out = append(out, RosterEntry{ID: EntryDevHost, Kind: KindHost, Name: "Dev server " + d.Host, GrantedBy: GrantedByYou,
			Line: "Reaches your dev server " + d.Host + " over SSH through the host link, as that account, with the sign-in saved in your keychain; its host key " + d.HostKey + " is pinned"})
	}
	for _, p := range c.Ports {
		out = append(out, RosterEntry{ID: PortEntryID(p.Name), Kind: KindPort, Name: fmt.Sprintf("Port %s (%s)", p.Name, p.Address()), GrantedBy: GrantedByYou,
			Line: fmt.Sprintf("Publishes what it serves on its port %d at http://%s on this computer, where your browser and any program on this computer can reach it", p.Port, p.Address())})
	}
	return out
}

// HostReady readies the Coding agent's dev server for a segment: its
// address, its host key held to the pinned one, its sign-in from the
// keychain. No one is asked anything: a supervisor has no terminal.
type HostReady func(ctx context.Context, d DevHost) (*targethost.Host, error)

// ReadyDevHost is the HostReady of a supervisor under the runner home home
// and the policy pol: the policy's host lists, the runner's and the person's
// known_hosts files, the sign-in the person saved.
func ReadyDevHost(home string, pol *policy.Policy) HostReady {
	return func(ctx context.Context, d DevHost) (*targethost.Host, error) {
		if pol == nil {
			return nil, errors.New("the dev server is readied under a runner policy, and there is none")
		}
		a, err := targethost.ParseAnswer(d.Host)
		if err != nil {
			return nil, err
		}
		return targethost.Prepare(ctx, spec.DevHostID, a, targethost.Options{
			Hosts: pol.Hosts, KnownHosts: targethost.DefaultKnownHostsFiles(home),
			Getenv: os.Getenv, Pin: d.HostKey,
		})
	}
}

// coderGrant is the segment's spec.CoderGrant from the roster, and the dev
// server readied when it is on.
func coderGrant(ctx context.Context, c Config, on func(string) bool, ready HostReady) (*spec.CoderGrant, *targethost.Host, error) {
	g := &spec.CoderGrant{Commands: on(EntryCommands), OpenEgress: on(EntryEgress), Browser: on(EntryBrowser)}
	g.DependencyScan = g.Commands && on(EntryDependencyScan)
	for _, p := range c.Ports {
		if on(PortEntryID(p.Name)) {
			g.Ports = append(g.Ports, spec.CoderPort{Name: p.Name, Port: p.Port})
		}
	}
	if c.DevHost == nil || !on(EntryDevHost) {
		return g, nil, nil
	}
	if ready == nil {
		return nil, nil, errors.New("the Coding agent has a dev server on, and this client cannot ready it")
	}
	h, err := ready(ctx, *c.DevHost)
	if err != nil {
		return nil, nil, fmt.Errorf("the dev server %s: %w", c.DevHost.Host, err)
	}
	g.DevHost = true
	return g, h, nil
}
