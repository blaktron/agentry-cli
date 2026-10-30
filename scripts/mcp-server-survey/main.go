// Command mcp-server-survey installs and starts declared MCP servers the way
// the runner does, and records what each one did: which version admits the
// default runtime, whether it installs, whether it starts and lists its
// tools, and what one keyless read-only call returned. Its results seed the
// drafting guide's soft lists, Runs here and Does not run here
// (pkg/drafter/knowledge/drafting-guide/sources/servers.yaml). README.md
// beside it is the procedure.
//
// It reuses the runner's own parts: the resolver and the builder
// (mcpinstall.ResolveAll, EnsureInstalled) in the default runtime images
// behind the install proxy, and the pinned MCP gateway launching each server
// from its read-only store with the catalog entry a run renders
// (compile.DeclaredServerGatewayFiles), a run's guards
// (compile.GatewayGuardFlags), its egress as TCP relays on port 443, and a
// run's container posture. Where its gateway differs from a run's, it is
// looser, never stricter, so a server that fails here fails in a run too:
// it speaks stdio rather than streaming HTTP, so the survey talks MCP to it
// directly; it reaches the Docker daemon's own socket, where a run's
// gateway on Linux goes through the run's filter (engine's daemon filter);
// and it uses the daemon's default runtime, where a
// hosted run's may be gVisor.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// entry is one server of the survey's list (list.yaml).
type entry struct {
	Ecosystem string `yaml:"ecosystem"`
	Package   string `yaml:"package"`
	// Version pins the version to survey; empty takes the newest that
	// admits the default runtime.
	Version string `yaml:"version,omitempty"`
	// Why names the source that put it on the list (README, "The list").
	Why string `yaml:"why"`
	// Egress, Env and Secrets are what a QuickAgent would declare for it.
	// A secret is given a placeholder value, never a real key, so a
	// server that wants one at start can start and list its tools.
	Egress  []string          `yaml:"egress,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Secrets []string          `yaml:"secrets,omitempty"`
	// Call is one read-only tool call that needs no key.
	Call *call `yaml:"call,omitempty"`
	// Note is the reason no call is listed, or anything else the
	// procedure should carry into the results.
	Note string `yaml:"note,omitempty"`
}

type call struct {
	Tool      string         `yaml:"tool" json:"tool"`
	Arguments map[string]any `yaml:"arguments,omitempty" json:"arguments,omitempty"`
}

// result is what the survey observed for one server.
type result struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version,omitempty"`
	Why       string `json:"why"`
	Date      string `json:"date"`
	Image     string `json:"image"`
	Runtime   string `json:"runtime"`
	// Excluded are newer versions passed over because their own
	// runtime range excludes the runtime.
	Excluded []string `json:"excluded,omitempty"`
	// DirectDependencies are the version's own dependencies (npm
	// dependencies, PyPI requires_dist without an extra marker).
	DirectDependencies []string `json:"directDependencies,omitempty"`
	// InstallScripts are the npm lifecycle scripts the version declares,
	// which --ignore-scripts skips.
	InstallScripts []string `json:"installScripts,omitempty"`
	// Stage is the furthest stage reached: resolve, install, start,
	// list, call.
	Stage   string      `json:"stage"`
	Outcome string      `json:"outcome"`
	Error   string      `json:"error,omitempty"`
	Tools   []string    `json:"tools,omitempty"`
	Call    *callResult `json:"call,omitempty"`
	Note    string      `json:"note,omitempty"`
	Log     string      `json:"log"`
	Seconds float64     `json:"seconds"`
}

type callResult struct {
	call
	IsError bool   `json:"isError"`
	Excerpt string `json:"excerpt"`
}

func main() {
	list := flag.String("list", "list.yaml", "the servers to survey")
	out := flag.String("out", ".", "where results, logs and server stores go: a directory outside any git work tree")
	only := flag.String("only", "", "survey only the entries whose package is in this comma-separated list")
	flag.Parse()
	if err := run(*list, *out, *only); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-server-survey:", err)
		os.Exit(1)
	}
}

