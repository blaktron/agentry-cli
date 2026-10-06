package drafter

import (
	"bytes"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
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
	// Versions is the versions the entry is about: an exact version, a
	// range in the ecosystem's grammar, or "all".
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
