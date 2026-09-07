// Engine support: run-dir preparation, placeholder rendering, secret and
// env resolution, teardown. This file owns exactly the run-scoped values
// the plan must not contain (§R4/§R6): host paths, the gateway token, and
// secret values — and the guarantee that the last two die with the run.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// runDirs are the per-run host locations.
type runDirs struct {
	root        string // <RunRoot>/runs/<id>
	workspace   string // root/workspace — the rw copy tools may mount
	inputs      string // root/inputs — the :ro operator-inputs channel (P2 §P4)
	session     string // root/session — an interactive session's harness store (DR-15)
	cfg         string // root/cfg — rendered gateway+harness files
	secretsPath string // root/secrets.env (0600, deleted on teardown)
	serversDir  string // the signature's server store (RUNNER-PLAN-3, :ro)
	outDir      string
	receiptsDir string
	// hostGatewayURL / hostProxyURL are the published 127.0.0.1 addresses
	// a host-run harness uses; empty for a container harness.
	hostGatewayURL string
	hostProxyURL   string
}

// replacer substitutes the §R4 placeholders with this run's real values.
func (rd *runDirs) replacer() *strings.Replacer {
	return strings.NewReplacer(
		spec.PHWorkspace, rd.workspace,
		spec.PHOut, rd.outDir,
		spec.PHCfg, rd.cfg,
		spec.PHSecrets, rd.secretsPath,
		spec.PHInputs, rd.inputs,
		spec.PHSession, rd.session,
		spec.PHServers, rd.serversDir,
		spec.PHHostGatewayURL, rd.hostGatewayURL,
		spec.PHHostProxyURL, rd.hostProxyURL,
		spec.PHRunID, filepath.Base(rd.root),
	)
}

// prepareRunDirs materializes the per-run tree: a fresh workspace copy
// (agents must not mutate the pristine install, §R1 idempotent re-runs),
// rendered config, and the output folder with reports/ and receipts/.
func prepareRunDirs(p Params, runID string, td *teardown) (*runDirs, error) {
	root := filepath.Join(p.RunRoot, "runs", runID)
	rd := &runDirs{
		root:      root,
		workspace: filepath.Join(root, "workspace"),
		inputs:    filepath.Join(root, "inputs"),
		session:   filepath.Join(root, "session"),
		cfg:       filepath.Join(root, "cfg"),
		// secrets.env lives INSIDE cfg/gateway because that directory is
		// the gateway's /mcp mount — the compiled `--secrets
		// /mcp/secrets.env` argv resolves to this file and nothing else.
		secretsPath: filepath.Join(root, "cfg", "gateway", "secrets.env"),
		serversDir:  p.ServersDir,
		outDir:      p.OutDir,
		receiptsDir: filepath.Join(p.OutDir, "receipts"),
	}
	if len(p.Plan.MCPServers) > 0 && rd.serversDir == "" {
		return nil, Classify(ExitCLI, fmt.Errorf("plan declares MCP servers but no server store was provided"))
	}
	// inputs/ exists even when empty: the compiled catalog always mounts
	// it :ro, and an absent bind source would fail the tools container.
	for _, dir := range []string{rd.workspace, rd.inputs, rd.session, filepath.Join(rd.cfg, "gateway"), filepath.Join(p.OutDir, "reports"), rd.receiptsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("create run dirs: %w", err))
		}
	}
	if err := stageInputs(p.InputPaths, rd.inputs); err != nil {
		return nil, err
	}
	if err := stageIntake(p, rd); err != nil {
		return nil, err
	}
	// secrets.env dies unconditionally; the rest of the run dir only
	// when not kept for debugging (§R11 invariant 6; KeepRunDir never
	// keeps secrets).
	td.add(func(context.Context) {
		_ = os.Remove(rd.secretsPath)
		if !p.KeepRunDir {
			_ = os.RemoveAll(root)
		}
	})

	if err := copyTree(p.WorkspaceSrc, rd.workspace); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("copy workspace: %w", err))
	}
	// Gateway files now; harness files once the infrastructure is up, so a
	// host-run harness sees the published addresses (Run / StartSession).
	if err := renderPlanFiles(p.PlanDir, rd, "gateway"); err != nil {
		return nil, err
	}
	if err := stampOwner(filepath.Join(rd.cfg, "gateway", "toolspec.json")); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("stamp owner into toolspec: %w", err))
	}
	// The plan the containers actually consumed, verbatim, for audit.
	if b, err := os.ReadFile(filepath.Join(p.PlanDir, "plan.json")); err == nil { //nolint:gosec // our plan dir
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "run-plan.json"), b, 0o644)
	}
	// Everything the gateway will hand to a tool container, labelled once
	// the directories exist and hold their content. Harness files are
	// rendered later into cfg/, and a new file takes its directory's type,
	// so they are covered by this too.
	relabelForToolContainers(context.Background(), p.Stderr,
		rd.workspace, rd.inputs, rd.cfg, rd.outDir, rd.serversDir)
	return rd, nil
}