func run(listPath, outDir, only string) error {
	raw, err := os.ReadFile(listPath) //nolint:gosec // the list is the caller's
	if err != nil {
		return err
	}
	var entries []entry
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("%s: %w", listPath, err)
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Ecosystem+":"+e.Package] = true
	}
	if only != "" {
		keep := strings.Split(only, ",")
		for _, k := range keep {
			if !slices.ContainsFunc(entries, func(e entry) bool { return e.Package == k }) {
				return fmt.Errorf("-only: %s is not on %s", k, listPath)
			}
		}
		entries = slices.DeleteFunc(entries, func(e entry) bool { return !slices.Contains(keep, e.Package) })
	}
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return err
	}
	// Its server stores and logs are throwaway, never a checkout's content
	// (README, "Running it").
	if inside, _ := exec.CommandContext(context.Background(), "git", "-C", outDir, "rev-parse", "--is-inside-work-tree").Output(); strings.TrimSpace(string(inside)) == "true" { //nolint:gosec // a fixed command
		return fmt.Errorf("%s is inside a git work tree; give -out a directory outside any work tree", outDir)
	}
	for _, sub := range []string{"store", "logs", "gateway"} {
		if err := os.MkdirAll(filepath.Join(outDir, sub), 0o700); err != nil {
			return err
		}
	}
	ctx := context.Background()
	d := dockerx.New()
	pol := policy.Default()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir, err := os.MkdirTemp(outDir, "runner-image-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(ctxDir) }()
	// The gateway image and the runner image, whose proxy the builder
	// installs through, as `agentry run` makes them.
	if err := gateway.EnsureInstalled(ctx, d, pol, runnerTag, ctxDir, os.Stderr, false); err != nil {
		return err
	}
	date := time.Now().UTC().Format(time.DateOnly)
	if err := runtimes(ctx, d, pol, outDir, date); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	// A second pass the same day (-only, after calls were added to the
	// list) replaces its servers' results and keeps the others.
	var results []result
	if b, err := os.ReadFile(filepath.Join(outDir, "results-"+date+".json")); err == nil { //nolint:gosec // the survey's own output
		if err := json.Unmarshal(b, &results); err != nil {
			return err
		}
		// A server taken off the list leaves the results with it.
		results = slices.DeleteFunc(results, func(r result) bool { return !listed[r.Ecosystem+":"+r.Package] })
	}
	for i, e := range entries {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s:%s\n", i+1, len(entries), e.Ecosystem, e.Package)
		r := survey(ctx, d, client, pol, outDir, date, e)
		fmt.Fprintf(os.Stderr, "    %s\n", r.Outcome)
		results = slices.DeleteFunc(results, func(o result) bool { return o.Ecosystem == r.Ecosystem && o.Package == r.Package })
		results = append(results, r)
		// Written after every server, so a long run interrupted keeps
		// what it has.
		if err := write(outDir, date, results); err != nil {
			return err
		}
	}
	return nil
}

// runtimeProbe is what a server in each default runtime image starts as:
// the gateway launches a server with no --user, so as the image's own
// user, with the image's HOME, on a writable root (a run mounts only the
// server's own store read-only). It also lists which of the programs
// servers most often spawn the image has.
const runtimeProbe = `echo "user=$(id -un) uid=$(id -u) gid=$(id -g)"; echo "home=$HOME"; ` +
	`(touch "$HOME/.survey" 2>/dev/null && echo home-writable=yes) || echo home-writable=no; ` +
	`(touch /tmp/.survey 2>/dev/null && echo tmp-writable=yes) || echo tmp-writable=no; ` +
	`for p in git curl wget python3 node npx uvx docker chromium; do command -v "$p" >/dev/null 2>&1 && echo "has=$p"; done; true`

