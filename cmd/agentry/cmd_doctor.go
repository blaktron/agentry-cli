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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// newDockerClient is swapped by tests for a dockerx.Fake.
var newDockerClient = func() dockerx.Client { return dockerx.New() }

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
}

// newDoctorCmd builds the doctor command.
func newDoctorCmd() *cobra.Command {
	var opts doctorOpts
	cmd := &cobra.Command{
		Use:   "doctor [ns/name[@range]]",
		Short: "Check that this machine (and optionally one BriefAgent) is ready to run",
		Long: "Report, without changing anything, whether `agentry run` would get past its\n" +
			"preflight on this machine: the operator policy and model, the docker engine\n" +
			"and the pinned gateway/harness/runner images, and the registry. With a ref\n" +
			"(or --local DIR) it also checks that BriefAgent's verified install, its\n" +
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
	f.StringVar(&opts.local, "local", "", "check a local BriefAgent directory instead of a registry ref")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
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
	var meta *installMeta
	var installDir string
	if pol != nil && (ref != "" || opts.local != "") {
		m, meta, installDir = doctorAgent(cmd, rep, home, ref, opts.local)
	}
	doctorDocker(cmd.Context(), rep, docker, pol, m)

	// 3. Registry reachability (always: a scheduled run resolves its ref
	// there unless the install is already cached).
	doctorRegistry(cmd.Context(), rep)

	// 4. The agent's store, profile and secrets.
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
	case pol.Model.APIKeyEnv == "":
		rep.fail("model", "%s/%s: no apiKeyEnv configured — run `agentry setup`", pol.Model.Provider, pol.Model.Name)
	case os.Getenv(pol.Model.APIKeyEnv) == "":
		rep.fail("model", "%s/%s: $%s is not set in this environment", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
	default:
		rep.ok("model", "%s/%s (key from $%s)", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
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
	images := []struct{ name, ref, fix string }{
		{"image:gateway", pol.Images.Gateway, "`agentry setup` pulls it"},
		{"image:harness", pol.Images.HarnessDockerAgent, "`agentry setup` pulls it"},
		{"image:runner", pol.RunnerImageTag(runnerimage.ContentVersion()), "`agentry setup` builds it"},
	}
	if m != nil {
		needNode, needPython := false, false
		for _, srv := range m.Capabilities.MCPServers {
			if srv.NPM != "" {
				needNode = true
			} else {
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

// doctorAgent locates the BriefAgent: a cached verified install for a
// registry ref (resolved without downloading), or a local directory.
func doctorAgent(cmd *cobra.Command, rep *doctorReport, home, ref, local string) (*manifest.Manifest, *installMeta, string) {
	if local != "" {
		m, err := manifestFromWorkspace(local)
		if err != nil {
			rep.fail("manifest", "%s: %v", local, err)
			return nil, nil, ""
		}
		digest, err := localTreeDigest(local)
		if err != nil {
			rep.fail("manifest", "%s: %v", local, err)
			return nil, nil, ""
		}
		rep.ok("manifest", "%s/%s@%s (%s)", m.Namespace, m.Name, m.Version, local)
		meta := &installMeta{Ref: fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
			Namespace: m.Namespace, Name: m.Name, Version: m.Version, Digest: digest, Source: "local"}
		return m, meta, ""
	}
	if _, err := manifest.ParseRef(ref); err != nil {
		rep.fail("resolve", "%v", err)
		return nil, nil, ""
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
	installDir := filepath.Join(home, "agents", res.Namespace, res.Name,
		fmt.Sprintf("%s-%s", res.Version, digestSuffix(res.Digest)))
	meta, ok := loadInstallMeta(installDir, res.Digest)
	if !ok {
		rep.warn("install", "not installed yet — `agentry run %s --prepare` downloads, verifies and installs it", ref)
		return nil, nil, ""
	}
	m, err := manifestFromWorkspace(filepath.Join(installDir, "workspace"))
	if err != nil {
		rep.fail("install", "%s: %v", installDir, err)
		return nil, nil, ""
	}
	rep.ok("install", "verified install %s", installDir)
	return m, meta, installDir
}

// doctorAgentState checks the server store, the profile and the secrets
// for one located BriefAgent.
func doctorAgentState(rep *doctorReport, home string, pol *policy.Policy, m *manifest.Manifest, meta *installMeta, installDir string) {
	profDir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, digestSuffix(meta.Digest))
	if n := len(m.Capabilities.MCPServers); n > 0 {
		serversDir := mcpinstall.StoreDir(profDir)
		missing := []string{}
		for _, srv := range m.Capabilities.MCPServers {
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
	}
	if installDir != "" {
		if _, err := os.Stat(filepath.Join(installDir, "plan", "plan.json")); err != nil {
			rep.warn("plan", "no compiled plan yet (compiled at first run)")
		} else {
			rep.ok("plan", "compiled")
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
	missing := []string{}
	for _, name := range secrets {
		if !engine.SecretResolvable(name, overrides, pol.SecretSource) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		rep.fail("secrets", "%d of %d declared secret(s) would not resolve (run refuses with exit %d): %s — set them in the %s source or `agentry run --reconfigure-profile`",
			len(missing), len(secrets), engine.ExitPolicy, strings.Join(missing, ", "), pol.SecretSource.Mode)
		return
	}
	rep.ok("secrets", "all %d declared secret(s) resolve", len(secrets))
}

// printDoctorReport renders the text form.
func printDoctorReport(w io.Writer, rep *doctorReport) {
	if rep.Ref != "" {
		_, _ = fmt.Fprintf(w, "agentry doctor — %s\n", rep.Ref)
	} else {
		_, _ = fmt.Fprintln(w, "agentry doctor — machine checks (pass a ref to check one BriefAgent)")
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
