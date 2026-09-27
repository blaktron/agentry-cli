// Package installed is the shared install-state record and the capability
// diff (§11 install row: "show capability summary (and DIFF vs currently
// installed version, requiring confirm on widened capabilities)").
//
// The registry knows what an agent declares; only the machine knows what it
// previously ACCEPTED. Without a local record, every reinstall re-asks the
// same question from scratch and an upgrade that quietly adds egress to a new
// host looks exactly like the install the user already approved. This file is
// that record.
//
// Location: ~/.config/agentry/installed.yaml — alongside config.yaml rather
// than under ~/.local/state. AGENTRY_CONFIG_DIR already relocates everything
// an agentry client keeps on disk (config, pinned signing keys, cached rule
// bundles); a second root would be a second thing to point at a container
// volume, and nothing here is large or high-churn enough to earn that.
//
// EVERY client (CLI, desktop) must read and write this same file, so a
// capability diff on upgrade works regardless of which client did the
// original install. The same clients hold a downloaded archive's own
// manifest to the contract they asked consent on (ArchiveWidenings), so
// the rule for that lives here too.
package installed

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// CapSnapshot is the on-disk form of a §5.4 capability contract.
//
// It is deliberately NOT manifest.Capabilities: that type's union fields
// (Egress, FSAccess) carry their shape in unexported-by-tag form with custom
// JSON codecs, so persisting it as YAML would silently drop the very fields
// the diff turns on. A flat, explicit record is also a format this file owns
// and can keep stable while the manifest evolves.
type CapSnapshot struct {
	// EgressHosts lists declared egress hosts; empty means none/undeclared.
	EgressHosts []string `yaml:"egressHosts,omitempty" json:"egressHosts,omitempty"`
	// EgressNone records an explicit `egress: none` (vs. not declared).
	EgressNone bool `yaml:"egressNone,omitempty" json:"egressNone,omitempty"`
	// FSRead/FSWrite are the declared modes ("", none, workspace, paths).
	FSRead       string   `yaml:"fsRead,omitempty" json:"fsRead,omitempty"`
	FSReadPaths  []string `yaml:"fsReadPaths,omitempty" json:"fsReadPaths,omitempty"`
	FSWrite      string   `yaml:"fsWrite,omitempty" json:"fsWrite,omitempty"`
	FSWritePaths []string `yaml:"fsWritePaths,omitempty" json:"fsWritePaths,omitempty"`
	// ExecCommands lists declared subprocess commands.
	ExecCommands []string `yaml:"execCommands,omitempty" json:"execCommands,omitempty"`
	// Secrets lists declared environment-variable NAMES (never values).
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// DataAccess lists declared semantic data categories.
	DataAccess []string `yaml:"dataAccess,omitempty" json:"dataAccess,omitempty"`
	// MCPServers lists declared MCP servers as "source:package@version"
	// (RUNNER-PLAN-3) — a new or changed server is a widening the consent
	// gate must re-ask about.
	MCPServers []string `yaml:"mcpServers,omitempty" json:"mcpServers,omitempty"`
	// MCPServerEnv is each declared server's plain env (name → literal
	// value), keyed like MCPServers. Values are configuration, not
	// secrets, so they are stored and shown: a changed value is a change
	// to what the server receives and re-asks for consent. Absent in
	// records written before env existed, which reads as "no env".
	MCPServerEnv map[string]map[string]string `yaml:"mcpServerEnv,omitempty" json:"mcpServerEnv,omitempty"`
	// Knowledge lists the knowledge packs the release ships in its archive
	// (the manifest's knowledge.ships, as manifest.Contract carries them):
	// a new one is text entering the agent's context, a widening the
	// consent gate re-asks about. Absent in records written before shipped
	// knowledge existed, which reads as "ships none".
	Knowledge []string `yaml:"knowledge,omitempty" json:"knowledge,omitempty"`
}

// Snapshot flattens a manifest capability block for storage.
func Snapshot(c manifest.Capabilities) CapSnapshot {
	s := CapSnapshot{
		EgressHosts:  append([]string(nil), c.Network.Egress.Hosts...),
		EgressNone:   c.Network.Egress.None,
		FSRead:       string(c.Filesystem.Read.Mode),
		FSReadPaths:  append([]string(nil), c.Filesystem.Read.Paths...),
		FSWrite:      string(c.Filesystem.Write.Mode),
		FSWritePaths: append([]string(nil), c.Filesystem.Write.Paths...),
		ExecCommands: append([]string(nil), c.Exec.Commands...),
		Secrets:      append([]string(nil), c.Secrets...),
	}
	for _, srv := range c.MCPServers {
		id := serverID(srv)
		s.MCPServers = append(s.MCPServers, id)
		if len(srv.Env) > 0 {
			if s.MCPServerEnv == nil {
				s.MCPServerEnv = map[string]map[string]string{}
			}
			s.MCPServerEnv[id] = maps.Clone(srv.Env)
		}
	}
	for _, d := range c.DataAccess {
		s.DataAccess = append(s.DataAccess, string(d))
	}
	s.Knowledge = slices.Clone(c.Knowledge)
	return s
}

