// Package mcpinstall resolves and installs the MCP servers a BriefAgent
// declares (RUNNER-PLAN-3): resolution turns each manifest pin into the
// exact registry artifact (npm / PyPI metadata, integrity recorded);
// installation materializes the dependency closure into the signature's
// server store inside a pinned runtime container. The store lives under
// the release's profile directory, which is what makes every isolation
// property hold: attachment is derived from the signature, so no other
// BriefAgent, user, or version can ever reference it.
package mcpinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
}

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
	if srv.NPM != "" {
		return sanitizeID("npm", srv.NPM)
	}
	return sanitizeID("pypi", srv.PyPI)
}

// ResolveAll resolves every declared server against the configured
// registries. Pure metadata: no artifacts are fetched and no docker is
// touched, so --dry-run can afford it.
func ResolveAll(ctx context.Context, client *http.Client, regs policy.MCPRegistries, servers []manifest.MCPServer) ([]Resolved, error) {
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	out := make([]Resolved, 0, len(servers))
	for _, srv := range servers {
		var r Resolved
		var err error
		if srv.NPM != "" {
			r, err = resolveNPM(ctx, client, regs.NPM, srv.NPM, srv.Version)
		} else {
			r, err = resolvePyPI(ctx, client, regs.PyPI, srv.PyPI, srv.Version)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve %s@%s: %w", srv.SourceID(), srv.Version, err)
		}
		out = append(out, r)
	}
	return out, nil
}

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
		return errors.New("not found in the configured registry")
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
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
		Dist struct {
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
	}, nil
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
		return "", errors.New("package declares no bin — not launchable as an MCP stdio server")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return base, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return "", errors.New("package bin field is neither a path nor a name map")
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
		URLs []struct {
			PackageType string `json:"packagetype"`
			Filename    string `json:"filename"`
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
		return Resolved{}, errors.New("release has no downloadable artifacts")
	}
	return Resolved{
		Source: "pypi", Package: pkg, Version: version,
		ID: sanitizeID("pypi", pkg),
		// Provisional (DR-11): the JSON API does not expose entry points,
		// so start from the normalized project name; the builder reads
		// the real console script from the installed dist (below).
		BinName:   normalizePyPIName(pkg),
		Integrity: integrity,
		Artifacts: artifacts,
	}, nil
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
	storeDir string, regs policy.MCPRegistries, images policy.Images, logw io.Writer,
) error {
	for i := range resolved {
		r := &resolved[i]
		prior, err := LoadMeta(storeDir, r.ID)
		if err != nil {
			return err
		}
		if prior != nil {
			if prior.Package != r.Package || prior.Version != r.Version ||
				(prior.Integrity != "" && r.Integrity != "" && prior.Integrity != r.Integrity) {
				return fmt.Errorf("server store for %s holds %s@%s (%s) but the registry now resolves %s@%s (%s) — the recorded pin is the trust decision; remove the profile to accept the change",
					r.ID, prior.Package, prior.Version, prior.Integrity, r.Package, r.Version, r.Integrity)
			}
			if prior.BinName != "" {
				// The store knows which console script the install
				// produced; the fresh resolution only has the guess.
				r.BinName = prior.BinName
			}
			_, _ = fmt.Fprintf(logw, "mcp server %s@%s already installed (reusing the signature's store)\n", r.Package, r.Version)
			continue
		}

		hostDir := filepath.Join(storeDir, r.ID)
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			return fmt.Errorf("create server store: %w", err)
		}
		_, _ = fmt.Fprintf(logw, "installing mcp server %s@%s (%s) into the signature store\n", r.Package, r.Version, r.Source)

		var argv []string
		var image string
		switch r.Source {
		case "npm":
			image = images.NodeRuntime
			// As the invoking user (below), whose HOME does not exist in
			// the image: npm's cache and config go under /tmp.
			argv = []string{"env", "HOME=/tmp", "npm_config_cache=/tmp/npm-cache", "npm", "install",
				"--prefix", r.prefix(), r.Package + "@" + r.Version,
				"--registry", regs.NPM,
				"--no-audit", "--no-fund", "--ignore-scripts", "--loglevel", "error"}
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
				return err
			}
			argv = []string{"sh", "-c", pipScript(r, regs)}
		default:
			return fmt.Errorf("unknown server source %q", r.Source)
		}

		if ok, err := d.ImageExists(ctx, image); err != nil {
			return err
		} else if !ok {
			_, _ = fmt.Fprintf(logw, "pulling %s (first MCP server install)\n", image)
			if err := d.Pull(ctx, image); err != nil {
				return fmt.Errorf("pull %s: %w", image, err)
			}
		}

		code, err := d.RunForeground(ctx, dockerx.RunSpec{
			Name:  "agentry-mcpinstall-" + r.ID,
			Image: image,
			Argv:  argv,
			// The store is the user's: the builder runs as the invoking
			// uid:gid so every file it writes is theirs and `rm -rf` of the
			// profile — what the drift refusal asks for — works
			// (agentry-cli#112). Hosts without POSIX ids leave it to the
			// image's own user.
			User: invokingUser(),
			// The name is fixed per server, so the container must go when
			// the build ends or the next agent declaring the same server
			// fails at install with a name conflict (agentry-cli#106).
			AutoRemove: true,
			Mounts: []dockerx.Mount{
				// The server store is the runner's own directory under the
				// signature profile, so it may be relabeled (agentry-cli#85).
				{Source: hostDir, Target: r.prefix(), Relabel: true},
			},
		}, nil, logw, logw)
		if err != nil {
			return fmt.Errorf("install %s: %w", r.ID, err)
		}
		if code != 0 {
			return fmt.Errorf("install %s: builder exited %d", r.ID, code)
		}
		if r.Source == "pypi" {
			if err := adoptConsoleScript(hostDir, r, logw); err != nil {
				return err
			}
		}

		r.InstalledAt = time.Now().UTC().Format(time.RFC3339)
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(metaPath(storeDir, r.ID), append(b, '\n'), 0o600); err != nil {
			return fmt.Errorf("record server resolution: %w", err)
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

// Specs turns resolutions into the plan's server specs.
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

// normalizePyPIName applies PEP 503 normalization: lower-case, with every
// run of '-', '_' or '.' collapsed to a single '-'.
func normalizePyPIName(pkg string) string {
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
	want := normalizePyPIName(pkg)
	for _, n := range clean {
		if normalizePyPIName(n) == want {
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

// pipScript is the builder's shell for a PyPI server (see the pypi case in
// EnsureInstalled). Kept as one string so the test can pin its shape.
func pipScript(r *Resolved, regs policy.MCPRegistries) string {
	p := r.prefix()
	idx := strings.TrimRight(regs.PyPI, "/") + "/simple"
	onlyBinary := "--only-binary=:all:"
	if regs.PyPIAllowSourceDists {
		onlyBinary = ""
	}
	return strings.Join([]string{
		"set -e",
		// as the invoking user, whose HOME does not exist in the image
		"export HOME=/tmp",
		fmt.Sprintf("python -m venv %s/venv", p),
		fmt.Sprintf("mkdir -p %s/dl", p),
		fmt.Sprintf("%s/venv/bin/pip download --quiet --no-cache-dir --no-deps %s --index-url %s --dest %s/dl %s==%s", p, onlyBinary, idx, p, r.Package, r.Version),
		fmt.Sprintf(`for f in %s/dl/*; do n=$(basename "$f"); want=$(grep " $n$" %s/%s | cut -d" " -f1); `+
			`[ -n "$want" ] || { echo "mcp install: $n is not an artifact the registry advertised for %s==%s"; exit 3; }; `+
			`echo "$want  $f" | sha256sum -c - >/dev/null || { echo "mcp install: $n does not match the advertised digest"; exit 4; }; done`,
			p, p, artifactSumsFile, r.Package, r.Version),
		fmt.Sprintf("%s/venv/bin/pip install --quiet --no-cache-dir %s --index-url %s --find-links %s/dl %s==%s", p, onlyBinary, idx, p, r.Package, r.Version),
		// Record the console scripts the installed dist declares
		// (agentry-cli#13): the host picks the launchable one from
		// this file rather than assuming the project name.
		fmt.Sprintf(`%s/venv/bin/python -c 'import importlib.metadata as m, sys; eps = sorted(e.name for e in m.distribution(sys.argv[1]).entry_points if e.group == "console_scripts"); open(sys.argv[2], "w").write("\n".join(eps) + "\n")' %s %s/%s`,
			p, r.Package, p, consoleScriptsFile),
	}, " && ")
}