// runtimes records runtimeProbe's answer for both default runtime images,
// in runtimes-<date>.txt (the images' default user and HOME).
func runtimes(ctx context.Context, d dockerx.Client, pol *policy.Policy, outDir, date string) error {
	var b strings.Builder
	for _, image := range []string{pol.Images.NodeRuntime, pol.Images.PythonRuntime} {
		if ok, err := d.ImageExists(ctx, image); err == nil && !ok {
			if err := d.Pull(ctx, image); err != nil {
				return err
			}
		}
		var out strings.Builder
		code, err := d.RunForeground(ctx, dockerx.RunSpec{
			Name: "agentry-survey-runtime", Image: image, Entrypoint: "sh",
			Argv: []string{"-c", runtimeProbe}, AutoRemove: true, Network: "none",
		}, nil, &out, &out)
		if err != nil || code != 0 {
			return fmt.Errorf("probe %s: exit %d: %v %s", image, code, err, out.String())
		}
		fmt.Fprintf(&b, "## %s (%s)\n%s\n", image, date, out.String())
	}
	return os.WriteFile(filepath.Join(outDir, "runtimes-"+date+".txt"), []byte(b.String()), 0o600)
}

func write(outDir, date string, results []result) error {
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "results-"+date+".json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# MCP server survey, %s\n\n| server | version | stage | outcome |\n| --- | --- | --- | --- |\n", date)
	for _, r := range results {
		fmt.Fprintf(&md, "| %s:%s | %s | %s | %s |\n", r.Ecosystem, r.Package, r.Version, r.Stage, strings.ReplaceAll(r.Outcome, "|", "/"))
	}
	return os.WriteFile(filepath.Join(outDir, "results-"+date+".md"), []byte(md.String()), 0o600)
}

func survey(ctx context.Context, d dockerx.Client, client *http.Client, pol *policy.Policy, outDir, date string, e entry) (r result) {
	start := time.Now()
	r = result{Ecosystem: e.Ecosystem, Package: e.Package, Why: e.Why, Date: date, Note: e.Note, Stage: "resolve"}
	defer func() { r.Seconds = time.Since(start).Round(time.Second).Seconds() }()
	switch e.Ecosystem {
	case "npm":
		r.Image, r.Runtime = pol.Images.NodeRuntime, "Node "+policy.DefaultNodeVersion
	case "pypi":
		r.Image, r.Runtime = pol.Images.PythonRuntime, "Python "+policy.DefaultPythonVersion
	default:
		r.Outcome = "unknown ecosystem " + e.Ecosystem
		return r
	}
	logPath := filepath.Join(outDir, "logs", strings.NewReplacer("/", "_", "@", "").Replace(e.Ecosystem+"-"+e.Package)+".log")
	r.Log = logPath
	logf, err := os.Create(logPath) //nolint:gosec // the survey's own output
	if err != nil {
		r.Outcome, r.Error = "could not open its log", err.Error()
		return r
	}
	defer func() { _ = logf.Close() }()

	// Resolve: the newest version whose own range admits the runtime.
	pick, err := choose(ctx, client, pol.MCPRegistries, e, &r)
	if err != nil {
		r.Outcome, r.Error = "did not resolve", err.Error()
		return r
	}
	r.Version = pick
	srv := manifest.MCPServer{Version: pick, Egress: e.Egress, Env: e.Env, Secrets: e.Secrets}
	if e.Ecosystem == "npm" {
		srv.NPM = e.Package
	} else {
		srv.PyPI = e.Package
	}
	resolved, err := mcpinstall.ResolveAll(ctx, client, pol.MCPRegistries, []manifest.MCPServer{srv})
	if err != nil {
		r.Outcome, r.Error = "did not resolve", err.Error()
		return r
	}

	// Install, with the runner's builder, into a store of the survey's own.
	r.Stage = "install"
	storeDir := filepath.Join(outDir, "store")
	_ = os.RemoveAll(filepath.Join(storeDir, resolved[0].ID))
	_ = os.Remove(filepath.Join(storeDir, resolved[0].ID+".json"))
	if err := mcpinstall.EnsureInstalled(ctx, d, resolved, storeDir, pol.MCPRegistries, pol.Images, pol.Docker, logf); err != nil {
		r.Outcome, r.Error = "did not install", tail(err.Error(), 600)
		return r
	}

	if e.Ecosystem == "pypi" {
		// The install chose the Python from the release's own range.
		r.Image, r.Runtime = resolved[0].Image(pol.Images), resolved[0].Runtime(pol.Images)
	}

	// Start it through the gateway and list its tools.
	r.Stage = "start"
	specs := mcpinstall.Specs(resolved, []manifest.MCPServer{srv}, pol.Images)
	tools, cr, stage, err := launch(ctx, d, pol, outDir, storeDir, specs[0], e, logf)
	r.Tools = tools
	r.Call = cr
	if stage != "" {
		r.Stage = stage
	}
	switch {
	case err != nil && len(tools) == 0:
		r.Outcome, r.Error = "installed; did not start or list tools", tail(err.Error(), 600)
	case err != nil:
		r.Outcome, r.Error = fmt.Sprintf("started and listed %d tools; the call failed", len(tools)), tail(err.Error(), 600)
	case cr == nil:
		r.Outcome = fmt.Sprintf("started and listed %d tools, not called", len(tools))
	case cr.IsError:
		r.Outcome = fmt.Sprintf("started and listed %d tools; %s returned an error", len(tools), cr.Tool)
	default:
		r.Outcome = fmt.Sprintf("started, listed %d tools and called %s", len(tools), cr.Tool)
	}
	return r
}

