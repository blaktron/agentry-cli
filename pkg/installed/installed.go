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
// original install.
package installed

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
		id := srv.SourceID() + "@" + srv.Version
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
	return s
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

// Diff is the comparison between an installed contract and a new one.
type Diff struct {
	// Widened lists strictly-more-permissive changes, phrased for a human.
	Widened []string `json:"widened,omitempty"`
	// Narrowed lists strictly-less-permissive changes.
	Narrowed []string `json:"narrowed,omitempty"`
}

// HasWidened reports whether anything requires confirmation.
func (d Diff) HasWidened() bool { return len(d.Widened) > 0 }

// Empty reports an identical contract.
func (d Diff) Empty() bool { return len(d.Widened) == 0 && len(d.Narrowed) == 0 }

// fsRank orders filesystem access declarations from least to most permissive.
// Explicit "none" ranks BELOW undeclared: a manifest that says nothing has
// told us nothing, which is worse than one that promises no access — but it
// is still better than one that asks for access.
var fsRank = map[string]int{"none": 0, "": 1, "paths": 2, "workspace": 3}

// DiffCapabilities compares the previously installed contract with the new
// one. Everything it reports is phrased the way §11 promises: "this update
// adds network egress to evil.example".
func DiffCapabilities(old, next CapSnapshot) Diff {
	var d Diff

	// Network egress: the canonical widening.
	added, removed := diffSets(old.EgressHosts, next.EgressHosts)
	for _, h := range added {
		if h == "*" {
			d.Widened = append(d.Widened, "adds UNRESTRICTED network egress (any host)")
			continue
		}
		d.Widened = append(d.Widened, "adds network egress to "+h)
	}
	for _, h := range removed {
		d.Narrowed = append(d.Narrowed, "drops network egress to "+h)
	}

	// Exec: none → commands, or a longer command list.
	added, removed = diffSets(old.ExecCommands, next.ExecCommands)
	for _, c := range added {
		if len(old.ExecCommands) == 0 {
			d.Widened = append(d.Widened, "adds subprocess execution: "+c+" (previously none)")
			continue
		}
		d.Widened = append(d.Widened, "adds subprocess execution: "+c)
	}
	for _, c := range removed {
		d.Narrowed = append(d.Narrowed, "drops subprocess execution: "+c)
	}

	// Filesystem, both directions. Write escalation is the one §11 names, but
	// a read widening is a widening too and gets the same treatment.
	d.appendFSDiff("read", old.FSRead, next.FSRead, old.FSReadPaths, next.FSReadPaths)
	d.appendFSDiff("write", old.FSWrite, next.FSWrite, old.FSWritePaths, next.FSWritePaths)

	// Secrets: a new environment variable name is a new thing being asked for.
	added, removed = diffSets(old.Secrets, next.Secrets)
	for _, s := range added {
		d.Widened = append(d.Widened, "asks for a new secret: "+s)
	}
	for _, s := range removed {
		d.Narrowed = append(d.Narrowed, "no longer asks for secret: "+s)
	}

	added, removed = diffSets(old.DataAccess, next.DataAccess)
	for _, s := range added {
		d.Widened = append(d.Widened, "adds data access: "+s)
	}
	for _, s := range removed {
		d.Narrowed = append(d.Narrowed, "drops data access: "+s)
	}

	// Declared MCP servers (RUNNER-PLAN-3): a new server — or the same
	// server at a different pin — is new tool surface and new supply
	// chain, so it widens. A dropped one narrows.
	added, removed = diffSets(old.MCPServers, next.MCPServers)
	for _, s := range added {
		line := "adds MCP server: " + s
		if env := next.MCPServerEnv[s]; len(env) > 0 {
			// A new server's env is part of what is being added.
			var pairs []string
			for _, k := range slices.Sorted(maps.Keys(env)) {
				pairs = append(pairs, k+"="+env[k])
			}
			line += " (env " + strings.Join(pairs, ", ") + ")"
		}
		d.Widened = append(d.Widened, line)
	}
	for _, s := range removed {
		d.Narrowed = append(d.Narrowed, "drops MCP server: "+s)
	}

	// A server's plain env: a new or changed value changes what the
	// server receives, so it widens and re-asks; a dropped key narrows.
	// Only servers present on both sides are compared — a new server is
	// already reported whole above.
	for _, id := range next.MCPServers {
		if !slices.Contains(old.MCPServers, id) {
			continue
		}
		oldEnv, newEnv := old.MCPServerEnv[id], next.MCPServerEnv[id]
		for _, k := range slices.Sorted(maps.Keys(newEnv)) {
			if prev, had := oldEnv[k]; !had {
				d.Widened = append(d.Widened, fmt.Sprintf("changes env of MCP server %s: %s=%s", id, k, newEnv[k]))
			} else if prev != newEnv[k] {
				d.Widened = append(d.Widened, fmt.Sprintf("changes env of MCP server %s: %s=%s (was %s)", id, k, newEnv[k], prev))
			}
		}
		for _, k := range slices.Sorted(maps.Keys(oldEnv)) {
			if _, still := newEnv[k]; !still {
				d.Narrowed = append(d.Narrowed, fmt.Sprintf("drops env %s of MCP server %s", k, id))
			}
		}
	}

	return d
}

// appendFSDiff records one filesystem direction's change.
func (d *Diff) appendFSDiff(dir, oldMode, newMode string, oldPaths, newPaths []string) {
	switch {
	case fsRank[newMode] > fsRank[oldMode]:
		d.Widened = append(d.Widened, fmt.Sprintf("widens filesystem %s from %s to %s",
			dir, orUndeclared(oldMode), describeFS(newMode, newPaths)))
	case fsRank[newMode] < fsRank[oldMode]:
		d.Narrowed = append(d.Narrowed, fmt.Sprintf("narrows filesystem %s from %s to %s",
			dir, orUndeclared(oldMode), describeFS(newMode, newPaths)))
	case newMode == "paths":
		added, removed := diffSets(oldPaths, newPaths)
		for _, p := range added {
			d.Widened = append(d.Widened, fmt.Sprintf("adds filesystem %s path: %s", dir, p))
		}
		for _, p := range removed {
			d.Narrowed = append(d.Narrowed, fmt.Sprintf("drops filesystem %s path: %s", dir, p))
		}
	}
}

func describeFS(mode string, paths []string) string {
	if mode == "paths" && len(paths) > 0 {
		return "paths " + strings.Join(paths, ", ")
	}
	return orUndeclared(mode)
}

func orUndeclared(mode string) string {
	if mode == "" {
		return "undeclared"
	}
	return mode
}

// diffSets returns the sorted elements added to and removed from a list.
func diffSets(old, next []string) (added, removed []string) {
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, v := range next {
		if !has(old, v) {
			added = append(added, v)
		}
	}
	for _, v := range old {
		if !has(next, v) {
			removed = append(removed, v)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
