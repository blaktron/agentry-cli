// `agentry doctor` (agentry-cli#15): the "check my setup" verb the converted
// agents lost with their doctor.sh. `setup` prepares a machine; `doctor`
// reports, without changing anything, whether a scheduled `agentry run`
// would get past its preflight — policy and model, the docker engine and
// the pinned images, the registry, and for a specific ref the verified
// install, its MCP server store, its profile and every declared secret
// (the exit-11 refusal, predicted instead of met at 03:00).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessprobe"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// newDockerClient is swapped by tests for a dockerx.Fake.
var newDockerClient = func() dockerx.Client { return dockerx.New() }

// modelKeyPresent asks the OS keychain for a stored model key without
// returning it; a seam for tests, which have no keychain. Doctor reads
// the entry itself (modelkey.Confirm) rather than trusting the presence
// marker a settings page reads, so it never says ok about a key removed
// behind the marker's back (agentry-cli#156).
var modelKeyPresent = modelkey.Confirm

// doctorHTTPTimeout bounds the registry liveness probe.
const doctorHTTPTimeout = 10 * time.Second

// doctorCheck is one line of the report.
type doctorCheck struct {
	// Name is the short check id (policy, docker, image:gateway, …).
	Name string `json:"name"`
	// Status is "ok", "warn" or "fail".
	Status string `json:"status"`
	// Detail says what was found, never a secret value.
	Detail string `json:"detail,omitempty"`
}

// doctorReport is the whole run.
type doctorReport struct {
	Ref    string        `json:"ref,omitempty"`
	Checks []doctorCheck `json:"checks"`
}

func (r *doctorReport) add(name, status, format string, args ...any) {
	r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
}
func (r *doctorReport) ok(name, f string, a ...any)   { r.add(name, "ok", f, a...) }
func (r *doctorReport) warn(name, f string, a ...any) { r.add(name, "warn", f, a...) }
func (r *doctorReport) fail(name, f string, a ...any) { r.add(name, "fail", f, a...) }

// Failed counts the checks a run would refuse on.
func (r *doctorReport) Failed() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == "fail" {
			n++
		}
	}
	return n
}

// doctorOpts carries the doctor command's flags.
type doctorOpts struct {
	local      string
	policyPath string
	adapter    string
}