// choose picks the version to survey and fills in the version's own facts
// the lists use (its direct dependencies and its install scripts).
func choose(ctx context.Context, client *http.Client, regs policy.MCPRegistries, e entry, r *result) (string, error) {
	if e.Ecosystem == "npm" {
		return chooseNPM(ctx, client, regs.NPM, e, r)
	}
	return choosePyPI(ctx, client, regs.PyPI, e, r)
}

type npmVersion struct {
	Engines      json.RawMessage   `json:"engines"`
	Dependencies map[string]string `json:"dependencies"`
	Scripts      map[string]string `json:"scripts"`
	Deprecated   json.RawMessage   `json:"deprecated"`
}

func chooseNPM(ctx context.Context, client *http.Client, registry string, e entry, r *result) (string, error) {
	var doc struct {
		DistTags map[string]string     `json:"dist-tags"`
		Versions map[string]npmVersion `json:"versions"`
	}
	if err := getJSON(ctx, client, strings.TrimRight(registry, "/")+"/"+url.PathEscape(e.Package), &doc); err != nil {
		return "", err
	}
	candidates := npmCandidates(doc.DistTags["latest"], doc.Versions, e.Version)
	for _, v := range candidates {
		meta := doc.Versions[v]
		var eng struct {
			Node string `json:"node"`
		}
		_ = json.Unmarshal(meta.Engines, &eng)
		why := mcpinstall.RuntimeMismatch(mcpinstall.Resolved{Source: "npm", DeclaredRuntime: strings.TrimSpace(eng.Node)}, policy.DefaultNodeVersion, policy.DefaultPythonVersion)
		if why != "" {
			r.Excluded = append(r.Excluded, v+": "+why)
			continue
		}
		r.DirectDependencies = slices.Sorted(maps.Keys(meta.Dependencies))
		for _, s := range []string{"preinstall", "install", "postinstall", "prepare"} {
			if _, ok := meta.Scripts[s]; ok {
				r.InstallScripts = append(r.InstallScripts, s+": "+tail(meta.Scripts[s], 120))
			}
		}
		return v, nil
	}
	return "", errors.New("no published version admits Node " + policy.DefaultNodeVersion)
}

