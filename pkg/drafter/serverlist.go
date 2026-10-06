package drafter

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ServerListFile is the drafting guide's two soft lists of declared MCP
// servers, Runs here and Does not run here, as one file in the pack's
// sources (plan drafter-knowledge.md §4.4, D3, D9; agentry-cli#658). The
// two pages that show them are rendered from it
// (TestTheDraftingGuideIsCurrent). A list is one thing for the Drafter to
// look at, never a gate in the registry or the runner, and no entry says a
// server is safe or good: each says what was observed, on which runtime, on
// which date.
const ServerListFile = manifest.KnowledgeDir + "/" + GuideName + "/" + knowledge.SourcesDir + "/servers.yaml"

// The two lists.
const (
	RunsHere       = "runs-here"
	DoesNotRunHere = "does-not-run-here"
)

// notRunReasons is the closed set of reasons a server or a dependency does
// not run here, each with the clause the pages give it after "it" (plan
// §4.4). NotRunReason reads it.
var notRunReasons = map[string]string{
	"musl-native":           "needs a native binary built for glibc, and the runtime images are Alpine (musl)",
	"needs-install-scripts": "needs an install script, and npm installs run with --ignore-scripts",
	"needs-arguments":       "needs arguments or flags to start, and a server is started with none",
	"needs-run-files":       "works on local files or folders, and none of the run's files are in its container",
	"spawns-binary":         "runs another program (a browser, git, a CLI), and its container holds only its runtime and busybox",
	"non-443-port":          "talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only",
	"remote-only":           "is a remote or HTTP-only server, and a declared server must speak stdio",
	"oauth-browser":         "signs in through a browser window or a credentials file, and a server gets only environment variables",
	"runtime-excluded":      "declares a runtime range that excludes the runtime's version",
	"writes-own-tree":       "writes into its own installed files, which are read-only",
	"secret-shaped-output":  "returns results that look like credentials, which the runner blocks",
	// Three the survey added to the plan's set (agentry-cli#658; DR-43's
	// addendum): a package that is not a server, a server whose tool list
	// stops the runner's gateway, and one that fails at start on any
	// machine.
	"not-a-server":        "is not an MCP server: it has no program to start",
	"invalid-tool-schema": "lists a tool whose input schema the runner's gateway refuses, which stops the gateway and every server in the run with it",
	"broken-upstream":     "fails at start on any machine, with the dependencies it installs today",
}

// NotRunReason is the clause the pages give a reason class, and whether
// the class is one of the closed set.
func NotRunReason(class string) (string, bool) {
	s, ok := notRunReasons[class]
	return s, ok
}

// ServerList is ServerListFile decoded: the servers on the two lists, the
// dependencies that mean a server does not run here, and what the survey
// saw of the runtime images a server runs in.
type ServerList struct {
	Servers      []ServerEntry     `yaml:"servers"`
	Dependencies []DependencyEntry `yaml:"dependencies"`
	Runtimes     []RuntimeEntry    `yaml:"runtimes"`
}

// RuntimeEntry is what a server meets inside one default runtime image:
// the user and HOME it starts as, and the programs the image has.
type RuntimeEntry struct {
	Image    string    `yaml:"image"`
	User     string    `yaml:"user"`
	Home     string    `yaml:"home"`
	Observed string    `yaml:"observed"`
	Programs []string  `yaml:"programs"`
	Evidence string    `yaml:"evidence"`
	Date     time.Time `yaml:"date"`
}

// ServerEntry is one server on one list.
type ServerEntry struct {
	Ecosystem string `yaml:"ecosystem"`
	Package   string `yaml:"package"`
	// Versions is the versions the entry is about: the exact version that
	// was seen, or "all". It is never a range (LoadServerList). Which other
	// versions an exact entry speaks for is versionBound's rule.
	Versions string `yaml:"versions"`
	List     string `yaml:"list"`
	// Reason is a class of the closed set (NotRunReason), on Does not run here only.
	Reason string `yaml:"reason,omitempty"`
	// Runtime is the runtime it was seen on ("Node 24.21.0").
	Runtime string `yaml:"runtime"`
	// Observed is what was seen, in one or two plain sentences.
	Observed string `yaml:"observed"`
	// Tools are the tools it listed, on Runs here.
	Tools []string `yaml:"tools,omitempty"`
	// Conditions are what a QuickAgent must declare or know for it to
	// run: its keys, its egress hosts, an env switch.
	Conditions []string `yaml:"conditions,omitempty"`
	// Evidence is where it was seen: "survey 2026-10-05", an issue.
	Evidence string    `yaml:"evidence"`
	Date     time.Time `yaml:"date"`
}

// DependencyEntry is a package whose presence as a server's direct
// dependency means the server does not run here.
type DependencyEntry struct {
	Ecosystem string    `yaml:"ecosystem"`
	Package   string    `yaml:"package"`
	Reason    string    `yaml:"reason"`
	Observed  string    `yaml:"observed"`
	Evidence  string    `yaml:"evidence"`
	Date      time.Time `yaml:"date"`
}

