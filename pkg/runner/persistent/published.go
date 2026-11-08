package persistent

// A published persistent agent is a persistent release installed from the
// registry, or an author's own folder installed on this computer, beside the
// agents built into Agentry. Its install is recorded in its state folder
// (release.yaml), which is what makes it one of the agents: the release it
// runs, the subagents its release declares, pinned when it was installed,
// and where it came from. Its state folder, its volumes and its socket are
// keyed by its reference with a dot, <ns>.<name>: no name or namespace holds
// a dot, so two publishers' agents of one name never share anything, and no
// published agent's key is a built-in's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// releaseFile is a published agent's install record in its state folder.
const releaseFile = "release.yaml"

// Where a published agent's release came from.
const (
	SourceRegistry = "registry"
	SourceLocal    = installed.SourceLocal
)

// Release is a published agent's install record: the release it runs and
// the subagents its release declares, as they were pinned at its install.
type Release struct {
	// Ref is the release's ns/name.
	Ref string `yaml:"ref" json:"ref"`
	// Version and Digest name the release folder the agent runs.
	Version string `yaml:"version" json:"version"`
	Digest  string `yaml:"digest" json:"digest"`
	// Source is SourceRegistry or SourceLocal.
	Source string `yaml:"source" json:"source"`
	// Registry is the registry a registry release was installed from, which
	// its subagents are run from; empty is this computer's default.
	Registry string `yaml:"registry,omitempty" json:"registry,omitempty"`
	// Folder is the author's folder a local agent was installed from, to
	// say where it came from; empty for a registry release.
	Folder string `yaml:"folder,omitempty" json:"folder,omitempty"`
	// Subagents are the subagents the release declares, each pinned to the
	// version its constraint resolved to when it was installed.
	Subagents   []SubagentPin `yaml:"subagents,omitempty" json:"subagents,omitempty"`
	InstalledAt time.Time     `yaml:"installedAt" json:"installedAt"`
}

// SubagentPin is one subagent a published agent's release declares, pinned
// at its install.
type SubagentPin struct {
	// Ref is the subagent's ns/name, Constraint the release's semver
	// constraint for it and Why its reason, as the release declares them.
	Ref        string `yaml:"ref" json:"ref"`
	Constraint string `yaml:"constraint" json:"constraint"`
	Why        string `yaml:"why" json:"why"`
	// Version is the version the constraint resolved to at the install;
	// empty when it resolved to none, with Unresolved saying why.
	Version    string `yaml:"version,omitempty" json:"version,omitempty"`
	Unresolved string `yaml:"unresolved,omitempty" json:"unresolved,omitempty"`
	// Reach is what that version's contract reaches, in a line, as its
	// roster entry's sentence quotes it.
	Reach string `yaml:"reach,omitempty" json:"reach,omitempty"`
	// Digest is that version's archive digest, as the registry answered
	// it when the constraint was resolved.
	Digest string `yaml:"digest,omitempty" json:"digest,omitempty"`
}

// Persona is the name and the text a published agent's release ships for
// it to take on.
type Persona struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Editable says the person may replace the text; a locked persona
	// may still be renamed.
	Editable bool `json:"editable"`
}

// Key is a published agent's key: its state folder's, its volumes' and its
// socket's name.
func Key(namespace, name string) string { return namespace + "." + name }

// isPublishedKey says a state folder's name is a published agent's key:
// only Key puts a dot in one.
func isPublishedKey(name string) bool { return strings.Contains(name, ".") }

// splitRef reads ns/name, refusing anything else.
func splitRef(ref string) (namespace, name string, err error) {
	ns, n, ok := strings.Cut(ref, "/")
	if !ok || !manifest.ValidNamespace(ns) || !manifest.ValidName(n) {
		return "", "", fmt.Errorf("%q is not a QuickAgent's ns/name", ref)
	}
	return ns, n, nil
}

// IsBuiltinRef says ref (ns/name) is a persistent built-in's, which is never
// installed as a published agent.
func IsBuiltinRef(ref string) bool {
	return slices.ContainsFunc(localrun.PersistentBuiltins(), func(b localrun.Builtin) bool { return b.Namespace()+"/"+b.Name() == ref })
}