// capabilities is Snapshot's inverse, for the diff only: the stored
// contract as the manifest type, so the one widening rule, the core's
// manifest.DiffCapabilities, can compare two snapshots (agentry-cli#201).
// For any record Snapshot wrote, Snapshot(s.capabilities()) is s. It is
// lossy — a server's tools, secrets and egress are not in the snapshot and
// come back empty — which is why it is not exported as a contract.
func (s CapSnapshot) capabilities() manifest.Capabilities {
	c := manifest.Capabilities{
		Network: manifest.Network{Egress: manifest.Egress{
			None:  s.EgressNone,
			Hosts: slices.Clone(s.EgressHosts),
		}},
		Filesystem: manifest.Filesystem{
			Read:  manifest.FSAccess{Mode: manifest.FSMode(s.FSRead), Paths: slices.Clone(s.FSReadPaths)},
			Write: manifest.FSAccess{Mode: manifest.FSMode(s.FSWrite), Paths: slices.Clone(s.FSWritePaths)},
		},
		Exec:      manifest.ExecCapability{Commands: slices.Clone(s.ExecCommands)},
		Secrets:   slices.Clone(s.Secrets),
		Knowledge: slices.Clone(s.Knowledge),
	}
	for _, d := range s.DataAccess {
		c.DataAccess = append(c.DataAccess, manifest.DataAccess(d))
	}
	for _, id := range s.MCPServers {
		srv := serverFromID(id)
		srv.Env = maps.Clone(s.MCPServerEnv[id])
		c.MCPServers = append(c.MCPServers, srv)
	}
	return c
}

// unparsedServer prefixes the name of a server whose recorded id did not
// parse. No package name holds a NUL, so its key matches no server a
// release declares.
const unparsedServer = "\x00unparsed:"

// serverFromID reads a "source:package@version" id back into the server
// the core keys by SourceID()+"@"+Version, splitting at the last "@" — a
// scoped npm name begins with one. An id Snapshot never writes (a
// hand-edited record) fails safe: it becomes a server no release declares,
// so the release's own server reads as added and the gate asks again, as
// the raw-string comparison this replaced did.
func serverFromID(id string) manifest.MCPServer {
	for _, src := range []string{"npm:", "pypi:"} {
		rest, ok := strings.CutPrefix(id, src)
		if !ok {
			continue
		}
		at := strings.LastIndex(rest, "@")
		if at <= 0 {
			break
		}
		srv := manifest.MCPServer{Version: rest[at+1:]}
		if src == "npm:" {
			srv.NPM = rest[:at]
		} else {
			srv.PyPI = rest[:at]
		}
		return srv
	}
	return manifest.MCPServer{PyPI: unparsedServer + id}
}

// Record is one installed (registry, agent, target, dir) tuple.
type Record struct {
	Registry     string      `yaml:"registry" json:"registry"`
	Namespace    string      `yaml:"namespace" json:"namespace"`
	Name         string      `yaml:"name" json:"name"`
	Target       string      `yaml:"target" json:"target"`
	Dir          string      `yaml:"dir" json:"dir"`
	Version      string      `yaml:"version" json:"version"`
	InstalledAt  time.Time   `yaml:"installedAt" json:"installedAt"`
	Capabilities CapSnapshot `yaml:"capabilities" json:"capabilities"`
	// Managed is true when the CLIENT chose Dir from the target's default.
	// Only a managed directory may be deleted on uninstall or cleared on
	// reinstall — a directory the user named is theirs and may hold files no
	// agentry client put there. Optional so files written before this field
	// existed still load; absent means NOT managed (the safe reading).
	Managed bool `yaml:"managed,omitempty" json:"managed,omitempty"`
}

// State is ~/.config/agentry/installed.yaml.
type State struct {
	Records []Record `yaml:"records,omitempty"`
}

// StatePath returns the install-state path inside the config dir.
func StatePath() (string, error) {
	dir, err := configdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "installed.yaml"), nil
}