// stageIntake writes the agent-side intake document and stages file
// answers next to the other inputs; the non-secret document is also a
// receipt (plan §3.5).
func stageIntake(p Params, rd *runDirs) error {
	if p.Intake.Empty() {
		return nil
	}
	for _, path := range p.Intake.FilePaths() {
		if _, err := os.Lstat(filepath.Join(rd.inputs, filepath.Base(path))); err == nil {
			return Classify(ExitCLI, fmt.Errorf("intake file %s collides with an input of the same basename under %s", path, spec.InputsMount))
		}
		if err := stageInputs([]string{path}, rd.inputs); err != nil {
			return err
		}
	}
	const name = "intake.json"
	if _, err := os.Lstat(filepath.Join(rd.inputs, name)); err == nil {
		return Classify(ExitCLI, fmt.Errorf("an input named %s is reserved for the intake answers", name))
	}
	doc, err := json.MarshalIndent(p.Intake.Document(spec.InputsMount), "", "  ")
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("encode intake: %w", err))
	}
	doc = append(doc, '\n')
	if err := os.WriteFile(filepath.Join(rd.inputs, name), doc, 0o644); err != nil { //nolint:gosec // agent-readable by design; carries no secret values
		return Classify(ExitCLI, fmt.Errorf("stage intake: %w", err))
	}
	if err := os.WriteFile(filepath.Join(rd.receiptsDir, name), doc, 0o644); err != nil { //nolint:gosec // receipt
		return Classify(ExitCLI, fmt.Errorf("intake receipt: %w", err))
	}
	return nil
}

// renderPlanFiles copies plan/{gateway,harness}/** into the run cfg dir,
// substituting placeholders. Rendered files, not the installed ones, are
// what containers mount — the install stays pristine and reusable.
func renderPlanFiles(planDir string, rd *runDirs, subs ...string) error {
	rep := rd.replacer()
	for _, sub := range subs {
		src := filepath.Join(planDir, sub)
		err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, rerr := filepath.Rel(planDir, path)
			if rerr != nil {
				return rerr
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // walking our own plan dir
			if rerr != nil {
				return rerr
			}
			dest := filepath.Join(rd.cfg, rel)
			if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
				return mkErr
			}
			return os.WriteFile(dest, []byte(rep.Replace(string(b))), 0o644)
		})
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("render plan files: %w", err))
		}
	}
	return nil
}

