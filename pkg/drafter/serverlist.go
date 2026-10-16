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
// sources. The two pages that show them are rendered from it
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
// not run here, each with the clause the pages give it after "it".
// NotRunReason reads it.
var notRunReasons = map[string]string{
	"musl-native":           "needs a native binary built for glibc, and the Node runtime image is Alpine (musl)",
	"needs-install-scripts": "needs an install script, and npm installs run with --ignore-scripts",
	"needs-arguments":       "needs arguments or flags to start, and a server is started with none",
	"needs-run-files":       "works on local files or folders, and none of the run's files are in its container",
	"spawns-binary":         "runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell",
	"non-443-port":          "talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only",
	"remote-only":           "is an HTTP-only server, and a package server must speak stdio (a vendor's own remote server is declared by its address instead)",
	"oauth-browser":         "signs in through a browser window or a credentials file of its own, and a package server gets only its environment and, with a sign-in the drafting guide lists, a token folder",
	"runtime-excluded":      "declares a runtime range that excludes the runtime's version",
	"writes-own-tree":       "writes into its own installed files, which are read-only",
	"secret-shaped-output":  "returns results that look like credentials, which the runner blocks",
	// Three more: a package that is not a server, a server whose tool list
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
// dependencies that mean a server does not run here, and what was
// seen of the runtime images a server runs in.
type ServerList struct {
	Servers      []ServerEntry     `yaml:"servers"`
	Dependencies []DependencyEntry `yaml:"dependencies"`
	Runtimes     []RuntimeEntry    `yaml:"runtimes"`
	// Remote are vendors' own remote MCP servers, by address and sign-in,
	// and SignIns the package servers' OAuth sign-ins seen working.
	// A draft may give a package an oauth block only by copying one listed
	// here.
	Remote  []RemoteEntry `yaml:"remote"`
	SignIns []SignInEntry `yaml:"signins"`
}

// RemoteEntry is one vendor's remote MCP server, as an address and a way
// of signing in. Seen says it answered a tool call through Agentry;
// otherwise the entry is what the vendor's own metadata said on its
// date, read anonymously.
type RemoteEntry struct {
	Vendor string `yaml:"vendor"`
	URL    string `yaml:"url"`
	// Auth is the manifest's: oauth, secret or none.
	Auth manifest.MCPAuth `yaml:"auth"`
	// Scopes are what the vendor's metadata offers, for auth oauth.
	Scopes []string `yaml:"scopes,omitempty"`
	// Client is, for auth oauth, ClientAgentry (the vendor lets Agentry
	// register itself) or ClientOwn (the person brings their own client).
	Client     string    `yaml:"client,omitempty"`
	Seen       bool      `yaml:"seen"`
	Observed   string    `yaml:"observed"`
	Conditions []string  `yaml:"conditions,omitempty"`
	Evidence   string    `yaml:"evidence"`
	Date       time.Time `yaml:"date"`
}

// SignInEntry is a package server's OAuth sign-in seen working: the
// package and version, the hosts and plain settings the block needs beside
// it, and the block itself, which a draft copies exactly.
type SignInEntry struct {
	Ecosystem string `yaml:"ecosystem"`
	Package   string `yaml:"package"`
	// Versions is the exact version seen, or "all".
	Versions string `yaml:"versions"`
	// Egress are hosts the server's egress must name.
	Egress []string `yaml:"egress"`
	// Env are plain settings the server's env must carry, as given.
	Env   map[string]string `yaml:"env,omitempty"`
	OAuth manifest.MCPOAuth `yaml:"oauth"`
	// Client is ClientAgentry or ClientOwn, as on a RemoteEntry.
	Client   string    `yaml:"client"`
	Observed string    `yaml:"observed"`
	Evidence string    `yaml:"evidence"`
	Date     time.Time `yaml:"date"`
}

// How a vendor's OAuth sign-in gets its client: Agentry registers itself
// (a client metadata document or dynamic registration), or the person
// brings their own OAuth client, entered once per vendor.
const (
	ClientAgentry = "agentry"
	ClientOwn     = "own"
)

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
	// Evidence is where it was seen.
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
	seenRemote := map[string]bool{}
	for i, r := range l.Remote {
		at := fmt.Sprintf("%s: remote[%d] (%s)", ServerListFile, i, r.URL)
		switch {
		case r.Vendor == "" || r.URL == "" || r.Observed == "" || r.Evidence == "" || r.Date.IsZero():
			return nil, fmt.Errorf("%s: vendor, url, observed, evidence and date are required", at)
		case !strings.HasPrefix(r.URL, "https://"):
			return nil, fmt.Errorf("%s: url must be https://", at)
		case r.Auth != manifest.AuthOAuth && r.Auth != manifest.AuthSecret && r.Auth != manifest.AuthNone:
			return nil, fmt.Errorf("%s: auth %q is not oauth, secret or none", at, r.Auth)
		case r.Auth == manifest.AuthOAuth && r.Client != ClientAgentry && r.Client != ClientOwn:
			return nil, fmt.Errorf("%s: an oauth entry's client must be %s or %s", at, ClientAgentry, ClientOwn)
		case r.Auth == manifest.AuthOAuth && len(r.Scopes) == 0:
			return nil, fmt.Errorf("%s: an oauth entry lists the scopes the vendor offers", at)
		case r.Auth != manifest.AuthOAuth && (r.Client != "" || len(r.Scopes) > 0):
			return nil, fmt.Errorf("%s: client and scopes belong to an oauth entry", at)
		case seenRemote[r.URL+" "+string(r.Auth)]:
			return nil, fmt.Errorf("%s: listed twice for auth %s", at, r.Auth)
		}
		seenRemote[r.URL+" "+string(r.Auth)] = true
	}
	seenSignIn := map[string]bool{}
	for i, si := range l.SignIns {
		at := fmt.Sprintf("%s: signins[%d] (%s:%s)", ServerListFile, i, si.Ecosystem, si.Package)
		key := si.Ecosystem + ":" + si.Package + "@" + si.Versions
		switch {
		case seenSignIn[key]:
			return nil, fmt.Errorf("%s: listed twice for version %s", at, si.Versions)
		case len(si.Egress) == 0:
			return nil, fmt.Errorf("%s: egress names the hosts the server reaches", at)
		case !slices.Contains(ecosystems, si.Ecosystem):
			return nil, fmt.Errorf("%s: ecosystem must be npm or pypi", at)
		case si.Package == "" || si.Versions == "" || si.OAuth.Issuer == "" || si.Observed == "" || si.Evidence == "" || si.Date.IsZero():
			return nil, fmt.Errorf("%s: package, versions, oauth.issuer, observed, evidence and date are required", at)
		case si.Versions != AllVersions && strings.ContainsAny(si.Versions, versionRangeChars):
			return nil, fmt.Errorf("%s: versions %q must be one version or %q, not a range", at, si.Versions, AllVersions)
		case si.Client != ClientAgentry && si.Client != ClientOwn:
			return nil, fmt.Errorf("%s: client must be %s or %s", at, ClientAgentry, ClientOwn)
		}
		seenSignIn[key] = true
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
// these. A Runs here entry speaks for its own version only: another
// version is judged with the guide. The Does not run here page names them
// (VersionBoundReasons).
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

// RemoteServer is the Remote entry for a remote server's address and
// auth, compared as written, with a trailing slash ignored.
func (l *ServerList) RemoteServer(address string, auth manifest.MCPAuth) (RemoteEntry, bool) {
	for _, r := range l.Remote {
		if strings.TrimSuffix(r.URL, "/") == strings.TrimSuffix(address, "/") && r.Auth == auth {
			return r, true
		}
	}
	return RemoteEntry{}, false
}

// SignIn is the listed sign-in for a package's version, if one is listed.
func (l *ServerList) SignIn(ecosystem, pkg, version string) (SignInEntry, bool) {
	for _, si := range l.SignIns {
		if si.Ecosystem == ecosystem && samePackage(ecosystem, si.Package, pkg) && (si.Versions == AllVersions || si.Versions == version) {
			return si, true
		}
	}
	return SignInEntry{}, false
}

// Differs names, by field, how a declared server's sign-in departs from
// the listed one: its oauth block not copied exactly, a host the listed
// egress names and the server's lacks, a listed setting missing or
// changed. "" when it is the listed sign-in. Every word is the entry's or
// a field's name, never the draft's, so nothing the model wrote reaches
// the repair turn through it.
func (si SignInEntry) Differs(srv manifest.MCPServer) string {
	var out []string
	o := srv.OAuth
	if o == nil {
		return "oauth is missing"
	}
	if o.Issuer != si.OAuth.Issuer {
		out = append(out, "oauth.issuer is not "+si.OAuth.Issuer)
	}
	if !slices.Equal(o.Scopes, si.OAuth.Scopes) {
		out = append(out, "oauth.scopes are not "+strings.Join(si.OAuth.Scopes, ", "))
	}
	if !slices.Equal(o.Files, si.OAuth.Files) {
		out = append(out, "oauth.files are not the listed files, path and template")
	}
	if !maps.Equal(o.Env, si.OAuth.Env) {
		out = append(out, "oauth.env is not the listed env")
	}
	for _, h := range si.Egress {
		if !slices.Contains(srv.Egress, h) {
			out = append(out, "egress lacks "+h)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(si.Env)) {
		if v, ok := srv.Env[k]; !ok || v != si.Env[k] {
			out = append(out, "env."+k+" is not "+si.Env[k])
		}
	}
	return strings.Join(out, "; ")
}
