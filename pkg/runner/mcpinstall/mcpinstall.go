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
	// console script (assumed = the normalized project name, DR-11).
	BinName string `json:"binName"`
	// Integrity is what the registry advertised for the artifact (npm SRI
	// or "sha256:<hex>" for PyPI).
	Integrity string `json:"integrity,omitempty"`
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
	// Prefer the wheel's digest; fall back to any artifact.
	for _, art := range meta.URLs {
		if art.PackageType == "bdist_wheel" && art.Digests.SHA256 != "" {
			integrity = "sha256:" + art.Digests.SHA256
			break
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
		// DR-11: the console script is assumed to equal the normalized
		// project name (the JSON API does not expose entry points).
		BinName:   pkg,
		Integrity: integrity,
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
			argv = []string{"npm", "install",
				"--prefix", r.prefix(), r.Package + "@" + r.Version,
				"--registry", regs.NPM,
				"--no-audit", "--no-fund", "--ignore-scripts", "--loglevel", "error"}
		case "pypi":
			image = images.PythonRuntime
			argv = []string{"sh", "-c",
				fmt.Sprintf("python -m venv %s/venv && %s/venv/bin/pip install --quiet --index-url %s/simple %s==%s",
					r.prefix(), r.prefix(), strings.TrimRight(regs.PyPI, "/"), r.Package, r.Version)}
		default:
			return fmt.Errorf("unknown server source %q", r.Source)
		}

		if ok, err := d.ImageExists(ctx, image); err != nil {
			return err
		} else if !ok {
			_, _ = fmt.Fprintf(logw, "pulling %s (first mcp-server install)\n", image)
			if err := d.Pull(ctx, image); err != nil {
				return fmt.Errorf("pull %s: %w", image, err)
			}
		}

		code, err := d.RunForeground(ctx, dockerx.RunSpec{
			Name:  "agentry-mcpinstall-" + r.ID,
			Image: image,
			Argv:  argv,
			Mounts: []dockerx.Mount{
				{Source: hostDir, Target: r.prefix()},
			},
		}, nil, logw, logw)
		if err != nil {
			return fmt.Errorf("install %s: %w", r.ID, err)
		}
		if code != 0 {
			return fmt.Errorf("install %s: builder exited %d", r.ID, code)
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
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