// stageInputs copies operator-named files/directories into the run's
// read-only inputs channel, basename-rooted (P2 §P4). A named path that
// does not exist refuses the run — a silently absent input is a debugging
// session, not a convenience.
func stageInputs(paths []string, dest string) error {
	seen := map[string]bool{}
	for _, src := range paths {
		abs, err := filepath.Abs(src)
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("resolve input %s: %w", src, err))
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("input file: %w", err))
		}
		base := filepath.Base(abs)
		if seen[base] {
			return Classify(ExitCLI, fmt.Errorf("two inputs share the basename %q — rename one (inputs are basename-rooted under %s)", base, spec.InputsMount))
		}
		seen[base] = true
		target := filepath.Join(dest, base)
		if fi.IsDir() {
			if err := copyTree(abs, target); err != nil {
				return Classify(ExitCLI, fmt.Errorf("stage input dir %s: %w", src, err))
			}
			continue
		}
		data, err := os.ReadFile(abs) //nolint:gosec // operator-named input file
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("stage input %s: %w", src, err))
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return Classify(ExitCLI, fmt.Errorf("stage input %s: %w", src, err))
		}
	}
	return nil
}

// writeSecretsFile resolves every declared secret and writes the gateway
// secrets file. Missing ⇒ refuse before any container starts, naming the
// variable (§R4 secrets row, exit 11) — a silently-degraded agent is the
// failure mode this exists to prevent. Resolution order: the caller's
// overrides (the MCP profile, P2 §P2) first, then the policy source.
func writeSecretsFile(p Params, path string) error {
	values, err := secretValues(p)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values[k])
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return Classify(ExitCLI, fmt.Errorf("write secrets file: %w", err))
	}
	return nil
}

// secretValues resolves every secret the plan needs (declared and MCP
// server secrets) to its value, keyed the way the gateway reads them; the
// session transcript redacts these same values (§R11 invariant 7).
func secretValues(p Params) (map[string]string, error) {
	values := map[string]string{}
	missingSet := map[string]bool{}
	resolve := func(name string) (string, bool) {
		v, ok := p.SecretOverrides[name]
		if !ok || v == "" {
			v, ok = lookupSecret(name, p.Policy.SecretSource)
		}
		if !ok || v == "" {
			missingSet[name] = true
			return "", false
		}
		return v, true
	}
	for _, name := range p.Plan.Secrets {
		if v, ok := resolve(name); ok {
			values[compile.SecretKey(name)] = v
		}
	}
	// Declared MCP servers' secrets (RUNNER-PLAN-3): same resolution
	// order, keyed per server so each value is scoped to its own server
	// and reaches nothing else.
	for _, srv := range p.Plan.MCPServers {
		for _, name := range srv.Secrets {
			if v, ok := resolve(name); ok {
				values[compile.ServerSecretKey(srv.Name, name)] = v
			}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return nil, Classify(ExitPolicy, fmt.Errorf("declared secret(s) not resolvable from the MCP profile or the %s source: %s (run with --reconfigure-profile to set them)",
			p.Policy.SecretSource.Mode, strings.Join(missing, ", ")))
	}
	return values, nil
}

// SecretResolvable reports whether name would resolve at run time: from
// the overrides (an MCP profile) first, then the policy source — the same
// order writeSecretsFile uses, exposed so `agentry doctor` can predict an
// exit-11 refusal without starting anything. Values never leave here.
func SecretResolvable(name string, overrides map[string]string, src policy.SecretSource) bool {
	if v, ok := overrides[name]; ok && v != "" {
		return true
	}
	v, ok := lookupSecret(name, src)
	return ok && v != ""
}

// lookupSecret resolves one name from the operator source (§R9, Q-R3).
func lookupSecret(name string, src policy.SecretSource) (string, bool) {
	switch src.Mode {
	case "envFile":
		vals, err := parseEnvFile(src.EnvFile)
		if err != nil {
			return "", false
		}
		v, ok := vals[name]
		return v, ok
	default: // "env"
		return os.LookupEnv(name)
	}
}

// parseEnvFile reads KEY=VALUE lines ('#' comments and blanks skipped; no
// quoting or export syntax — the file format is deliberately dumb).
func parseEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, nil
}