// LoadServerList reads ServerListFile from fsys (FS(), or a laid-down
// tree) and holds every entry to the file's rules: a known ecosystem and
// list, a reason from the closed set exactly where one belongs, the
// observation, evidence and date every entry carries, and one entry per
// package and list.
func LoadServerList(fsys fs.FS) (*ServerList, error) {
	b, err := fs.ReadFile(fsys, ServerListFile)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var l ServerList
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%s: %w", ServerListFile, err)
	}
	seen := map[string]bool{}
	for i, s := range l.Servers {
		at := fmt.Sprintf("%s: servers[%d] (%s:%s)", ServerListFile, i, s.Ecosystem, s.Package)
		switch {
		case !slices.Contains(ecosystems, s.Ecosystem):
			return nil, fmt.Errorf("%s: ecosystem must be npm or pypi", at)
		case s.Package == "" || s.Versions == "" || s.Runtime == "" || s.Observed == "" || s.Evidence == "" || s.Date.IsZero():
			return nil, fmt.Errorf("%s: package, versions, runtime, observed, evidence and date are required", at)
		case s.List != RunsHere && s.List != DoesNotRunHere:
			return nil, fmt.Errorf("%s: list must be %s or %s", at, RunsHere, DoesNotRunHere)
		case s.List == DoesNotRunHere && notRunReasons[s.Reason] == "":
			return nil, fmt.Errorf("%s: reason %q is not one of the closed set", at, s.Reason)
		case s.List == RunsHere && s.Reason != "":
			return nil, fmt.Errorf("%s: a Runs here entry carries no reason", at)
		case s.Versions != AllVersions && strings.ContainsAny(s.Versions, versionRangeChars):
			return nil, fmt.Errorf("%s: versions %q must be one version or %q, not a range", at, s.Versions, AllVersions)
		}
		key := s.Ecosystem + ":" + s.Package + ":" + s.List
		if seen[key] {
			return nil, fmt.Errorf("%s: listed twice on %s", at, s.List)
		}
		seen[key] = true
	}
	for i, d := range l.Dependencies {
		at := fmt.Sprintf("%s: dependencies[%d] (%s:%s)", ServerListFile, i, d.Ecosystem, d.Package)
		switch {
		case !slices.Contains(ecosystems, d.Ecosystem):
			return nil, fmt.Errorf("%s: ecosystem must be npm or pypi", at)
		case d.Package == "" || d.Observed == "" || d.Evidence == "" || d.Date.IsZero():
			return nil, fmt.Errorf("%s: package, observed, evidence and date are required", at)
		case notRunReasons[d.Reason] == "":
			return nil, fmt.Errorf("%s: reason %q is not one of the closed set", at, d.Reason)
		}
	}
	for i, r := range l.Runtimes {
		if r.Image == "" || r.User == "" || r.Home == "" || r.Observed == "" || r.Evidence == "" || r.Date.IsZero() {
			return nil, fmt.Errorf("%s: runtimes[%d]: image, user, home, observed, evidence and date are required", ServerListFile, i)
		}
	}
	return &l, nil
}

// ecosystems are the two a declared server comes from.
var ecosystems = []string{"npm", "pypi"}

// AllVersions is an entry's versions when it speaks for every version.
const AllVersions = "all"

// versionRangeChars are what a range has and a version does not.
const versionRangeChars = " <>=^~*|,!"

// versionBound are the reasons that hold for the version seen and not for
// the package's others: a runtime range a later release may widen, a tool
// schema it may correct, an upstream break it may mend. Every other reason
// is the server's design (a glibc binary, launch arguments, a browser it
// spawns, a port), so a Does not run here entry with an exact version
// speaks for every version of its package unless its reason is one of
// these (agentry-cli#660; DR-67's addendum). A Runs here entry speaks for
// its own version only: another version is judged with the guide. The
// Does not run here page names them (VersionBoundReasons).
var versionBound = map[string]bool{"runtime-excluded": true, "invalid-tool-schema": true, "broken-upstream": true}

// VersionBoundReasons are versionBound's classes, sorted.
func VersionBoundReasons() []string { return slices.Sorted(maps.Keys(versionBound)) }

// packageOf is a declared package server's ecosystem and package name.
func packageOf(srv manifest.MCPServer) (ecosystem, pkg string) {
	if srv.NPM != "" {
		return "npm", srv.NPM
	}
	return "pypi", srv.PyPI
}

// NotRunHere is the Does not run here entry that covers a pin: its
// package, in the ecosystem's own name rule, and a version the entry
// speaks for.
func (l *ServerList) NotRunHere(ecosystem, pkg, version string) (ServerEntry, bool) {
	for _, e := range l.Servers {
		if e.List == DoesNotRunHere && e.is(ecosystem, pkg) &&
			(e.Versions == AllVersions || e.Versions == version || !versionBound[e.Reason]) {
			return e, true
		}
	}
	return ServerEntry{}, false
}

// RunsHereEntry is the Runs here entry of a package, if it is listed; seen
// says the entry speaks for this version (its own, or all).
func (l *ServerList) RunsHereEntry(ecosystem, pkg, version string) (e ServerEntry, listed, seen bool) {
	for _, e := range l.Servers {
		if e.List == RunsHere && e.is(ecosystem, pkg) {
			return e, true, e.Versions == AllVersions || e.Versions == version
		}
	}
	return ServerEntry{}, false, false
}

// ListedDependency is the first of deps, a server's direct dependencies in
// one ecosystem, that the dependencies section lists.
func (l *ServerList) ListedDependency(ecosystem string, deps []string) (DependencyEntry, bool) {
	for _, dep := range deps {
		for _, d := range l.Dependencies {
			if d.Ecosystem == ecosystem && samePackage(ecosystem, d.Package, dep) {
				return d, true
			}
		}
	}
	return DependencyEntry{}, false
}

func (e ServerEntry) is(ecosystem, pkg string) bool {
	return e.Ecosystem == ecosystem && samePackage(ecosystem, e.Package, pkg)
}

// samePackage compares names as the ecosystem does: npm's exactly, PyPI's
// normalized (PEP 503).
func samePackage(ecosystem, a, b string) bool {
	if ecosystem == "pypi" {
		return mcpinstall.NormalizePyPIName(a) == mcpinstall.NormalizePyPIName(b)
	}
	return a == b
}
