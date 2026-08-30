// Install state and the capability diff (§11 install row: "show capability
// summary (and DIFF vs currently installed version, requiring confirm on
// widened capabilities in interactive mode; --yes for CI)").
//
// The registry knows what an agent declares; only the machine knows what it
// previously ACCEPTED. Without a local record, every reinstall re-asks the
// same question from scratch and an upgrade that quietly adds egress to a new
// host looks exactly like the install the user already approved. This file is
// that record.
//
// Location: ~/.config/agentry/installed.yaml — alongside config.yaml rather
// than under ~/.local/state. AGENTRY_CONFIG_DIR already relocates everything
// the CLI keeps on disk (config, pinned signing keys, cached rule bundles);
// a second root would be a second thing to point at a container volume, and
// nothing here is large or high-churn enough to earn that.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"
)

// capSnapshot is the on-disk form of a §5.4 capability contract.
//
// It is deliberately NOT manifest.Capabilities: that type's union fields
// (Egress, FSAccess) carry their shape in unexported-by-tag form with custom
// JSON codecs, so persisting it as YAML would silently drop the very fields
// the diff turns on. A flat, explicit record is also a format this file owns
// and can keep stable while the manifest evolves.
type capSnapshot struct {
	// EgressHosts lists declared egress hosts; empty means none/undeclared.
	EgressHosts []string `yaml:"egressHosts,omitempty"`
	// EgressNone records an explicit `egress: none` (vs. not declared).
	EgressNone bool `yaml:"egressNone,omitempty"`
	// FSRead/FSWrite are the declared modes ("", none, workspace, paths).
	FSRead       string   `yaml:"fsRead,omitempty"`
	FSReadPaths  []string `yaml:"fsReadPaths,omitempty"`
	FSWrite      string   `yaml:"fsWrite,omitempty"`
	FSWritePaths []string `yaml:"fsWritePaths,omitempty"`
	// ExecCommands lists declared subprocess commands.
	ExecCommands []string `yaml:"execCommands,omitempty"`
	// Secrets lists declared environment-variable NAMES (never values).
	Secrets []string `yaml:"secrets,omitempty"`
	// DataAccess lists declared semantic data categories.
	DataAccess []string `yaml:"dataAccess,omitempty"`
}

// snapshotCapabilities flattens a manifest capability block for storage.
func snapshotCapabilities(c manifest.Capabilities) capSnapshot {
	s := capSnapshot{
		EgressHosts:  append([]string(nil), c.Network.Egress.Hosts...),
		EgressNone:   c.Network.Egress.None,
		FSRead:       string(c.Filesystem.Read.Mode),
		FSReadPaths:  append([]string(nil), c.Filesystem.Read.Paths...),
		FSWrite:      string(c.Filesystem.Write.Mode),
		FSWritePaths: append([]string(nil), c.Filesystem.Write.Paths...),
		ExecCommands: append([]string(nil), c.Exec.Commands...),
		Secrets:      append([]string(nil), c.Secrets...),
	}
	for _, d := range c.DataAccess {
		s.DataAccess = append(s.DataAccess, string(d))
	}
	return s
}

// installRecord is one installed (registry, agent, target, dir) tuple.
type installRecord struct {
	Registry     string      `yaml:"registry"`
	Namespace    string      `yaml:"namespace"`
	Name         string      `yaml:"name"`
	Target       string      `yaml:"target"`
	Dir          string      `yaml:"dir"`
	Version      string      `yaml:"version"`
	InstalledAt  time.Time   `yaml:"installedAt"`
	Capabilities capSnapshot `yaml:"capabilities"`
}

// installState is ~/.config/agentry/installed.yaml.
type installState struct {
	Records []installRecord `yaml:"records,omitempty"`
}

// installStatePath returns the manifest path inside the config dir.
func installStatePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "installed.yaml"), nil
}

// loadInstallState reads the install manifest; a missing file is empty state.
//
// A CORRUPT file is also treated as empty, with a warning from the caller
// rather than an error: refusing to install because a bookkeeping file got
// truncated would turn a cosmetic problem into an outage, and the worst case
// is one extra confirmation prompt.
func loadInstallState() (installState, error) {
	var st installState
	path, err := installStatePath()
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
		return installState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}

// saveInstallState writes the manifest (0600: it names local paths).
func saveInstallState(st installState) error {
	path, err := installStatePath()
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

// installKey identifies one installation. The DIRECTORY is part of the key:
// the same agent installed as a claude-skill in ~/.claude/skills and as an
// mcp server in a project are two independent installations with independent
// capability histories, and a diff that conflated them would ask the wrong
// question about the wrong files.
type installKey struct {
	registry, namespace, name, target, dir string
}

func (k installKey) matches(r installRecord) bool {
	return strings.TrimRight(r.Registry, "/") == strings.TrimRight(k.registry, "/") &&
		r.Namespace == k.namespace && r.Name == k.name &&
		r.Target == k.target && r.Dir == k.dir
}

// findInstall returns the record for a key, or nil.
func (st *installState) findInstall(k installKey) *installRecord {
	for i := range st.Records {
		if k.matches(st.Records[i]) {
			return &st.Records[i]
		}
	}
	return nil
}

// record upserts an installation.
func (st *installState) record(k installKey, version string, caps capSnapshot) {
	rec := installRecord{
		Registry: strings.TrimRight(k.registry, "/"), Namespace: k.namespace,
		Name: k.name, Target: k.target, Dir: k.dir,
		Version: version, InstalledAt: time.Now().UTC(), Capabilities: caps,
	}
	if cur := st.findInstall(k); cur != nil {
		*cur = rec
		return
	}
	st.Records = append(st.Records, rec)
}

// capDiff is the comparison between an installed contract and a new one.
type capDiff struct {
	// Widened lists strictly-more-permissive changes, phrased for a human.
	Widened []string
	// Narrowed lists strictly-less-permissive changes.
	Narrowed []string
}

// widened reports whether anything requires confirmation.
func (d capDiff) widened() bool { return len(d.Widened) > 0 }

// empty reports an identical contract.
func (d capDiff) empty() bool { return len(d.Widened) == 0 && len(d.Narrowed) == 0 }

// fsRank orders filesystem access declarations from least to most permissive.
// Explicit "none" ranks BELOW undeclared: a manifest that says nothing has
// told us nothing, which is worse than one that promises no access — but it
// is still better than one that asks for access.
var fsRank = map[string]int{"none": 0, "": 1, "paths": 2, "workspace": 3}

// diffCapabilities compares the previously installed contract with the new
// one. Everything it reports is phrased the way §11 promises: "this update
// adds network egress to evil.example".
func diffCapabilities(old, next capSnapshot) capDiff {
	var d capDiff

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

	return d
}

// appendFSDiff records one filesystem direction's change.
func (d *capDiff) appendFSDiff(dir, oldMode, newMode string, oldPaths, newPaths []string) {
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