// resolveEnv turns plan EnvVars into concrete values: literals get
// placeholder substitution, FromHostEnv resolves from the operator's
// environment (missing ⇒ exit 11, named), FromRunToken injects the
// per-run gateway token.
func resolveEnv(vars []spec.EnvVar, rep *strings.Replacer, token string) (map[string]string, error) {
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		switch {
		case v.FromRunToken:
			out[v.Name] = token
		case v.FromHostEnv != "":
			val, ok := os.LookupEnv(v.FromHostEnv)
			if !ok || val == "" {
				return nil, Classify(ExitPolicy,
					fmt.Errorf("required environment variable %s is not set (policy names it for %s)", v.FromHostEnv, v.Name))
			}
			out[v.Name] = val
		default:
			out[v.Name] = rep.Replace(v.Value)
		}
	}
	return out, nil
}

// resolveMounts substitutes placeholders in mount sources.
func resolveMounts(mounts []spec.Mount, rep *strings.Replacer) []dockerx.Mount {
	out := make([]dockerx.Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, dockerx.Mount{
			Source:   rep.Replace(m.Source),
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
			Relabel:  runScopedMount(m.Source),
		})
	}
	return out
}

// selinuxRoot exists exactly when the host runs SELinux.
const selinuxRoot = "/sys/fs/selinux"

// containerFileType is the SELinux type a container may read and write —
// the same one docker's `z` mount option applies, and with no category, so
// every container in the run shares it.
const containerFileType = "container_file_t"

// relabelForToolContainers gives the directories the GATEWAY binds into the
// tool containers a label those containers may use.
//
// Our own mounts carry docker's `z` option, but the workspace, the output
// folder, the inputs and the config dir are bound by the gateway rather
// than by us, so docker never relabels them on our behalf. Without this a
// confined tool container is refused every read: the BriefAgent's fs_read
// comes back denied, the model retries, and the run dies on the turn budget
// instead of on anything that names the real cause (agentry-cli#85).
//
// Best effort on purpose. A host without SELinux has nothing to do here,
// and where the relabel cannot be done the run continues and fails with the
// tool's own error — a second failure invented here would only obscure it.
func relabelForToolContainers(ctx context.Context, stderr io.Writer, dirs ...string) {
	if _, err := os.Stat(selinuxRoot); err != nil {
		return
	}
	chcon, err := exec.LookPath("chcon")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: SELinux is enabled but chcon is not installed — the tool containers may be refused access to this run's directories\n")
		return
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		out, cerr := exec.CommandContext(ctx, chcon, "-R", "-t", containerFileType, dir).CombinedOutput() //nolint:gosec // our own run directories
		if cerr != nil {
			_, _ = fmt.Fprintf(stderr, "note: could not label %s for the tool containers: %v %s\n", dir, cerr, bytes.TrimSpace(out))
		}
	}
}

// DockerSocket is the daemon socket the gateway mounts to launch the tool
// containers — the one path in a run that belongs to the machine.
const DockerSocket = "/var/run/docker.sock"

// mountsDockerSocket says whether a container drives the host's docker
// daemon. Such a container needs SELinux labelling switched off to reach
// the socket at all, and deriving that from the mounts rather than from a
// container's name means the exemption follows the privilege wherever it
// goes instead of being remembered separately (agentry-cli#85).
func mountsDockerSocket(mounts []spec.Mount) bool {
	for _, m := range mounts {
		if m.Source == DockerSocket {
			return true
		}
	}
	return false
}

// runScopedMount says whether a plan mount's source is a directory this run
// made — which is exactly what the run-scoped placeholders name — rather
// than a path that belongs to the machine. Only the former may be
// relabeled for SELinux (agentry-cli#85). The decision is taken on the
// UNSUBSTITUTED source on purpose: after substitution a run directory and
// a system path are both just absolute paths, and the plan's own vocabulary
// is what tells them apart. The gateway's docker socket is the case that
// matters — relabeling it would change a system-wide file's label for every
// container on the host.
func runScopedMount(source string) bool {
	for _, ph := range []string{
		spec.PHWorkspace, spec.PHOut, spec.PHCfg, spec.PHInputs,
		spec.PHServers, spec.PHSession,
	} {
		if strings.Contains(source, ph) {
			return true
		}
	}
	return false
}