// Load reads the install manifest; a missing file is empty state.
//
// A CORRUPT file is reported as an error so the caller can decide: the CLI
// warns and treats it as empty, because refusing to install over a truncated
// bookkeeping file would turn a cosmetic problem into an outage, and the
// worst case is one extra confirmation prompt.
func Load() (State, error) {
	var st State
	path, err := StatePath()
	if err != nil {
		return st, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // fixed name under the config dir
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}

// Save writes the manifest (0600: it names local paths).
func Save(st State) error {
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := yaml.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode install state: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Key identifies one installation. The DIRECTORY is part of the key:
// the same agent installed as a claude-skill in ~/.claude/skills and as an
// mcp server in a project are two independent installations with independent
// capability histories, and a diff that conflated them would ask the wrong
// question about the wrong files.
type Key struct {
	Registry  string `json:"registry" yaml:"registry"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Name      string `json:"name" yaml:"name"`
	Target    string `json:"target" yaml:"target"`
	Dir       string `json:"dir" yaml:"dir"`
}

func (k Key) matches(r Record) bool {
	return strings.TrimRight(r.Registry, "/") == strings.TrimRight(k.Registry, "/") &&
		r.Namespace == k.Namespace && r.Name == k.Name &&
		r.Target == k.Target && r.Dir == k.Dir
}

// Find returns the record for a key, or nil.
func (st *State) Find(k Key) *Record {
	for i := range st.Records {
		if k.matches(st.Records[i]) {
			return &st.Records[i]
		}
	}
	return nil
}

// Upsert records an installation. managed says whether the client (not the
// user) chose the directory; see Record.Managed.
func (st *State) Upsert(k Key, version string, caps CapSnapshot, managed bool) {
	rec := Record{
		Registry: strings.TrimRight(k.Registry, "/"), Namespace: k.Namespace,
		Name: k.Name, Target: k.Target, Dir: k.Dir,
		Version: version, InstalledAt: time.Now().UTC(), Capabilities: caps,
		Managed: managed,
	}
	if cur := st.Find(k); cur != nil {
		*cur = rec
		return
	}
	st.Records = append(st.Records, rec)
}

// Remove deletes the record for a key, reporting whether one was present.
func (st *State) Remove(k Key) bool {
	for i := range st.Records {
		if k.matches(st.Records[i]) {
			st.Records = append(st.Records[:i], st.Records[i+1:]...)
			return true
		}
	}
	return false
}

// Diff is the comparison between an installed contract and a new one: the
// core's manifest.CapabilityDiff, one type for the CLI, the desktop and the
// registry's account install.
type Diff = manifest.CapabilityDiff

// DiffCapabilities compares the previously installed contract with the new
// one. The rule is the core's manifest.DiffCapabilities — the registry
// re-asks for an account install's acceptance by it (DEV-120) — so the
// promise that the CLI, the desktop and the site ask again on the same
// widening is kept by construction, not by two copies staying in step
// (agentry-cli#201). Everything it reports is phrased the way §11 promises:
// "this update adds network egress to evil.example".
func DiffCapabilities(old, next CapSnapshot) Diff {
	return manifest.DiffCapabilities(old.capabilities(), next.capabilities())
}

// ArchiveWidenings reports what an archive's own manifest declares beyond
// the contract the person consented to, the resolve manifest's, on which
// the prompt and the diff were raised (agentry-cli#252; the desktop's
// agentry-desktop#163). The digest and the attestation cover the archive,
// so a widening means the registry's resolve answer diverged from the
// bytes it served, and an install refuses it rather than reconciling. It
// is the core's rule, manifest.DiffCapabilities, on the two full
// contracts, plus what that rule does not yet compare: a declared MCP
// server's own egress, secrets and tool restriction (agentry#527). The
// desktop keeps a copy until it calls this (agentry-desktop#220).
func ArchiveWidenings(consented, archive manifest.Capabilities) []string {
	widened := manifest.DiffCapabilities(consented, archive).Widened
	return append(widened, serverWidenings(consented.MCPServers, archive.MCPServers)...)
}

// serverID is a declared server's key in a snapshot and in the core's
// diff: its package id and pin, "npm:@scope/x@1.2.3" (serverFromID's
// inverse).
func serverID(s manifest.MCPServer) string { return s.SourceID() + "@" + s.Version }

// serverWidenings reports what the archive grants a declared MCP server
// beyond the consented declaration of the same server (same package and
// pin); a server the consented contract lacks is the core diff's to report.
func serverWidenings(consented, archive []manifest.MCPServer) []string {
	byID := make(map[string]manifest.MCPServer, len(consented))
	for _, s := range consented {
		byID[serverID(s)] = s
	}
	var out []string
	for _, s := range archive {
		id := serverID(s)
		c, ok := byID[id]
		if !ok {
			continue
		}
		for _, h := range s.Egress {
			if !slices.Contains(c.Egress, h) {
				out = append(out, fmt.Sprintf("adds network egress to %s for MCP server %s", h, id))
			}
		}
		for _, n := range s.Secrets {
			if !slices.Contains(c.Secrets, n) {
				out = append(out, fmt.Sprintf("asks for a new secret %s for MCP server %s", n, id))
			}
		}
		if len(c.Tools) == 0 {
			continue // the consented declaration already granted the full set
		}
		if len(s.Tools) == 0 {
			out = append(out, fmt.Sprintf("grants MCP server %s its full tool set (consented: %s)", id, strings.Join(c.Tools, ", ")))
			continue
		}
		for _, tool := range s.Tools {
			if !slices.Contains(c.Tools, tool) {
				out = append(out, fmt.Sprintf("adds tool %s to MCP server %s", tool, id))
			}
		}
	}
	return out
}
