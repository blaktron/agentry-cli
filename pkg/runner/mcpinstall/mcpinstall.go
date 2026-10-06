// Package mcpinstall resolves and installs the MCP servers a QuickAgent
// declares (RUNNER-PLAN-3): resolution turns each manifest pin into the
// exact registry artifact (npm / PyPI metadata, integrity recorded);
// installation materializes the dependency closure into the signature's
// server store inside a pinned runtime container. The store lives under
// the release's profile directory, which is what makes every isolation
// property hold: attachment is derived from the signature, so no other
// QuickAgent, user, or version can ever reference it.
package mcpinstall

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// httpTimeout bounds each registry metadata request.
const httpTimeout = 30 * time.Second

// Resolved is one manifest pin turned into exact artifacts.
type Resolved struct {
	// Source is "npm" or "pypi"; Package/Version echo the manifest pin.
	Source  string `json:"source"`
	Package string `json:"package"`
	Version string `json:"version"`
	// ID is the store subdirectory and catalog-name stem: source plus the
	// sanitized package name.
	ID string `json:"id"`
	// BinName is the launchable entry: the npm bin name, or the PyPI
	// console script. For PyPI the JSON API does not expose entry points,
	// so resolution starts from the normalized project name and
	// EnsureInstalled replaces it with the console script the installed
	// distribution actually declares (entry_points.txt, agentry-cli#13 /
	// DR-11); the recorded resolution carries the resolved name.
	BinName string `json:"binName"`
	// Integrity is what the registry advertised for the artifact (npm SRI
	// or "sha256:<hex>" for PyPI — the pure-Python wheel when one exists).
	Integrity string `json:"integrity,omitempty"`
	// Artifacts (PyPI) maps every advertised filename to its sha256 hex,
	// so the builder can verify whichever wheel pip picks for its platform
	// against what the registry's metadata said (agentry-cli#4).
	Artifacts map[string]string `json:"artifacts,omitempty"`
	// InstalledAt is stamped by EnsureInstalled.
	InstalledAt string `json:"installedAt,omitempty"`
	// StoreDigest is the server store's tree as the install left it
	// (storeDigest); a reuse that finds anything else refuses
	// (agentry-cli#162). The runs mount the store read-only, so nothing
	// legitimate changes it after the install.
	StoreDigest string `json:"storeDigest,omitempty"`
	// FetchHosts are the hosts, other than the registry's own, that the
	// registry's metadata said the pinned artifact is downloaded from (the
	// npm tarball's host; PyPI's file host, files.pythonhosted.org for
	// pypi.org). They join the install proxy's allowlist
	// (agentry-cli#92).
	FetchHosts []string `json:"fetchHosts,omitempty"`
	// InstallEgress is the allowlist the install's proxy enforced, as
	// host:port, stamped by EnsureInstalled; the proxy's own decisions are
	// in <id>.install-proxy.log beside this record (agentry-cli#92).
	InstallEgress []string `json:"installEgress,omitempty"`
	// DeclaredRuntime is the language range the release declares for
	// itself, as the registry's metadata gives it: npm's engines.node,
	// PyPI's requires_python; "" when it declares none (RuntimeMismatch,
	// agentry-cli#609).
	DeclaredRuntime string `json:"declaredRuntime,omitempty"`
	// Dependencies are the release's direct dependencies, by name, as the
	// metadata the resolution already fetched gives them: npm's
	// dependencies, PyPI's requires_dist less the extras' (normalized,
	// PEP 503). InstallScripts are the npm lifecycle scripts an install
	// would run (preinstall, install, postinstall), which a run's install
	// skips. Both are read by the Drafter's server check (agentry-cli#660)
	// and are not part of the recorded resolution.
	Dependencies   []string `json:"-"`
	InstallScripts []string `json:"-"`
}

// SourceID is the declared server's ecosystem-qualified id, as the
// manifest's MCPServer.SourceID gives it ("npm:@scope/x").
func (r Resolved) SourceID() string { return r.Source + ":" + r.Package }

// ServerName is the gateway catalog name for a resolved server.
func (r Resolved) ServerName() string { return "mcp-" + r.ID }

// prefix is the fixed in-container path for this server's closure.
func (r Resolved) prefix() string { return spec.MCPStorePrefix + "/" + r.ID }