// substituteAll substitutes placeholders across an argv.
func substituteAll(args []string, rep *strings.Replacer) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = rep.Replace(a)
	}
	return out
}

// readTask drains the task input (bounded: a task is an argument, not a
// dataset — big inputs belong in the workspace) and records it in the
// receipts.
func readTask(r io.Reader, receiptsDir string) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("read task input: %w", err))
	}
	_ = os.WriteFile(filepath.Join(receiptsDir, "task.txt"), b, 0o644)
	return b, nil
}

// writeMeta records run identity and timing (timestamps live here, never
// in the plan — determinism, §R4).
func writeMeta(receiptsDir, runID string, p Params, start time.Time) {
	meta := map[string]any{
		"runId":   runID,
		"ref":     p.Plan.Ref,
		"adapter": p.Plan.Harness.Adapter,
		"budgets": map[string]int{
			"timeoutSeconds": p.Budgets.TimeoutSeconds,
			"maxTurns":       p.Budgets.MaxTurns,
		},
		"started": start.UTC().Format(time.RFC3339),
		"ended":   time.Now().UTC().Format(time.RFC3339),
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(receiptsDir, "meta.json"), append(b, '\n'), 0o644)
	}
}

// gatewayDiagnostics appends the gateway log tail to a probe failure so
// the operator sees why without archaeology.
func gatewayDiagnostics(ctx context.Context, d dockerx.Client, name string) string {
	logs, err := d.Logs(ctx, name, 40)
	if err != nil || strings.TrimSpace(logs) == "" {
		return ""
	}
	return "\ngateway log tail:\n" + strings.TrimSpace(logs)
}

// stderrTail appends the agent's stderr tail to a harness failure.
func stderrTail(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // our receipts file
	if err != nil || len(b) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(b))
	lines := strings.Split(s, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return "\nagent stderr tail:\n" + strings.Join(lines, "\n")
}

// teardown runs cleanup functions in reverse registration order, then
// sweeps anything still labeled with this run (§R11 invariant 6). Each
// step is best-effort: teardown must never mask the run's own error.
type teardown struct {
	d      dockerx.Client
	stderr io.Writer
	funcs  []func(context.Context)
	runID  string
}

func (t *teardown) add(f func(context.Context)) { t.funcs = append(t.funcs, f) }

func (t *teardown) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := len(t.funcs) - 1; i >= 0; i-- {
		t.funcs[i](ctx)
	}
	if t.runID != "" {
		if leftover, err := t.d.PSByLabel(ctx, "agentry.run="+t.runID); err == nil {
			for _, name := range leftover {
				_ = t.d.RemoveForce(ctx, name)
			}
		}
	}
}

// Naming helpers: every run-scoped docker object carries the run id in its
// name and an agentry.run label for sweeps.
func containerName(runID, role string) string { return "agentry-" + runID + "-" + role }
func networkName(runID string) string         { return "agentry-" + runID + "-net" }
func runLabels(runID string) map[string]string {
	return map[string]string{"agentry.run": runID}
}

// NewRunID mints a run identifier. Exported because the CLI derives the
// default output folder name from it before handing it to Run.
func NewRunID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate gateway token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// copyTree copies src into dest preserving permission bits (same shape as
// the install exporter's copyTree; duplicated because cmd/agentry is
// package main and this must stay importable).
func copyTree(src, dest string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p) //nolint:gosec // walking the caller's own tree
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// stampOwner writes the invoking user's uid/gid into the per-run copy of
// the tools spec so the tools container — root, writing into bind mounts
// the user owns — hands back what it creates (agentry-cli#55). The
// compiled plan is untouched: ownership is a property of the run, not of
// the install, and the plan hash must not change per user. Hosts without
// POSIX ids (Getuid() < 0) leave the field out.
func stampOwner(specPath string) error {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return nil
	}
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // a plan without a tools server has no spec to stamp
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	doc["owner"] = map[string]int{"uid": uid, "gid": gid}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644)
}