// newDoctorCmd builds the doctor command.
func newDoctorCmd() *cobra.Command {
	var opts doctorOpts
	cmd := &cobra.Command{
		Use:   "doctor [ns/name[@range]]",
		Short: "Check that this machine (and optionally one QuickAgent) is ready to run",
		Long: "Report, without changing anything, whether `agentry run` would get past its\n" +
			"preflight on this machine: the operator policy and model, the docker engine\n" +
			"and the pinned gateway/harness/runner images, and the registry. With a ref\n" +
			"(or --local DIR) it also checks that QuickAgent's verified install, its\n" +
			"declared MCP servers' store, its MCP profile, and that every declared secret\n" +
			"resolves — the exit-11 refusal a scheduled run would otherwise meet.\n\n" +
			"Exit status 0 when every check passes (warnings allowed), 1 otherwise.\n" +
			"--json prints the report as JSON. No secret VALUE is ever printed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runDoctor(cmd, ref, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.local, "local", "", "check a local QuickAgent directory instead of a registry ref")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.StringVar(&opts.adapter, "adapter", "", "also check this harness adapter's installation (e.g. claude)")
	return cmd
}

// runDoctor runs every check and prints the report.
func runDoctor(cmd *cobra.Command, ref string, opts doctorOpts) error {
	if ref != "" && opts.local != "" {
		return errors.New("pass either ns/name[@range] or --local DIR, not both")
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	rep := &doctorReport{Ref: ref}
	if opts.local != "" {
		rep.Ref = "local:" + opts.local
	}

	// 1. Policy + model: the file setup writes; never created here.
	policyPath := opts.policyPath
	if policyPath == "" {
		if policyPath, err = policy.DefaultPath(); err != nil {
			return err
		}
	}
	pol := doctorPolicy(rep, policyPath)

	// 2. Docker + images (only the ones this run would need).
	docker := newDockerClient()
	var m *manifest.Manifest
	var meta *installdir.Meta
	var installDir string
	if pol != nil && (ref != "" || opts.local != "") {
		m, meta, installDir = doctorAgent(cmd, rep, home, ref, opts.local)
	}
	doctorDocker(cmd.Context(), rep, docker, pol, m)
	doctorHarness(cmd.Context(), rep, pol, opts.adapter)

	// 3. Registry reachability (always: a scheduled run resolves its ref
	// there unless the install is already cached).
	doctorRegistry(cmd.Context(), rep)

	// 4. The agent's declared commands against the catalog (a located
	// manifest is enough), then its store, profile and secrets.
	if pol != nil && m != nil {
		doctorExecCatalog(rep, pol, m)
	}
	if pol != nil && m != nil && meta != nil {
		doctorAgentState(rep, home, pol, m, meta, installDir)
	}

	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printDoctorReport(cmd.OutOrStdout(), rep)
	}
	if n := rep.Failed(); n > 0 {
		return engine.Classify(engine.ExitCLI, fmt.Errorf("doctor: %d check(s) failed", n))
	}
	return nil
}

// doctorHarness reports the installation behind a host-run adapter (plan
// runner-m-r4-adapters §5): whether the executable resolves, which release
// it is, and whether the endpoint the policy points it at can be reached
// through the run's proxy at all. A scheduled `agentry run --adapter
// claude` fails on exactly these three things. The facts come from
// harnessprobe, which the desktop's wizard reads too (agentry-cli#147);
// this renders them as checks.
func doctorHarness(ctx context.Context, rep *doctorReport, pol *policy.Policy, name string) {
	if name == "" {
		return
	}
	r := harnessprobe.Probe(ctx, pol, name)
	if !r.Known {
		rep.fail("adapter", "%s", r.Reason)
		return
	}
	if !r.Host {
		rep.ok("adapter:"+name, "containerised harness; its image is checked above")
		return
	}
	check := "harness:" + name
	if !r.Found {
		rep.fail(check, "%s", r.Reason)
		return
	}
	if len(r.Warnings) > 0 {
		for _, w := range r.Warnings {
			rep.warn(check, "%s", w)
		}
	} else if r.Source == harnessprobe.SourceKnown {
		rep.ok(check, "%s (%s) — a usual install location, not on this process's PATH", r.Path, r.Version)
	} else {
		rep.ok(check, "%s (%s)", r.Path, r.Version)
	}
	if r.Endpoint == nil {
		return
	}
	switch {
	case !r.Endpoint.Valid:
		rep.fail(check+":endpoint", "harness.%s.baseUrl %q is not a URL", name, r.Endpoint.BaseURL)
	case r.Endpoint.Allowed:
		rep.ok(check+":endpoint", "%s is in model.proxyAllow", r.Endpoint.HostPort)
	default:
		rep.fail(check+":endpoint", "harness.%s.baseUrl points at %s, which model.proxyAllow does not list — the run's proxy would deny it",
			name, r.Endpoint.HostPort)
	}
	if r.Endpoint.Valid {
		if w := setup.PlainHTTPWarning(r.Endpoint.BaseURL); w != "" {
			rep.warn(check+":endpoint", "%s", w)
		}
	}
}

// doctorPolicy loads the operator policy and checks the model block.
func doctorPolicy(rep *doctorReport, path string) *policy.Policy {
	pol, _, err := policy.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rep.fail("policy", "%s does not exist — run `agentry setup`", path)
		} else {
			rep.fail("policy", "%s: %v", path, err)
		}
		return nil
	}
	rep.ok("policy", "%s", path)
	switch {
	case pol.Model.Provider == "" || pol.Model.Name == "":
		rep.fail("model", "no provider/model configured — run `agentry setup`")
	case policy.IsModelSignIn(pol.Model.APIKeyKind):
		// A model sign-in (agentry-cli#738): the person's grant, kept in
		// the model sign-in store; doctor asks only whether one is kept.
		vendor := modelsignin.VendorOf(pol.Model.APIKeyKind)
		var r *modelsignin.Record
		h, err := runnerHome()
		if err == nil {
			r, err = modelsignin.Open(h, nil).Get(vendor)
		}
		switch {
		case err != nil && !errors.Is(err, modelsignin.ErrNone):
			rep.fail("model", "%s/%s: %v", pol.Model.Provider, pol.Model.Name, err)
		case err != nil:
			rep.fail("model", "%s/%s spends %s (apiKeyKind %s) and none is kept on this computer — run `agentry setup` to sign in", pol.Model.Provider, pol.Model.Name, policy.SignInSentence(pol.Model.APIKeyKind), pol.Model.APIKeyKind)
		case r.Broken != "":
			rep.fail("model", "%s/%s: the %s sign-in no longer works (%s) — run `agentry setup` to sign in again", pol.Model.Provider, pol.Model.Name, vendor, r.Broken)
		case vendor == modelsignin.Claude && r.ConfigDir == "":
			// Each Claude Code signs in where it runs (agentry-cli#740):
			// the image, the default, is not signed in.
			rep.ok("model", "%s/%s (spends %s, signed in for the Claude Code installed on this computer only: runs need --adapter claude, or `agentry model-signins add claude` for the image, the default)", pol.Model.Provider, pol.Model.Name, policy.SignInSentence(pol.Model.APIKeyKind))
		default:
			rep.ok("model", "%s/%s (spends %s, kept as the model sign-in %s; runs report tokens, not dollars)", pol.Model.Provider, pol.Model.Name, policy.SignInSentence(pol.Model.APIKeyKind), vendor)
		}
	case pol.Model.APIKeyEnv == "":
		rep.fail("model", "%s/%s: no apiKeyEnv configured — run `agentry setup`", pol.Model.Provider, pol.Model.Name)
	case os.Getenv(pol.Model.APIKeyEnv) == "" && policy.KeySource(policy.HarnessPolicy{}, pol.Model) == modelkey.SourceKeyring:
		// The variable is unset and the policy names the keychain as the
		// fallback (agentry-cli#148): a run would read it from there.
		present, err := modelKeyPresent(pol.Model.Provider)
		switch {
		case err != nil:
			rep.fail("model", "%s/%s: $%s is not set and the OS keychain could not be asked — %v", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv, err)
		case !present:
			rep.fail("model", "%s/%s: $%s is not set and the OS keychain has no entry %s (apiKeySource keyring) — run `agentry setup` to store the key, or set the variable", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv, modelkey.Account(pol.Model.Provider))
		default:
			rep.ok("model", "%s/%s (key from the OS keychain entry %s; $%s unset)", pol.Model.Provider, pol.Model.Name, modelkey.Account(pol.Model.Provider), pol.Model.APIKeyEnv)
		}
	case os.Getenv(pol.Model.APIKeyEnv) == "":
		rep.fail("model", "%s/%s: $%s is not set in this environment", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
	default:
		rep.ok("model", "%s/%s (key from $%s)", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
	}
	// A custom endpoint over plain http off this machine is allowed
	// (agentry-cli#377) and is worth a warning on every check.
	if w := setup.PlainHTTPWarning(pol.Model.BaseURL); w != "" {
		rep.warn("model:endpoint", "%s", w)
	}
	// A subscription token (agentry-cli#171) drives Claude Code alone; a
	// machine whose default adapter is the container harness cannot run
	// with it, and doctor says so before a run finds out.
	if policy.KeyKind(policy.HarnessPolicy{}, pol.Model) == policy.KeyKindSubscription {
		rep.warn("model:kind", "the key is an Anthropic subscription token (apiKeyKind subscription): runs through Claude Code only (`agentry run --adapter claude`); the container harness refuses it — store a Console API key for those")
	}
	if pol.SecretSource.Mode == "envFile" {
		if _, err := os.Stat(pol.SecretSource.EnvFile); err != nil {
			rep.fail("secretSource", "envFile %s: %v", pol.SecretSource.EnvFile, err)
		} else {
			rep.ok("secretSource", "envFile %s", pol.SecretSource.EnvFile)
		}
	} else {
		rep.ok("secretSource", "env")
	}
	// The exec catalog (agentry-cli#100): what a declared exec command
	// resolves to on this machine beyond the commands every machine
	// provides (agentry-cli#595).
	if len(pol.Exec.Packages) == 0 {
		rep.ok("exec", "the commands every machine provides (%s); no others catalogued, so a QuickAgent declaring another is refused at plan time until exec.packages names its apk package", strings.Join(policy.BaseExecCommands(), ", "))
	} else {
		var entries []string
		for _, c := range slices.Sorted(maps.Keys(pol.Exec.Packages)) {
			entries = append(entries, c+" → "+pol.Exec.Packages[c])
		}
		line := strings.Join(entries, ", ")
		if pol.Exec.APKMirror != "" {
			line += "; mirror " + pol.Exec.APKMirror
		}
		rep.ok("exec", "%s", line)
	}
	return pol
}

// doctorDocker checks the engine and the images a run would need.
func doctorDocker(ctx context.Context, rep *doctorReport, d dockerx.Client, pol *policy.Policy, m *manifest.Manifest) {
	if err := d.Available(ctx); err != nil {
		rep.fail("docker", "%v", err)
		return
	}
	rep.ok("docker", "engine available")
	if pol == nil {
		return
	}
	if want := pol.Docker.Runtime; want != "" {
		// The policy's runtime is enforced, never preferred (DR-23): a
		// run on a daemon without it stops at ensure, so doctor says so
		// here, with the fix.
		table, err := d.Runtimes(ctx)
		have := dockerx.SortedRuntimeNames(table)
		switch {
		case err != nil:
			rep.fail("docker:runtime", "%v", err)
		case slices.Contains(have, want):
			rep.ok("docker:runtime", "%s registered on the daemon", want)
		default:
			rep.fail("docker:runtime", "policy docker.runtime is %q but the daemon has only: %s — install it and register it in the daemon's runtimes, or unset docker.runtime", want, strings.Join(have, ", "))
		}
		// The Drafter's web tools under a runtime resolve names only
		// through the host's own name servers (agentry-cli#386): without
		// them a draft is refused at compile, so doctor says so first.
		// The llm-proxy, the install proxy and the relays take the runtime
		// only with them (agentry-cli#337).
		if len(pol.Docker.Resolvers) == 0 {
			rep.warn("docker:resolvers", "policy docker.runtime is %q and docker.resolvers is empty: `agentry draft` will be refused (its web tools cannot reach docker's DNS under %s), and the llm-proxy, the install proxy and the relays stay on the daemon's default — name the host's upstream name servers in docker.resolvers", want, want)
		} else {
			rep.ok("docker:resolvers", "under %s, the Drafter's web tools, the llm-proxy, the install proxy and, where the per-run docker filter runs, the relays resolve through %s (a host-run harness's proxy stays on the daemon's default)", want, strings.Join(pol.Docker.Resolvers, ", "))
		}
		// The gateway takes a runtime of its own, registered to open a
		// host Unix socket (agentry-cli#337).
		switch gw := pol.Docker.GatewayRuntime; {
		case gw == "":
			rep.warn("docker:gatewayRuntime", "the gateway stays on the daemon's default: it reaches the run's docker filter through a host Unix socket, which %s opens only when registered with %s — register it again under a name of its own and set docker.gatewayRuntime", want, dockerx.HostUDSOpen)
		case err != nil:
			// docker:runtime has already failed for the unreadable table.
		case !slices.Contains(have, want):
			rep.warn("docker:gatewayRuntime", "%s not checked: docker.runtime %q is not registered (docker:runtime)", gw, want)
		case !slices.Contains(have, gw):
			rep.fail("docker:gatewayRuntime", "policy docker.gatewayRuntime is %q but the daemon has only: %s — register %s again under that name with runtimeArgs [%q], or unset docker.gatewayRuntime", gw, strings.Join(have, ", "), want, dockerx.HostUDSOpen)
		default:
			if rerr := dockerx.CheckHostUDSRegistration(table, want, gw); rerr != nil {
				rep.fail("docker:gatewayRuntime", "policy docker.gatewayRuntime: %v — fix the registration in the daemon's runtimes, or unset docker.gatewayRuntime", rerr)
			} else {
				rep.ok("docker:gatewayRuntime", "%s registered on the daemon as %s with %s, for the gateway", gw, want, dockerx.HostUDSOpen)
			}
		}
	}
	images := []struct{ name, ref, fix string }{
		{"image:gateway", pol.Images.Gateway, "`agentry setup` pulls it"},
		{"image:harness", pol.Images.HarnessDockerAgent, "`agentry setup` pulls it"},
		{"image:runner", pol.RunnerImageTag(runnerimage.ContentVersion()), "`agentry setup` builds it"},
	}
	if m != nil {
		needNode, needPython := false, false
		for _, srv := range m.Capabilities.MCPServers {
			switch {
			case srv.NPM != "":
				needNode = true
			case srv.PyPI != "":
				needPython = true
			}
		}
		if needNode {
			images = append(images, struct{ name, ref, fix string }{"image:nodeRuntime", pol.Images.NodeRuntime, "pulled at first MCP server install"})
		}
		if needPython {
			images = append(images, struct{ name, ref, fix string }{"image:pythonRuntime", pol.Images.PythonRuntime, "pulled at first MCP server install"})
		}
	}
	for _, im := range images {
		if im.ref == "" {
			rep.fail(im.name, "no image pinned in the policy")
			continue
		}
		ok, err := d.ImageExists(ctx, im.ref)
		switch {
		case err != nil:
			rep.fail(im.name, "%s: %v", im.ref, err)
		case !ok:
			rep.fail(im.name, "%s is not present locally — %s", im.ref, im.fix)
		default:
			rep.ok(im.name, "%s", im.ref)
		}
	}
}

// doctorRegistry probes the configured registry's liveness endpoint.
func doctorRegistry(ctx context.Context, rep *doctorReport) {
	reg := registryURL()
	ctx, cancel := context.WithTimeout(ctx, doctorHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg+"/healthz", nil)
	if err != nil {
		rep.fail("registry", "%s: %v", reg, err)
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		rep.fail("registry", "%s unreachable: %v", reg, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		rep.fail("registry", "%s/healthz answered %d", reg, res.StatusCode)
		return
	}
	if credentialFor(reg) == "" {
		rep.ok("registry", "%s reachable (no credential stored; public agents only)", reg)
		return
	}
	rep.ok("registry", "%s reachable (credential stored)", reg)
}

// doctorAgent locates the QuickAgent: a cached verified install for a
// registry ref (resolved without downloading), or a local directory.
func doctorAgent(cmd *cobra.Command, rep *doctorReport, home, ref, local string) (*manifest.Manifest, *installdir.Meta, string) {
	if local != "" {
		m, err := localrun.ManifestFrom(local)
		if err != nil {
			rep.fail("manifest", "%s: %v", local, err)
			return nil, nil, ""
		}
		digest, err := localrun.TreeDigest(local)
		if err != nil {
			rep.fail("manifest", "%s: %v", local, err)
			return nil, nil, ""
		}
		rep.ok("manifest", "%s/%s@%s (%s)", m.Namespace, m.Name, m.Version, local)
		meta := &installdir.Meta{Ref: fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
			Namespace: m.Namespace, Name: m.Name, Version: m.Version, Digest: digest, Source: "local"}
		// A local tree installs under the same digest-keyed layout as a
		// registry ref (cmd_run.go), so its cached plans are checkable too
		// (agentry-cli#60).
		installDir := installdir.ReleaseDir(home, m.Namespace, m.Name, m.Version, digest)
		if _, ierr := installdir.Inspect(installDir, digest); ierr != nil {
			if errors.Is(ierr, installdir.ErrNotInstalled) {
				rep.warn("install", "not installed yet — `agentry run --local %s --prepare` verifies and installs it", local)
			} else {
				// A local directory is its own source: a cached copy that
				// differs is replaced from it by the next run, never refused,
				// so the repair a registry install names does not apply.
				rep.warn("install", "the cached copy at %s is not what was laid down from %s — the next run lays it down again from the directory", installDir, local)
			}
			return m, meta, ""
		}
		rep.ok("install", "verified install %s", installDir)
		return m, meta, installDir
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		rep.fail("resolve", "%v", err)
		return nil, nil, ""
	}
	// A local install (agentry-cli#491) is checked where a run reads it,
	// against its integrity record, with no registry to resolve.
	if meta, installDir, ok, lerr := localinstall.Materialize(home, parsed.Namespace, parsed.Name, parsed.Range); ok {
		if lerr != nil {
			rep.fail("install", "%v", lerr)
			return nil, nil, ""
		}
		rep.ok("resolve", "%s: installed from a local folder, unsigned (%s) — a run needs --allow-unsigned", meta.Ref, installdir.DigestSuffix(meta.Digest))
		m, merr := localrun.ManifestFrom(filepath.Join(installDir, "workspace"))
		if merr != nil {
			rep.fail("install", "%v", merr)
			return nil, nil, ""
		}
		rep.ok("install", "verified install %s", installDir)
		return m, meta, installDir
	}
	reg := registryURL()
	client, err := newAPIClientChecked(cmd, reg, credentialFor(reg))
	if err != nil {
		rep.fail("resolve", "%v", err)
		return nil, nil, ""
	}
	res, err := resolveRef(cmd.Context(), client, ref, false)
	if err != nil {
		rep.fail("resolve", "%s: %v", ref, err)
		return nil, nil, ""
	}
	if res.Yanked {
		rep.warn("resolve", "%s/%s@%s is YANKED (%s)", res.Namespace, res.Name, res.Version, res.Digest)
	} else {
		rep.ok("resolve", "%s/%s@%s (%s)", res.Namespace, res.Name, res.Version, res.Digest)
	}
	installDir := installdir.ReleaseDir(home, res.Namespace, res.Name, res.Version, res.Digest)
	meta, ierr := installdir.Inspect(installDir, res.Digest)
	var changed *installdir.ChangedError
	switch {
	case errors.As(ierr, &changed):
		// What a run refuses (agentry-cli#326), said here first.
		changed.Ref = fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version)
		rep.fail("install", "%v", changed)
		return nil, nil, ""
	case errors.Is(ierr, installdir.ErrNotInstalled):
		rep.warn("install", "not installed yet — `agentry run %s --prepare` downloads, verifies and installs it", ref)
		return nil, nil, ""
	case errors.Is(ierr, installdir.ErrNoRecord):
		rep.warn("install", "%s has no integrity record from this runner home (laid down by an older CLI) — the next run fetches it again and checks it as a first fetch is", installDir)
		return nil, nil, ""
	case ierr != nil:
		rep.fail("install", "%s: %v", installDir, ierr)
		return nil, nil, ""
	}
	m, err := localrun.ManifestFrom(filepath.Join(installDir, "workspace"))
	if err != nil {
		rep.fail("install", "%s: %v", installDir, err)
		return nil, nil, ""
	}
	rep.ok("install", "verified install %s", installDir)
	return m, meta, installDir
}

// doctorAgentState checks the server store, the profile and the secrets
// for one located QuickAgent.
// doctorExecCatalog checks a QuickAgent's declared exec commands against
// the machine's catalog: what the compiler will refuse, said here first
// (agentry-cli#100).
func doctorExecCatalog(rep *doctorReport, pol *policy.Policy, m *manifest.Manifest) {
	cmds := m.Capabilities.Exec.Commands
	if len(cmds) == 0 {
		return
	}
	var missing, mapped []string
	for _, c := range cmds {
		if pkg := pol.Exec.Package(c); pkg == "" {
			missing = append(missing, c)
		} else {
			mapped = append(mapped, c+" → "+pkg)
		}
	}
	if len(missing) > 0 {
		rep.fail("exec", "declared command(s) not in this machine's exec.packages catalog: %s — a run is refused at plan time until the policy names each one's apk package", strings.Join(missing, ", "))
		return
	}
	rep.ok("exec", "declared commands catalogued: %s", strings.Join(mapped, ", "))
}

func doctorAgentState(rep *doctorReport, home string, pol *policy.Policy, m *manifest.Manifest, meta *installdir.Meta, installDir string) {
	profDir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, installdir.DigestSuffix(meta.Digest))
	if n := len(mcpinstall.Packages(m.Capabilities.MCPServers)); n > 0 {
		serversDir := mcpinstall.StoreDir(profDir)
		missing := []string{}
		for _, srv := range mcpinstall.Packages(m.Capabilities.MCPServers) {
			sm, err := mcpinstall.LoadMeta(serversDir, mcpinstall.IDFor(srv))
			if err != nil || sm == nil || sm.Package != srvPackage(srv) || sm.Version != srv.Version {
				missing = append(missing, srv.SourceID()+"@"+srv.Version)
			}
		}
		if len(missing) > 0 {
			rep.warn("mcpServers", "%d of %d declared server(s) not in the signature store (%s) — `agentry run --prepare` installs them", len(missing), n, strings.Join(missing, ", "))
		} else {
			rep.ok("mcpServers", "%d declared server(s) installed in the signature store", n)
		}
		// The registry's vulnerability verdict (agentry#177): what a run
		// would refuse on, warn about, or could not check.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		statuses := mcpinstall.CheckVulnerabilities(ctx, nil, registryURL(), m.Capabilities.MCPServers)
		cancel()
		for _, st := range statuses {
			switch {
			case st.Refuses() && !pol.MCPServers.AllowVulnerable:
				rep.fail("mcpServers.vulnerabilities", "%s: %s — a run refuses it; pin a fixed version%s", st.Ref, st.Describe(st.Refusing()), fixedNote(st))
			case st.Refuses():
				rep.warn("mcpServers.vulnerabilities", "%s: %s, allowed by policy mcpServers.allowVulnerable%s", st.Ref, st.Describe(st.Refusing()), fixedNote(st))
			case st.Status == "vulnerable":
				rep.warn("mcpServers.vulnerabilities", "%s: %s, below the refusal bar — a run proceeds", st.Ref, st.Describe(st.Advisories))
			case st.Status == "error" && len(st.Advisories) > 0:
				rep.warn("mcpServers.vulnerabilities", "%s: the registry's last lookup failed; it last knew %s, below the refusal bar — a run proceeds", st.Ref, st.Describe(st.Advisories))
			case st.Status == "error":
				rep.warn("mcpServers.vulnerabilities", "%s: the registry's last lookup failed and it knows no advisories for this version — a run proceeds", st.Ref)
			case st.Status == "unknown":
				rep.warn("mcpServers.vulnerabilities", "%s: the registry has not scanned this version (no manifest declared it yet)", st.Ref)
			case st.Status == "unreachable":
				rep.warn("mcpServers.vulnerabilities", "%s: could not ask the registry (%s) — a run proceeds unchecked", st.Ref, st.Err)
			case st.Status == mcpinstall.StatusRemote:
				continue // a remote server is no package version; nothing to look up
			default:
				rep.ok("mcpServers.vulnerabilities", "%s: no known advisories as of %s", st.Ref, st.CheckedAt.UTC().Format("2006-01-02"))
			}
		}
	}
	if installDir != "" {
		// One plan per run mode (plan/<mode>/); a plan from an older CLI
		// is stale and `agentry run --prepare` recompiles it in place
		// (agentry-cli#60).
		for _, mode := range m.SupportedModes() {
			b, err := os.ReadFile(filepath.Join(installDir, "plan", string(mode), "plan.json")) //nolint:gosec // our install layout
			switch {
			case err != nil:
				rep.warn("plan."+string(mode), "no compiled plan yet (compiled at first run, or by `agentry run --prepare`)")
			case localrun.PlanVersionOf(b) != spec.PlanVersion:
				rep.warn("plan."+string(mode), "compiled by an older CLI (plan version %s, want %s) — `agentry run --prepare` recompiles it", localrun.PlanVersionOf(b), spec.PlanVersion)
			default:
				rep.ok("plan."+string(mode), "compiled (plan version %s)", spec.PlanVersion)
			}
		}
	}

	secrets := allSecretNames(m)
	prof, err := profile.Load(profDir)
	switch {
	case err != nil:
		rep.fail("profile", "%s: %v", profDir, err)
	case prof == nil && len(secrets) > 0:
		rep.warn("profile", "no MCP profile — secrets resolve from the %s source (an interactive run offers guided setup)", pol.SecretSource.Mode)
	case prof == nil:
		rep.ok("profile", "none needed (no declared secrets)")
	case prof.Digest != meta.Digest:
		rep.warn("profile", "profile is for digest %s, install is %s", prof.Digest, meta.Digest)
	default:
		rep.ok("profile", "%s (%d secret(s) held)", profDir, len(prof.Secrets))
	}
	if len(secrets) == 0 {
		return
	}
	var overrides map[string]string
	if prof != nil {
		overrides = prof.Secrets
	}
	// An optional secret that would not resolve is a note, not a failure:
	// the run starts without it (DR-70). Only a required one refuses.
	missing, absent := []string{}, []string{}
	for _, name := range secrets {
		switch {
		case engine.SecretResolvable(name, overrides, pol.SecretSource):
		case m.Capabilities.IsOptionalSecret(name):
			absent = append(absent, name)
		default:
			missing = append(missing, name)
		}
	}
	note := ""
	if len(absent) > 0 {
		note = fmt.Sprintf("; optional and not set, so a run goes without them: %s", strings.Join(absent, ", "))
	}
	if len(missing) > 0 {
		rep.fail("secrets", "%d of %d declared secret(s) would not resolve (run refuses with exit %d): %s — set them in the %s source or `agentry run --reconfigure-profile`%s",
			len(missing), len(secrets), engine.ExitPolicy, strings.Join(missing, ", "), pol.SecretSource.Mode, note)
		return
	}
	if len(absent) > 0 {
		required := 0
		for _, name := range secrets {
			if !m.Capabilities.IsOptionalSecret(name) {
				required++
			}
		}
		rep.ok("secrets", "all %d required secret(s) resolve%s", required, note)
		return
	}
	rep.ok("secrets", "all %d declared secret(s) resolve", len(secrets))
}

// printDoctorReport renders the text form.
func printDoctorReport(w io.Writer, rep *doctorReport) {
	if rep.Ref != "" {
		_, _ = fmt.Fprintf(w, "agentry doctor — %s\n", rep.Ref)
	} else {
		_, _ = fmt.Fprintln(w, "agentry doctor — machine checks (pass a ref to check one QuickAgent)")
	}
	for _, c := range rep.Checks {
		mark := map[string]string{"ok": "ok  ", "warn": "warn", "fail": "FAIL"}[c.Status]
		_, _ = fmt.Fprintf(w, "  %s  %-18s %s\n", mark, c.Name, c.Detail)
	}
	if n := rep.Failed(); n > 0 {
		_, _ = fmt.Fprintf(w, "%d check(s) failed\n", n)
	} else {
		_, _ = fmt.Fprintln(w, "ready")
	}
}

func fixedNote(st mcpinstall.VulnStatus) string {
	if f := st.FixedIn(); len(f) > 0 {
		return " (fixed in " + strings.Join(f, ", ") + ")"
	}
	return ""
}