// Command is the argv that launches the server inside its runtime
// container (stdio transport).
func (r Resolved) Command() []string {
	if r.Source == "npm" {
		return []string{r.prefix() + "/node_modules/.bin/" + r.BinName}
	}
	return []string{r.prefix() + "/venv/bin/" + r.BinName}
}

// Image picks the runtime image for the server's ecosystem.
func (r Resolved) Image(images policy.Images) string {
	if r.Source == "npm" {
		return images.NodeRuntime
	}
	return images.PythonRuntime
}

// sanitizeID maps a package name onto the [a-z0-9-] grammar catalog names
// and directory names share.
func sanitizeID(source, pkg string) string {
	var b strings.Builder
	b.WriteString(source)
	b.WriteByte('-')
	for _, ch := range strings.ToLower(pkg) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == '-' }), "-"), "-")
}

// IDFor is the store/catalog id a declared server resolves under —
// exported so callers can check the store before resolving.
func IDFor(srv manifest.MCPServer) string {
	switch {
	case srv.NPM != "":
		return sanitizeID("npm", srv.NPM)
	case srv.IsRemote():
		// The address without its scheme: host and path name the server.
		return sanitizeID(spec.SourceURL, strings.TrimPrefix(srv.URL, "https://"))
	}
	return sanitizeID("pypi", srv.PyPI)
}

// Packages is the declared servers that are packages: what is resolved
// against the registries and installed into the store. A remote server
// (agentry-cli#663) has neither; Specs renders it as it is declared.
func Packages(servers []manifest.MCPServer) []manifest.MCPServer {
	out := make([]manifest.MCPServer, 0, len(servers))
	for _, srv := range servers {
		if !srv.IsRemote() {
			out = append(out, srv)
		}
	}
	return out
}

// ResolveAll resolves every declared server against the configured
// registries. Pure metadata: no artifacts are fetched and no docker is
// touched, so --dry-run can afford it.
func ResolveAll(ctx context.Context, client *http.Client, regs policy.MCPRegistries, servers []manifest.MCPServer) ([]Resolved, error) {
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	servers = Packages(servers)
	out := make([]Resolved, 0, len(servers))
	for _, srv := range servers {
		pkg := srv.NPM
		if pkg == "" {
			pkg = srv.PyPI
		}
		if err := builderArgs(pkg, srv.Version); err != nil {
			return nil, notResolved(srv, fmt.Errorf("resolve %s@%s: %w", srv.SourceID(), srv.Version, &PinError{err}))
		}
		var r Resolved
		var err error
		if srv.NPM != "" {
			r, err = resolveNPM(ctx, client, regs.NPM, srv.NPM, srv.Version)
		} else {
			r, err = resolvePyPI(ctx, client, regs.PyPI, srv.PyPI, srv.Version)
		}
		if err != nil {
			return nil, notResolved(srv, fmt.Errorf("resolve %s@%s: %w", srv.SourceID(), srv.Version, err))
		}
		out = append(out, r)
	}
	return out, nil
}

// notResolved names the declared server a resolution failed for, in the
// run's reason (agentry-cli#628).
func notResolved(srv manifest.MCPServer, err error) error {
	return receipt.WithReason(err, receipt.ReasonMCPNotResolved, "", srv.SourceID())
}

// installFailed names the declared server and the install's step that
// failed, in the run's reason (agentry-cli#628).
func installFailed(r *Resolved, step string, err error) error {
	return receipt.WithReason(err, receipt.ReasonMCPInstallFailed, step, r.SourceID())
}

// PinError is a resolution that failed because of the pin itself, not
// the network or the registry's health: the release is not there, has no
// launchable entry or nothing to download, or its name or version is not
// one the builder passes on. A Drafter's rule refuses a draft for one;
// any other resolution error is the computer's (agentry-cli#609).
type PinError struct{ Err error }

func (e *PinError) Error() string { return e.Err.Error() }
func (e *PinError) Unwrap() error { return e.Err }