// npmCandidates orders the versions to try: the pin alone when there is
// one; otherwise the latest tag, then every other release newest first
// (pre-releases only when the latest tag is one).
func npmCandidates(latest string, versions map[string]npmVersion, pin string) []string {
	if pin != "" {
		return []string{pin}
	}
	var vs []*semver.Version
	allowPre := false
	if lv, err := semver.NewVersion(latest); err == nil && lv.Prerelease() != "" {
		allowPre = true
	}
	for s := range versions {
		v, err := semver.NewVersion(s)
		if err != nil || (v.Prerelease() != "" && !allowPre) {
			continue
		}
		vs = append(vs, v)
	}
	sort.Sort(sort.Reverse(semver.Collection(vs)))
	out := []string{}
	if latest != "" {
		out = append(out, latest)
	}
	for _, v := range vs {
		if s := v.Original(); s != latest {
			out = append(out, s)
		}
	}
	return out
}

type pypiFile struct {
	RequiresPython *string `json:"requires_python"`
	Yanked         bool    `json:"yanked"`
}

func choosePyPI(ctx context.Context, client *http.Client, registry string, e entry, r *result) (string, error) {
	base := strings.TrimRight(registry, "/") + "/pypi/" + url.PathEscape(e.Package)
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string][]pypiFile `json:"releases"`
	}
	if err := getJSON(ctx, client, base+"/json", &doc); err != nil {
		return "", err
	}
	candidates := pypiCandidates(doc.Info.Version, doc.Releases, e.Version)
	for _, v := range candidates {
		files := doc.Releases[v]
		if len(files) == 0 || files[0].Yanked {
			continue
		}
		rp := ""
		if files[0].RequiresPython != nil {
			rp = strings.TrimSpace(*files[0].RequiresPython)
		}
		why := mcpinstall.RuntimeMismatch(mcpinstall.Resolved{Source: "pypi", DeclaredRuntime: rp}, policy.DefaultNodeVersion, policy.DefaultPythonVersion)
		if why != "" {
			r.Excluded = append(r.Excluded, v+": "+why)
			continue
		}
		var rel struct {
			Info struct {
				RequiresDist []string `json:"requires_dist"`
			} `json:"info"`
		}
		if err := getJSON(ctx, client, base+"/"+url.PathEscape(v)+"/json", &rel); err == nil {
			for _, req := range rel.Info.RequiresDist {
				if strings.Contains(req, "extra ==") {
					continue
				}
				r.DirectDependencies = append(r.DirectDependencies, strings.TrimSpace(req))
			}
		}
		return v, nil
	}
	return "", errors.New("no published release admits Python " + policy.DefaultPythonVersion)
}

// preRelease marks a PEP 440 pre-release or development release.
var preRelease = regexp.MustCompile(`(a|b|rc|dev)\d*`)

// pypiCandidates orders releases as npmCandidates does, with PEP 440's
// release numbers compared numerically.
func pypiCandidates(latest string, releases map[string][]pypiFile, pin string) []string {
	if pin != "" {
		return []string{pin}
	}
	allowPre := preRelease.MatchString(latest)
	var vs []string
	for v := range releases {
		if v == latest || (!allowPre && preRelease.MatchString(v)) {
			continue
		}
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return pep440Less(vs[j], vs[i]) })
	return append([]string{latest}, vs...)
}

// pep440Less compares the leading release numbers, enough to order the
// releases of one project newest first.
func pep440Less(a, b string) bool {
	pa, pb := releaseNums(a), releaseNums(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	return a < b
}

func releaseNums(v string) []int {
	var out []int
	for _, p := range strings.Split(v, ".") {
		n := 0
		digits := false
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n, digits = n*10+int(c-'0'), true
		}
		if !digits {
			break
		}
		out = append(out, n)
	}
	return out
}

func getJSON(ctx context.Context, client *http.Client, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(into)
}

// syncBuffer is the gateway's log as it arrives: written by the docker
// client's goroutine, read for an error's tail while the gateway runs.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// placeholder is the value a declared secret is given: enough for a server
// that checks one is set at start, never a real key.
const placeholder = "survey-placeholder-not-a-key"