// LoadRelease reads the install record of the published agent keyed key; a
// key with none is not installed (fs.ErrNotExist).
func LoadRelease(home, key string) (Release, error) {
	var r Release
	b, err := os.ReadFile(filepath.Join(Dir(home, key), releaseFile)) //nolint:gosec // the runner home's own file
	if err != nil {
		return r, err
	}
	if err := yaml.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("read the install record of %s: %w", key, err)
	}
	ns, name, err := splitRef(r.Ref)
	if err != nil || Key(ns, name) != key || IsBuiltinRef(r.Ref) {
		return r, fmt.Errorf("the install record in %s names %q, another agent", Dir(home, key), r.Ref)
	}
	// The version and the digest name the release's folder: each is held to
	// its form, so neither can lead the folder anywhere else.
	if !releaseVersionRE.MatchString(r.Version) || strings.Contains(r.Version, "..") {
		return r, fmt.Errorf("the install record in %s names the version %q, which is not one", Dir(home, key), r.Version)
	}
	if !releaseDigestRE.MatchString(r.Digest) {
		return r, fmt.Errorf("the install record in %s names the digest %q, which is not a sha256 digest", Dir(home, key), r.Digest)
	}
	return r, nil
}

// releaseVersionRE and releaseDigestRE are the forms an install record's
// version and digest take.
var (
	releaseVersionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	releaseDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// SaveRelease records a published agent's install in its state folder and
// enables it: what `agentry agent install` writes once its release is laid
// down and the person agreed to it. Its configuration, conversation and
// volumes, kept from an earlier install of the same reference, are its
// again; an agent the person disabled stays disabled when only its version
// moves.
func SaveRelease(home string, r Release) (Agent, error) {
	ns, name, err := splitRef(r.Ref)
	if err != nil {
		return Agent{}, err
	}
	if IsBuiltinRef(r.Ref) {
		return Agent{}, fmt.Errorf("%s is built into Agentry, and runs as the built-in", r.Ref)
	}
	if r.Source != SourceRegistry && r.Source != SourceLocal {
		return Agent{}, fmt.Errorf("an install record from %q, neither the registry nor a local folder", r.Source)
	}
	key := Key(ns, name)
	_, priorErr := LoadRelease(home, key)
	firstInstall := errors.Is(priorErr, fs.ErrNotExist)
	b, err := yaml.Marshal(r)
	if err != nil {
		return Agent{}, err
	}
	dir := Dir(home, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Agent{}, fmt.Errorf("the %s agent's folder: %w", r.Ref, err)
	}
	if err := writeFileAtomic(dir, releaseFile, b); err != nil {
		return Agent{}, fmt.Errorf("record the install of %s: %w", r.Ref, err)
	}
	a, err := agentOf(home, key, r)
	if err != nil {
		return Agent{}, err
	}
	if firstInstall {
		cfg, err := LoadConfig(home, key)
		if err != nil {
			return a, err
		}
		cfg.Enabled = true
		if err := SaveConfig(home, key, cfg); err != nil {
			return a, err
		}
	}
	return a, nil
}

// writeFileAtomic replaces dir/name with b (0600) through a temporary file.
func writeFileAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// agentOf is the published agent an install record makes: its release
// folder, read for its persona and the packs it ships. The folder is held
// to its integrity record when a segment starts, not here: a listing reads
// what it says of itself.
func agentOf(home, key string, r Release) (Agent, error) {
	ns, name, err := splitRef(r.Ref)
	if err != nil {
		return Agent{}, err
	}
	dir := installdir.ReleaseDir(home, ns, name, r.Version, r.Digest)
	a := Agent{
		Name: key, Ref: r.Ref, Version: r.Version, Title: name, ShareName: key + "-share",
		Source: r.Source, Subagents: slices.Clone(r.Subagents), Registry: r.Registry,
		Release: &localrun.Installed{Dir: dir, Meta: &installdir.Meta{
			Ref: r.Ref + "@" + r.Version, Namespace: ns, Name: name, Version: r.Version, Digest: r.Digest, Source: r.Source,
		}},
	}
	m, err := localrun.ManifestFrom(filepath.Join(dir, "workspace"))
	if err != nil {
		return a, fmt.Errorf("the release of %s@%s: %w", r.Ref, r.Version, err)
	}
	a.Packs, a.Caps, a.DesktopLine = m.ShippedPacks(), m.Capabilities.Persistent, m.Capabilities.Desktop
	if m.Persona != nil {
		p, err := readPersona(filepath.Join(dir, "workspace"), m)
		if err != nil {
			return a, fmt.Errorf("the persona of %s@%s: %w", r.Ref, r.Version, err)
		}
		a.Persona = p
	}
	return a, nil
}

// readPersona reads the persona the release ships, at most
// manifest.MaxPersonaFileBytes of UTF-8, beneath its workspace.
func readPersona(workspace string, m *manifest.Manifest) (*Persona, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(m.PersonaFile())
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, manifest.MaxPersonaFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > manifest.MaxPersonaFileBytes || !utf8.Valid(b) {
		return nil, fmt.Errorf("%s is not UTF-8 text of at most %d bytes", m.PersonaFile(), manifest.MaxPersonaFileBytes)
	}
	return &Persona{Name: m.Persona.Name, Text: strings.TrimSpace(string(b)), Editable: m.Persona.IsEditable()}, nil
}

// publishedAgents are the published agents installed in home, by reference.
// A state folder whose record does not read is left out.
func publishedAgents(home string) []Agent {
	entries, err := os.ReadDir(filepath.Join(home, "persistent"))
	if err != nil {
		return nil
	}
	var out []Agent
	for _, e := range entries {
		if !e.IsDir() || !isPublishedKey(e.Name()) {
			continue
		}
		r, err := LoadRelease(home, e.Name())
		if err != nil {
			continue
		}
		a, err := agentOf(home, e.Name(), r)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Agent) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}

// AgentsIn are the persistent agents of the runner home home: the built-ins,
// then the published agents installed there, by reference.
func AgentsIn(home string) []Agent { return append(Agents(), publishedAgents(home)...) }

// LookupIn is the agent of the runner home home that name names: a
// built-in by its short name, a published agent by its reference (ns/name)
// or its key, or by its name alone when no other agent has it. A built-in's
// short name is always the built-in. A name two published agents share is
// refused, naming both.
func LookupIn(home, name string) (Agent, error) {
	for _, a := range Agents() {
		if a.Name == name || a.Ref == name {
			return a, nil
		}
	}
	published := publishedAgents(home)
	for _, a := range published {
		if a.Ref == name || a.Name == name {
			return a, nil
		}
	}
	var matches []string
	var match Agent
	for _, a := range published {
		if _, short, _ := strings.Cut(a.Ref, "/"); short == name {
			matches, match = append(matches, a.Ref), a
		}
	}
	switch len(matches) {
	case 1:
		return match, nil
	case 0:
		var names []string
		for _, a := range Agents() {
			names = append(names, a.Name)
		}
		for _, a := range published {
			names = append(names, a.Ref)
		}
		return Agent{}, fmt.Errorf("no persistent agent named %q (the agents are %s)", name, strings.Join(names, ", "))
	}
	return Agent{}, fmt.Errorf("%q names more than one persistent agent (%s): name one by its reference", name, strings.Join(matches, ", "))
}

// Uninstall removes a published agent's install record, so it is no longer
// one of the agents, and keeps its state folder and volumes, which a later
// install of the same reference takes up again, unless removeData: then it
// is first reset as Reset does, its volumes and what it keeps in the
// keychain removed, and its state folder goes too, the files in its share
// folder, which are the person's, staying. It refuses a built-in and a
// running agent, and holds the agent's lock while it works. The release
// folder is the caller's to remove, with its consent record.
func Uninstall(ctx context.Context, home, name string, d dockerx.Client, removeData bool) error {
	a, err := LookupIn(home, name)
	if err != nil {
		return err
	}
	if a.Release == nil {
		return fmt.Errorf("%s is built into Agentry, and is not uninstalled: disable it (agentry agent disable %s)", a.Title, a.Name)
	}
	if removeData {
		if err := reset(ctx, home, a.Name, d, resetAll); err != nil {
			return err
		}
	}
	owner, err := Acquire(home, a.Name, ClientCLI)
	if err != nil {
		return err
	}
	defer owner.Release()
	if err := os.Remove(filepath.Join(Dir(home, a.Name), releaseFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the install record of %s: %w", a.Ref, err)
	}
	if !removeData {
		return nil
	}
	owner.Release()
	if err := os.RemoveAll(Dir(home, a.Name)); err != nil {
		return fmt.Errorf("remove the %s agent's folder: %w", a.Ref, err)
	}
	return nil
}

// PackageRef is the package the agent runs, as ns/name@version.
func (a Agent) PackageRef() string {
	if a.Release != nil {
		return a.Ref + "@" + a.Version
	}
	return a.Builtin.Ref()
}

// Published says the agent is installed from a release, not built in.
func (a Agent) Published() bool { return a.Release != nil }

// checksShare says a file of the agent's share is read only once it was
// checked: a built-in's as it declares, a published agent's always.
func (a Agent) checksShare() bool { return a.Release != nil || a.Builtin.ChecksShare() }