func getJSON(ctx context.Context, client *http.Client, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return &PinError{errors.New("not found in the configured registry")}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

func resolveNPM(ctx context.Context, client *http.Client, registry, pkg, version string) (Resolved, error) {
	var meta struct {
		Name    string          `json:"name"`
		Bin     json.RawMessage `json:"bin"`
		Engines json.RawMessage `json:"engines"`
		// Read leniently (npmNames): an old packument's odd shape here
		// must not fail a resolution a run depends on.
		Dependencies     json.RawMessage `json:"dependencies"`
		PeerDependencies json.RawMessage `json:"peerDependencies"`
		PeerMeta         json.RawMessage `json:"peerDependenciesMeta"`
		Scripts          json.RawMessage `json:"scripts"`
		Dist             struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
			Shasum    string `json:"shasum"`
		} `json:"dist"`
	}
	u := strings.TrimRight(registry, "/") + "/" + url.PathEscape(pkg) + "/" + url.PathEscape(version)
	if err := getJSON(ctx, client, u, &meta); err != nil {
		return Resolved{}, err
	}
	bin, err := npmBinName(pkg, meta.Bin)
	if err != nil {
		return Resolved{}, err
	}
	integrity := meta.Dist.Integrity
	if integrity == "" && meta.Dist.Shasum != "" {
		integrity = "sha1:" + meta.Dist.Shasum
	}
	return Resolved{
		Source: "npm", Package: pkg, Version: version,
		ID: sanitizeID("npm", pkg), BinName: bin, Integrity: integrity,
		FetchHosts:      fetchHosts(registry, meta.Dist.Tarball),
		DeclaredRuntime: npmEnginesNode(meta.Engines),
		Dependencies:    npmDirectDependencies(meta.Dependencies, meta.PeerDependencies, meta.PeerMeta),
		InstallScripts:  npmInstallScripts(meta.Scripts),
	}, nil
}