// launch starts the gateway with this one server, speaks MCP to it over
// stdio, and returns the tools the server listed and the call's result.
func launch(ctx context.Context, d dockerx.Client, pol *policy.Policy, outDir, storeDir string,
	srv spec.MCPServerSpec, e entry, logw io.Writer,
) ([]string, *callResult, string, error) {
	if foreign, err := d.PSByLabel(ctx, engine.RelayLabel); err == nil && len(foreign) > 0 {
		return nil, nil, "", fmt.Errorf("another gateway's relays are on the shared relay network (%s); wait for them to go", strings.Join(foreign, ", "))
	}
	files, err := compile.DeclaredServerGatewayFiles([]spec.MCPServerSpec{srv})
	if err != nil {
		return nil, nil, "", err
	}
	cfg := filepath.Join(outDir, "gateway", srv.StoreSubdir)
	_ = os.RemoveAll(cfg)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		return nil, nil, "", err
	}
	files["catalog.yaml"] = []byte(strings.ReplaceAll(string(files["catalog.yaml"]), spec.PHServers, storeDir))
	var secrets strings.Builder
	for _, name := range srv.Secrets {
		fmt.Fprintf(&secrets, "%s=%s\n", compile.ServerSecretKey(srv.Name, name), placeholder)
	}
	files["secrets.env"] = []byte(secrets.String())
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(cfg, name), b, 0o644); err != nil { //nolint:gosec // the gateway reads them
			return nil, nil, "", err
		}
	}
	lim := pol.Docker.Limits.WithDefaults()
	name := "agentry-survey-gw-" + srv.StoreSubdir
	_ = d.RemoveForce(ctx, name)
	gw := dockerx.RunSpec{
		Name:  name,
		Image: pol.Images.Gateway,
		// A run's gateway files and guards (compile.GatewayGuardFlags)
		// with the stdio transport in place of streaming HTTP, and a
		// run's limits for the servers it launches.
		Argv: append(append([]string{
			"--transport", "stdio",
			"--servers", srv.Name,
			"--catalog", compile.GatewayConfigDir + "/catalog.yaml",
			"--registry", compile.GatewayConfigDir + "/registry.yaml",
			"--config", compile.GatewayConfigDir + "/config.yaml",
			"--tools-config", compile.GatewayConfigDir + "/tools.yaml",
			"--secrets", compile.GatewayConfigDir + "/secrets.env",
		}, compile.GatewayGuardFlags()...), "--cpus", fmt.Sprint(lim.CPUs), "--memory", lim.Memory),
		Env: map[string]string{
			"HOME":                                  "/root",
			"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS": storeDir,
		},
		Mounts: []dockerx.Mount{
			{Source: cfg, Target: compile.GatewayConfigDir, ReadOnly: true, Relabel: true},
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		},
		// Not its container name, so it attaches no network of its own
		// to the server (engine.startInfra); short, because docker caps a
		// hostname at 64 bytes.
		Hostname:            "agentry-survey-gateway",
		Network:             "none",
		Interactive:         true,
		AutoRemove:          true,
		DisableSELinuxLabel: true,
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	// A run's gateway posture (engine.startInfra): every capability
	// dropped but the one that reads the secrets file.
	gw.CapAdd = []string{"DAC_READ_SEARCH"}
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	gwLog := &syncBuffer{}
	errw := io.MultiWriter(logw, gwLog)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		_, err := d.RunForeground(ctx, gw, stdinR, stdoutW, errw)
		// A gateway that exits (or never started) ends every wait on it.
		close(exited)
		_ = stdoutW.CloseWithError(io.EOF)
		_ = stdinR.CloseWithError(io.ErrClosedPipe)
		done <- err
	}()
	defer func() {
		_ = stdinW.Close()
		// Keep reading what the gateway writes, or docker blocks on a full
		// pipe and never exits.
		go func() { _, _ = io.Copy(io.Discard, stdoutR) }()
		// Only that the gateway exited matters here, not how.
		select {
		case err := <-done:
			_ = err
		case <-time.After(30 * time.Second):
			_ = d.Stop(context.Background(), name, 10)
			select {
			case err := <-done:
				_ = err
			case <-time.After(30 * time.Second):
				_ = d.RemoveForce(context.Background(), name)
			}
		}
		for _, relay := range engine.RelaysStartedBy(gwLog.String()) {
			_ = d.RemoveForce(context.Background(), relay)
		}
	}()
	// gatewayTail is the end of the gateway's log for an error, with the
	// docker client's own error when the gateway did not run at all.
	gatewayTail := func() string {
		tail := gatewayLogTail(gwLog.String())
		select {
		case err := <-done:
			done <- err // the teardown still waits on it
			if err != nil {
				tail += " — docker: " + err.Error()
			}
		default:
		}
		return tail
	}
	c := &mcpClient{w: stdinW, r: bufio.NewReaderSize(stdoutR, 1<<20), exited: exited}
	if _, err := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentry-mcp-server-survey", "version": "1"},
	}, 120*time.Second); err != nil {
		return nil, nil, "start", fmt.Errorf("initialize: %w%s", err, gatewayTail())
	}
	if err := c.notify("notifications/initialized"); err != nil {
		return nil, nil, "start", err
	}
	var tools []string
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.request("tools/list", params, 180*time.Second)
		if err != nil {
			return tools, nil, "start", fmt.Errorf("tools/list: %w%s", err, gatewayTail())
		}
		var page struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return tools, nil, "start", err
		}
		for _, t := range page.Tools {
			tools = append(tools, t.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	sort.Strings(tools)
	if len(tools) == 0 {
		return nil, nil, "start", fmt.Errorf("the gateway served no tools for it%s", gatewayTail())
	}
	if e.Call == nil {
		return tools, nil, "list", nil
	}
	args := e.Call.Arguments
	if args == nil {
		args = map[string]any{}
	}
	res, err := c.request("tools/call", map[string]any{"name": e.Call.Tool, "arguments": args}, 120*time.Second)
	if err != nil {
		return tools, nil, "list", fmt.Errorf("tools/call %s: %w%s", e.Call.Tool, err, gatewayTail())
	}
	var out struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(res, &out)
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
			text.WriteString(" ")
		}
	}
	return tools, &callResult{call: *e.Call, IsError: out.IsError, Excerpt: tail(strings.TrimSpace(text.String()), 300)}, "call", nil
}

// gatewayLogTail is the end of the gateway's log, for an error.
func gatewayLogTail(log string) string {
	if log == "" {
		return ""
	}
	return " — gateway log ends: " + tail(strings.TrimSpace(log), 400)
}

// mcpClient speaks newline-delimited JSON-RPC 2.0, MCP's stdio framing.
// It is used for one exchange at a time: a request that times out
// leaves its reader on the stream, so the caller gives up on the gateway.
type mcpClient struct {
	w      io.Writer
	r      *bufio.Reader
	id     int
	exited <-chan struct{}
}

func (c *mcpClient) notify(method string) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := c.w.Write(append(b, '\n'))
	return err
}

func (c *mcpClient) request(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.id++
	id := c.id
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	type reply struct {
		res json.RawMessage
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		for {
			line, err := c.r.ReadBytes('\n')
			if err != nil {
				ch <- reply{err: fmt.Errorf("the gateway closed its output: %w", err)}
				return
			}
			var msg struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &msg) != nil || msg.ID == nil || *msg.ID != id {
				continue // a notification, a server request, or noise
			}
			if msg.Error != nil {
				ch <- reply{err: fmt.Errorf("error %d: %s", msg.Error.Code, msg.Error.Message)}
				return
			}
			ch <- reply{res: msg.Result}
			return
		}
	}()
	select {
	case r := <-ch:
		return r.res, r.err
	case <-c.exited:
		// The answer and the exit can arrive together; the answer wins.
		select {
		case r := <-ch:
			return r.res, r.err
		case <-time.After(time.Second):
		}
		return nil, errors.New("the gateway exited")
	case <-time.After(timeout):
		return nil, fmt.Errorf("no answer in %s", timeout)
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