// npmDirectDependencies are the names a plain install pulls in directly:
// dependencies, and the peer dependencies npm 7 and later install unless
// peerDependenciesMeta marks them optional. Sorted and unique; a field of
// another shape than an object reads as empty.
func npmDirectDependencies(deps, peers, peerMeta json.RawMessage) []string {
	var optional map[string]struct {
		Optional bool `json:"optional"`
	}
	_ = json.Unmarshal(peerMeta, &optional)
	seen := map[string]bool{}
	for name := range npmObject(deps) {
		seen[name] = true
	}
	for name := range npmObject(peers) {
		if !optional[name].Optional {
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

// npmInstallScripts are the lifecycle scripts an npm install would run, in
// the order it runs them.
func npmInstallScripts(raw json.RawMessage) []string {
	scripts := npmObject(raw)
	var out []string
	for _, name := range []string{"preinstall", "install", "postinstall"} {
		var cmd string
		if json.Unmarshal(scripts[name], &cmd) == nil && strings.TrimSpace(cmd) != "" {
			out = append(out, name)
		}
	}
	return out
}

// npmObject is a packument field read as an object; nil for any other
// shape (an old package's array, a string).
func npmObject(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// npmEnginesNode is a packument version's engines.node, or "" when it has
// none. Some old packages publish engines as an array of strings, which npm
// ignores; so does this.
func npmEnginesNode(raw json.RawMessage) string {
	var e struct {
		Node string `json:"node"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	return strings.TrimSpace(e.Node)
}

// npmBinName picks the launchable bin: a string bin means "named after the
// package"; a map prefers the key matching the unscoped package name, then
// the lexicographically first — deterministic, and disclosed in the plan.
func npmBinName(pkg string, raw json.RawMessage) (string, error) {
	base := pkg
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if len(raw) == 0 {
		return "", &PinError{errors.New("package declares no bin — not launchable as an MCP stdio server")}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return base, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return "", &PinError{errors.New("package bin field is neither a path nor a name map")}
	}
	if _, ok := m[base]; ok {
		return base, nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names[0], nil
}

func resolvePyPI(ctx context.Context, client *http.Client, registry, pkg, version string) (Resolved, error) {
	var meta struct {
		Info struct {
			RequiresPython *string  `json:"requires_python"`
			RequiresDist   []string `json:"requires_dist"`
		} `json:"info"`
		URLs []struct {
			PackageType string `json:"packagetype"`
			Filename    string `json:"filename"`
			URL         string `json:"url"`
			Digests     struct {
				SHA256 string `json:"sha256"`
			} `json:"digests"`
		} `json:"urls"`
	}
	u := strings.TrimRight(registry, "/") + "/pypi/" + url.PathEscape(pkg) + "/" + url.PathEscape(version) + "/json"
	if err := getJSON(ctx, client, u, &meta); err != nil {
		return Resolved{}, err
	}
	integrity := ""
	artifacts := map[string]string{}
	// The drift pin prefers the pure-Python wheel (one file for every
	// platform), then any wheel, then any artifact; every advertised file
	// is recorded so the builder can verify the one pip actually picks.
	for _, art := range meta.URLs {
		if art.Filename != "" && art.Digests.SHA256 != "" {
			artifacts[art.Filename] = art.Digests.SHA256
		}
		if art.PackageType == "bdist_wheel" && art.Digests.SHA256 != "" && strings.HasSuffix(art.Filename, "-none-any.whl") {
			integrity = "sha256:" + art.Digests.SHA256
		}
	}
	if integrity == "" {
		for _, art := range meta.URLs {
			if art.PackageType == "bdist_wheel" && art.Digests.SHA256 != "" {
				integrity = "sha256:" + art.Digests.SHA256
				break
			}
		}
	}
	if integrity == "" {
		for _, art := range meta.URLs {
			if art.Digests.SHA256 != "" {
				integrity = "sha256:" + art.Digests.SHA256
				break
			}
		}
	}
	if len(meta.URLs) == 0 {
		return Resolved{}, &PinError{errors.New("release has no downloadable artifacts")}
	}
	urls := make([]string, 0, len(meta.URLs))
	for _, art := range meta.URLs {
		urls = append(urls, art.URL)
	}
	return Resolved{
		Source: "pypi", Package: pkg, Version: version,
		ID: sanitizeID("pypi", pkg),
		// Provisional (DR-11): the JSON API does not expose entry points,
		// so start from the normalized project name; the builder reads
		// the real console script from the installed dist (below).
		BinName:         NormalizePyPIName(pkg),
		Integrity:       integrity,
		Artifacts:       artifacts,
		FetchHosts:      fetchHosts(registry, urls...),
		DeclaredRuntime: requiresPython(meta.Info.RequiresPython),
		Dependencies:    pypiDirectDependencies(meta.Info.RequiresDist),
	}, nil
}

// pypiDirectDependencies are the project names of a release's
// requires_dist (PEP 508) that a plain install pulls in: a requirement
// whose marker names an extra is left out, and every other marker is kept,
// since the check says what may be installed, not what will. Names are
// normalized (PEP 503), sorted and unique.
func pypiDirectDependencies(reqs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, req := range reqs {
		name, marker, _ := strings.Cut(req, ";")
		if extraMarker.MatchString(marker) {
			continue
		}
		name = strings.TrimSpace(name)
		// The name ends where its extras, version or URL begin.
		if i := strings.IndexAny(name, "[(<>=!~@ \t"); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			continue
		}
		if n := NormalizePyPIName(name); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// extraMarker matches a PEP 508 marker that names an extra.
var extraMarker = regexp.MustCompile(`\bextra\s*==`)

func requiresPython(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// StoreDir is the signature's server store under its profile directory.
func StoreDir(profileDir string) string { return filepath.Join(profileDir, "servers") }

// metaPath is the per-server resolution record inside the store.
func metaPath(storeDir, id string) string { return filepath.Join(storeDir, id+".json") }

// LoadMeta reads a stored resolution; (nil, nil) when absent.
func LoadMeta(storeDir, id string) (*Resolved, error) {
	b, err := os.ReadFile(metaPath(storeDir, id)) //nolint:gosec // our store layout
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var r Resolved
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metaPath(storeDir, id), err)
	}
	return &r, nil
}

// EnsureInstalled materializes every resolved server into the store. A
// server whose recorded resolution matches is reused without network or
// docker; a recorded resolution that DISAGREES with the fresh one refuses
// — the pin is the trust decision, drift is the attack (RUNNER-PLAN-3
// §P2). Builder output streams to logw.
func EnsureInstalled(ctx context.Context, d dockerx.Client, resolved []Resolved,
	storeDir string, regs policy.MCPRegistries, images policy.Images, docker policy.DockerPolicy, logw io.Writer,
) error {
	lim := docker.Limits.WithDefaults()
	for i := range resolved {
		r := &resolved[i]
		prior, err := LoadMeta(storeDir, r.ID)
		if err != nil {
			return installFailed(r, receipt.InstallRecord, err)
		}
		if prior != nil {
			if prior.Package != r.Package || prior.Version != r.Version ||
				(prior.Integrity != "" && r.Integrity != "" && prior.Integrity != r.Integrity) {
				return installFailed(r, receipt.InstallPinChanged, fmt.Errorf("server store for %s holds %s@%s (%s) but the registry now resolves %s@%s (%s) — the recorded pin is the trust decision; remove the profile to accept the change",
					r.ID, prior.Package, prior.Version, prior.Integrity, r.Package, r.Version, r.Integrity))
			}
			if err := checkStore(storeDir, prior, logw); err != nil {
				return installFailed(r, receipt.InstallStoreChanged, err)
			}
			if prior.BinName != "" {
				// The store knows which console script the install
				// produced; the fresh resolution only has the guess.
				r.BinName = prior.BinName
			}
			r.StoreDigest = prior.StoreDigest
			_, _ = fmt.Fprintf(logw, "mcp server %s@%s already installed (reusing the signature's store)\n", r.Package, r.Version)
			continue
		}
		if err := builderArgs(r.Package, r.Version); err != nil {
			return installFailed(r, receipt.InstallBuilder, fmt.Errorf("install %s: %w", r.ID, err))
		}
		allow, err := installAllowlist(r, regs)
		if err != nil {
			return installFailed(r, receipt.InstallProxy, fmt.Errorf("install %s: %w", r.ID, err))
		}

		hostDir := filepath.Join(storeDir, r.ID)
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			return installFailed(r, receipt.InstallRecord, fmt.Errorf("create server store: %w", err))
		}
		_, _ = fmt.Fprintf(logw, "installing mcp server %s@%s (%s) into the signature store\n", r.Package, r.Version, r.Source)

		var argv []string
		var image string
		switch r.Source {
		case "npm":
			image = images.NodeRuntime
			// The pinned tarball verified against the integrity recorded at
			// resolution before anything is installed, then installed from
			// that verified file (agentry-cli#91, DR-13): npm_builder.js.
			argv = npmArgv(r, regs)
		case "pypi":
			image = images.PythonRuntime
			// Wheels only, and the pinned artifact verified against the
			// registry's advertised digest before anything is installed
			// (agentry-cli#4, DR-12/DR-13): `pip download --no-deps` fetches
			// exactly the declared package; the file it picked for this
			// platform must be one the JSON API listed, with that digest;
			// then the install runs from that verified file plus wheels
			// for the dependency closure. A source distribution would run
			// the package's own build code in the builder — refused unless
			// policy pypiAllowSourceDists says so.
			if err := writeArtifactSums(hostDir, r.Artifacts); err != nil {
				return installFailed(r, receipt.InstallRecord, err)
			}
			argv = pipArgv(r, regs)
		default:
			return fmt.Errorf("unknown server source %q", r.Source)
		}

		if ok, err := d.ImageExists(ctx, image); err != nil {
			return installFailed(r, receipt.InstallImage, err)
		} else if !ok {
			_, _ = fmt.Fprintf(logw, "pulling %s (first MCP server install)\n", image)
			if err := d.Pull(ctx, image); err != nil {
				return installFailed(r, receipt.InstallImage, fmt.Errorf("pull %s: %w", image, err))
			}
		}

		egress, err := startInstallProxy(ctx, d, r.ID, storeDir, allow, images, docker, logw)
		if err != nil {
			return installFailed(r, receipt.InstallProxy, fmt.Errorf("install %s: %w", r.ID, err))
		}
		hosts, err := egress.hosts(ctx, docker.Runtime)
		if err != nil {
			egress.stop(ctx, "")
			return installFailed(r, receipt.InstallProxy, fmt.Errorf("install %s: %w", r.ID, err))
		}
		code, err := d.RunForeground(ctx, dockerx.RunSpec{
			Name:  "agentry-mcpinstall-" + r.ID,
			Image: image,
			Argv:  argv,
			// The builder's only route out is the install proxy
			// (agentry-cli#92): npm, pip and the builders' own fetches
			// honour these, and the network has no other way off it.
			Env: egress.env(),
			// The store is the user's: the builder runs as the invoking
			// uid:gid so every file it writes is theirs and `rm -rf` of the
			// profile — what the drift refusal asks for — works
			// (agentry-cli#112). Hosts without POSIX ids leave it to the
			// image's own user.
			User: invokingUser(),
			// The install runs under the policy's runtime like every
			// container of a run (DR-23); "" is the daemon's default. The
			// proxy's alias is pinned for it (egress.hosts).
			Runtime:    docker.Runtime,
			ExtraHosts: hosts,

			// The name is fixed per server, so the container must go when
			// the build ends or the next agent declaring the same server
			// fails at install with a name conflict (agentry-cli#106).
			AutoRemove: true,
			// The install's own internal network (agentry-cli#92), where
			// the only other container is the proxy, whose allowlist is
			// the configured registry and the artifact hosts its metadata
			// named. It replaced the default bridge (agentry-cli#162),
			// which reached every host there is.
			Network: egress.network,
			// Who started it (package owner): the builder precedes any run
			// id, so it carries no agentry.run label, but a CLI killed
			// mid-install leaves it building with nobody waiting, and the
			// next run's sweep removes it by its dead owner
			// (agentry-cli#161).
			Labels: installLabels(r.ID),
			Mounts: []dockerx.Mount{
				// The server store is the runner's own directory under the
				// signature profile, so it may be relabeled (agentry-cli#85).
				{Source: hostDir, Target: r.prefix(), Relabel: true},
			},
			// The run's container posture (agentry-cli#163); its root stays
			// writable, since npm and pip write where they please.
		}.WithPosture(lim.Memory, lim.CPUs, lim.Pids), nil, logw, logw)
		// The proxy's allow/deny lines are the evidence of what the
		// install reached; kept beside the record, failed install or not.
		egress.stop(ctx, filepath.Join(storeDir, r.ID+installProxyLogSuffix))
		if err != nil {
			return installFailed(r, receipt.InstallBuilder, fmt.Errorf("install %s: %w", r.ID, err))
		}
		if r.Source == "npm" && code == npmEnginesRefused {
			// A verdict on the server's own range, not a failed step.
			return receipt.WithReason(fmt.Errorf("install %s: %s@%s declares an engines.node range that the Node in the runtime image %s does not satisfy (the builder's line above gives the range); nothing was installed. Pin a version of the server whose range it satisfies, or set the runner policy's images.nodeRuntime to an image with a Node it supports",
				r.ID, r.Package, r.Version, image), receipt.ReasonMCPRuntimeExcluded, "", r.SourceID())
		}
		if r.Source == "npm" && code == npmEnginesUnchecked {
			return installFailed(r, receipt.InstallRuntimeCheck, fmt.Errorf("install %s: the Node version %s@%s needs could not be checked in the runtime image %s (the builder's line above says why); nothing was installed",
				r.ID, r.Package, r.Version, image))
		}
		if code != 0 {
			return installFailed(r, receipt.InstallBuilder, fmt.Errorf("install %s: builder exited %d (the install proxy's decisions are in %s)", r.ID, code,
				filepath.Join(storeDir, r.ID+installProxyLogSuffix)))
		}
		r.InstallEgress = allow
		if r.Source == "pypi" {
			if err := adoptConsoleScript(hostDir, r, logw); err != nil {
				return installFailed(r, receipt.InstallBuilder, err)
			}
		}

		digest, err := storeDigest(hostDir)
		if err != nil {
			return installFailed(r, receipt.InstallRecord, fmt.Errorf("install %s: hash the server store: %w", r.ID, err))
		}
		r.StoreDigest = digest
		r.InstalledAt = time.Now().UTC().Format(time.RFC3339)
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return installFailed(r, receipt.InstallRecord, err)
		}
		if err := os.WriteFile(metaPath(storeDir, r.ID), append(b, '\n'), 0o600); err != nil {
			return installFailed(r, receipt.InstallRecord, fmt.Errorf("record server resolution: %w", err))
		}
	}
	return nil
}

// invokingUser is the uid:gid the install container runs as, or "" on a
// host without POSIX ids (the engine's stampOwner draws the same line).
func invokingUser() string {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// Specs turns resolutions into the plan's server specs, and each declared
// remote server into its own: an address, its auth, its tools and its one
// host, which the compiler runs the runner image's bridge for
// (agentry-cli#663).
func Specs(resolved []Resolved, servers []manifest.MCPServer, images policy.Images) []spec.MCPServerSpec {
	bySource := map[string]manifest.MCPServer{}
	for _, srv := range servers {
		bySource[srv.SourceID()] = srv
	}
	out := make([]spec.MCPServerSpec, 0, len(resolved))
	for _, r := range resolved {
		m := bySource[r.Source+":"+r.Package]
		out = append(out, spec.MCPServerSpec{
			Name:        r.ServerName(),
			Source:      r.Source,
			Package:     r.Package,
			Version:     r.Version,
			Integrity:   r.Integrity,
			Image:       r.Image(images),
			Command:     r.Command(),
			StoreSubdir: r.ID,
			Tools:       append([]string(nil), m.Tools...),
			Egress:      append([]string(nil), m.Egress...),
			Secrets:     append([]string(nil), m.Secrets...),
			Env:         copyEnv(m.Env),
		})
	}
	for _, srv := range servers {
		if !srv.IsRemote() {
			continue
		}
		out = append(out, spec.MCPServerSpec{
			Name:    "mcp-" + IDFor(srv),
			Source:  spec.SourceURL,
			Package: srv.URL,
			Tools:   append([]string(nil), srv.Tools...),
			Egress:  []string{srv.RemoteHost() + ":443"}, // the one port the bridge reaches
			Secrets: append([]string(nil), srv.Secrets...),
			Auth:    string(srv.Auth),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// copyEnv returns a copy of a manifest env map, nil when empty so a plan
// without env stays byte-identical to one written before env existed.
func copyEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	return maps.Clone(env)
}

// artifactSumsFile is the `sha256sum -c`-shaped list of every artifact the
// registry advertised for the pinned release, written into the server's
// store directory (mounted at the prefix) for the builder to check against.
const artifactSumsFile = "artifacts.sha256"

func writeArtifactSums(hostDir string, artifacts map[string]string) error {
	names := make([]string, 0, len(artifacts))
	for n := range artifacts {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(artifacts[n] + "  " + n + "\n")
	}
	if err := os.WriteFile(filepath.Join(hostDir, artifactSumsFile), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write artifact digests: %w", err)
	}
	return nil
}

// consoleScriptsFile is where the PyPI builder leaves the console scripts
// the installed distribution declares (one name per line, from its
// entry_points metadata), read back by adoptConsoleScript.
const consoleScriptsFile = "console-scripts"

// NormalizePyPIName applies PEP 503 normalization: lower-case, with every
// run of '-', '_' or '.' collapsed to a single '-'.
func NormalizePyPIName(pkg string) string {
	var b strings.Builder
	dash := false
	for _, ch := range strings.ToLower(pkg) {
		if ch == '-' || ch == '_' || ch == '.' {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteRune(ch)
	}
	return b.String()
}

// chooseConsoleScript picks the launchable entry among the console scripts
// a PyPI distribution declares: the one named like the project wins, a
// sole script is taken as-is, and otherwise the first in sorted order
// (deterministic, and reported so the choice is visible in the log). ok is
// false when the dist declares no console script at all.
func chooseConsoleScript(pkg string, names []string) (name string, ok bool) {
	clean := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 {
		return "", false
	}
	want := NormalizePyPIName(pkg)
	for _, n := range clean {
		if NormalizePyPIName(n) == want {
			return n, true
		}
	}
	sort.Strings(clean)
	return clean[0], true
}

// adoptConsoleScript reads the console scripts the builder recorded for the
// installed distribution and sets r.BinName to the launchable one. A dist
// with no console script keeps the DR-11 guess (it will fail at gateway
// health, as before) but says so in the log.
func adoptConsoleScript(hostDir string, r *Resolved, logw io.Writer) error {
	b, err := os.ReadFile(filepath.Join(hostDir, consoleScriptsFile)) //nolint:gosec // our store layout
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("install %s: read console scripts: %w", r.ID, err)
	}
	names := strings.Split(strings.TrimSpace(string(b)), "\n")
	name, ok := chooseConsoleScript(r.Package, names)
	if !ok {
		_, _ = fmt.Fprintf(logw, "mcp server %s@%s declares no console script; launching %s (may fail at gateway health)\n", r.Package, r.Version, r.BinName)
		return nil
	}
	if len(names) > 1 {
		_, _ = fmt.Fprintf(logw, "mcp server %s@%s declares console scripts %s; launching %s\n", r.Package, r.Version, strings.Join(names, ", "), name)
	}
	r.BinName = name
	return nil
}

// pypiBuilder is the PyPI builder's program (pypi_builder.py), run as
// `python -c` with every value a separate argument: nothing a manifest
// says passes through a shell (agentry-cli#162).
//
//go:embed pypi_builder.py
var pypiBuilder string

// pipArgv is the builder's argv for a PyPI server (see the pypi case in
// EnsureInstalled).
func pipArgv(r *Resolved, regs policy.MCPRegistries) []string {
	onlyBinary := "--only-binary=:all:"
	if regs.PyPIAllowSourceDists {
		onlyBinary = ""
	}
	return []string{"python", "-c", pypiBuilder,
		r.prefix(), r.Package, r.Version, strings.TrimRight(regs.PyPI, "/") + "/simple", onlyBinary}
}

// npm_builder.js's exits for the engines check (agentry-cli#608): the
// pinned package's engines.node excludes the runtime image's Node (or is
// no range at all), or the check could not run (an unreadable tarball, or
// an image whose npm carries no semver).
const (
	npmEnginesRefused   = 5
	npmEnginesUnchecked = 6
)

// npmBuilder is the npm builder's program (npm_builder.js), run as
// `node -e` with every value a separate argument, like the PyPI builder.
//
//go:embed npm_builder.js
var npmBuilder string

// npmDownloadDir is where npm_builder.js leaves the verified tarball,
// under the server's store directory; the install is from that file.
const npmDownloadDir = "dl"

// npmArgv is the builder's argv for an npm server. It runs as the invoking
// user (see EnsureInstalled), whose HOME does not exist in the image: npm's
// cache and config go under /tmp.
func npmArgv(r *Resolved, regs policy.MCPRegistries) []string {
	return []string{"env", "HOME=/tmp", "npm_config_cache=/tmp/npm-cache",
		"node", "-e", npmBuilder,
		r.prefix(), r.Package, r.Version, regs.NPM, r.Integrity}
}

// builderArgs refuses a package or version the builder could not pass to
// pip or npm as the plain value it must be: one that starts with "-" (an
// option to either tool), or that holds a character no npm name, PyPI
// name, exact semver or exact PEP 440 release has, such as the "file:" or
// URL forms of an npm spec, which would install something other than the
// pinned registry artifact. The manifest's own grammar (V5) is the
// registry's check at publish; this one holds at the builder whatever the
// manifest came through (agentry-cli#162).
func builderArgs(pkg, version string) error {
	if !builderValue(pkg, "@/._-") {
		return fmt.Errorf("package %q is not a name the MCP-server builder will pass on", pkg)
	}
	if !builderValue(version, ".+-") {
		return fmt.Errorf("version %q is not an exact release the MCP-server builder will pass on", version)
	}
	return nil
}

func builderValue(v, punct string) bool {
	if v == "" || strings.HasPrefix(v, "-") {
		return false
	}
	for _, c := range v {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune(punct, c)
		if !ok {
			return false
		}
	}
	return true
}

// storeDigest hashes a server's store tree: every path in lexical order,
// with a regular file's executable bits and content digest and a
// symlink's target (a venv links its interpreter). A reuse recomputes it
// and refuses any difference (agentry-cli#162).
func storeDigest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "L %q %q\n", rel, target)
		case t.IsDir():
			_, _ = fmt.Fprintf(h, "D %q\n", rel)
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			sum, err := fileSHA256(p)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "F %q %o %s\n", rel, info.Mode().Perm()&0o111, sum)
		default:
			_, _ = fmt.Fprintf(h, "O %q %s\n", rel, t)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a path inside our own store
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkStore holds a reused server store to the digest its install
// recorded: a difference refuses. A record written before stores were
// hashed gets today's digest, said in the log, and is held to it from
// then on.
func checkStore(storeDir string, prior *Resolved, logw io.Writer) error {
	got, err := storeDigest(filepath.Join(storeDir, prior.ID))
	if err != nil {
		return fmt.Errorf("server store for %s: %w", prior.ID, err)
	}
	if prior.StoreDigest == "" {
		prior.StoreDigest = got
		b, err := json.MarshalIndent(prior, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(metaPath(storeDir, prior.ID), append(b, '\n'), 0o600); err != nil {
			return fmt.Errorf("record server store digest: %w", err)
		}
		_, _ = fmt.Fprintf(logw, "mcp server %s@%s: recorded the store's digest %s (first check of a store installed before digests were kept)\n", prior.Package, prior.Version, got)
		return nil
	}
	if got != prior.StoreDigest {
		return fmt.Errorf("server store for %s changed since it was installed (recorded %s, now %s) — something wrote into the signature's store; remove the profile to reinstall", prior.ID, prior.StoreDigest, got)
	}
	return nil
}

// installLabels are the installer container's labels: the server it builds
// and the owner labels of this process (none where owner has no identity).
func installLabels(serverID string) map[string]string {
	return owner.With(map[string]string{"agentry.mcpinstall": serverID})
}
