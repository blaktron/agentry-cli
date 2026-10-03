// Engine support: run-dir preparation, placeholder rendering, secret and
// env resolution, teardown. This file owns exactly the run-scoped values
// the plan must not contain (§R4/§R6): host paths, the gateway token, and
// secret values — and the guarantee that the last two die with the run.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/removal"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sockfilter"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// runDirs are the per-run host locations.
type runDirs struct {
	root        string // <RunRoot>/runs/<id>
	workspace   string // root/workspace — the rw copy tools may mount
	inputs      string // root/inputs — the :ro operator-inputs channel (P2 §P4)
	inputsText  string // root/inputs-text — the :ro decoded inputs, the host's alone to write (file-handling.md §5)
	knowledge   string // root/knowledge — the :ro knowledge packs, one directory each (knowledge.md §4.5)
	session     string // the per-run volume an interactive session's harness store lives in (DR-15, agentry-cli#90)
	cfg         string // root/cfg — rendered gateway+harness files
	secretsPath string // root/secrets.env (0600, deleted on teardown)
	serversDir  string // the signature's server store (RUNNER-PLAN-3, :ro)
	outDir      string
	receiptsDir string
	// webLedger is receipts/web.ndjson for a run with the web tools: made
	// here and labelled for the tools container, the one file of the
	// receipts it may write (web.go).
	webLedger string
	// reviewLedger is receipts/review.ndjson for a run with the review
	// grant, made as webLedger is.
	reviewLedger string
	// web says the run has the web tools, and runID names it, for the
	// receipt's daemon line.
	web   bool
	runID string
	// hostGatewayURL / hostProxyURL are the published 127.0.0.1 addresses
	// a host-run harness uses; empty for a container harness.
	hostGatewayURL string
	hostProxyURL   string
	// modelHosts are the llm-proxy's pinned model endpoints ("name:ip",
	// modelhosts.go), for the receipt.
	modelHosts []string
	// filter is the gateway's per-run docker socket (dockerfilter.go,
	// agentry-cli#219), and filterLog its decision log; nil until the
	// gateway is about to start.
	filter    *sockfilter.Filter
	filterLog *os.File
	filterDir string // the short fallback directory, when the run's was too long
	// redact masks the run's secret values out of every receipt and error
	// (redact.go); set once the token and the secrets are known, nil-safe
	// before.
	redact *redactor
}

// replacer substitutes the §R4 placeholders with this run's real values.
func (rd *runDirs) replacer() *strings.Replacer {
	return rd.replacerWith(nativePath)
}

// nativePath spells a host path as the host does: for the runner's own
// docker calls and for what a host-run harness reads.
func nativePath(p string) string { return p }

// gatewayPath spells a host path for the gateway (gatewayHostPath).
func gatewayPath(p string) string { return gatewayHostPath(p, gatewayOS) }

// gatewayReplacer is replacer for what only the gateway reads — its env
// and its rendered config files — where each host path is spelled as
// gatewayHostPath has it, the form the gateway can check and hand to the
// daemon on its socket (agentry-cli#414). The CLI's own docker calls keep
// replacer's native paths.
func (rd *runDirs) gatewayReplacer() *strings.Replacer {
	return rd.replacerWith(gatewayPath)
}

// gatewayCanMount says whether a host folder can be handed to the gateway
// for a bind. On Windows only a drive-letter path can: a network share
// (\\server\share\…) or a long-form path (\\?\C:\…) would reach the
// gateway with its backslashes, and its bind check would refuse the run
// with nothing that names the folder (agentry-cli#425).
func gatewayCanMount(path, goos string) error {
	if goos != "windows" || path == "" {
		return nil
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return fmt.Errorf("is a network share or a long-form path, which Docker Desktop cannot give a run's containers; use a folder on a local drive (C:\\…)")
	}
	if gatewayHostPath(path, goos) == path {
		return fmt.Errorf("is not on a drive (C:\\…); use a folder on a local drive")
	}
	return nil
}

// gatewayEnv resolves the gateway container's env: its bind allow-lists
// name host paths, in the gateway's spelling.
func (rd *runDirs) gatewayEnv(plan *spec.Plan, token string) (map[string]string, error) {
	return resolveEnv(plan.Gateway.Env, rd.gatewayReplacer(), token)
}

// gatewayOS is the host OS gatewayHostPath spells for; a variable so a
// test on any host can take the Windows path.
var gatewayOS = runtime.GOOS

func (rd *runDirs) replacerWith(hostPath func(string) string) *strings.Replacer {
	return strings.NewReplacer(
		spec.PHWorkspace, hostPath(rd.workspace),
		spec.PHOut, hostPath(rd.outDir),
		spec.PHCfg, hostPath(rd.cfg),
		spec.PHSecrets, hostPath(rd.secretsPath),
		spec.PHInputs, hostPath(rd.inputs),
		spec.PHInputsText, hostPath(rd.inputsText),
		spec.PHKnowledge, hostPath(rd.knowledge),
		spec.PHSession, rd.session,
		spec.PHServers, hostPath(rd.serversDir),
		spec.PHHostGatewayURL, rd.hostGatewayURL,
		spec.PHHostProxyURL, rd.hostProxyURL,
		spec.PHRunID, filepath.Base(rd.root),
	)
}

// gatewayHostPath spells a host path for the gateway, which runs in a
// Linux container and drives the daemon through its socket. On Windows a
// drive path becomes /<drive>/…: the gateway splits its bind allow-lists
// on ":" (filepath.SplitList on Linux), so a C:\ root never survives the
// list, and /c/Users/… is the form Docker Desktop's socket takes for a
// sibling container's bind of a Windows path (docker/for-win#6700).
// Elsewhere the path is unchanged.
func gatewayHostPath(p, goos string) string {
	if goos != "windows" || len(p) < 2 || p[1] != ':' {
		return p
	}
	drive := p[0] | 0x20 // ASCII lower case
	if drive < 'a' || drive > 'z' {
		return p
	}
	rest := strings.Trim(strings.ReplaceAll(p[2:], `\`, "/"), "/")
	if rest == "" {
		return "/" + string(drive)
	}
	return "/" + string(drive) + "/" + rest
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
		// The decoded inputs, written only by the host from the decode
		// step's checked output (inputs.go).
		inputsText: filepath.Join(root, "inputs-text"),
		knowledge:  filepath.Join(root, "knowledge"),
		// The session store is a per-run named volume, not a host
		// directory (agentry-cli#90): the harness runs unprivileged, and a
		// host directory it could write was a world-writable one.
		session: sessionVolumeName(runID),
		cfg:     filepath.Join(root, "cfg"),
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
	// Before anything is made: a folder the gateway could not be given as
	// a /<drive>/… path is refused by name (agentry-cli#425).
	for _, f := range []struct{ what, path string }{
		{"the runner home", p.RunRoot}, {"the output folder", p.OutDir}, {"the MCP server store", p.ServersDir},
	} {
		if err := gatewayCanMount(f.path, gatewayOS); err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("%s %s: %w", f.what, f.path, err))
		}
	}
	// inputs/, inputs-text/ and knowledge/ exist even when empty: the
	// compiled catalog always mounts them :ro, and an absent bind source
	// would fail the tools container.
	for _, dir := range []string{rd.workspace, rd.inputs, rd.inputsText, rd.knowledge, filepath.Join(rd.cfg, "gateway"), filepath.Join(p.OutDir, "reports"), rd.receiptsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("create run dirs: %w", err))
		}
	}
	// Registered before anything is staged into it: a failure in stageInputs
	// or stageIntake — or a panic — otherwise leaves the run directory, and
	// whatever was copied into it, behind for the next run to trip over
	// (agentry-cli#161). secrets.env and the run's copies of the attached
	// knowledge packs die unconditionally; the rest of the run dir only when
	// not kept for debugging (§R11 invariant 6; KeepRunDir never keeps
	// secrets, nor a pack's pages, which a pack kept sealed on the machine
	// would otherwise leave in the clear there; DR-42).
	td.add(func(context.Context) {
		td.removeSensitive(rd.secretsPath, false)
		td.removeSensitive(rd.knowledge, true)
		if !p.KeepRunDir {
			_ = os.RemoveAll(root)
		}
	})
	rd.runID, rd.web = runID, p.Plan.Capabilities.WebTools
	if p.Web != nil {
		// Made here, and only here, as a new regular file: whatever the
		// output folder held under the name is removed first, so a link a
		// person left there is not followed.
		rd.webLedger = filepath.Join(rd.receiptsDir, WebLedgerFile)
		_ = os.Remove(rd.webLedger)
		f, err := os.OpenFile(rd.webLedger, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // appended by the tools container
		if err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("create the web ledger: %w", err))
		}
		_ = f.Close()
	}
	if p.Review != nil {
		rd.reviewLedger = filepath.Join(rd.receiptsDir, ReviewLedgerFile)
		_ = os.Remove(rd.reviewLedger)
		f, err := os.OpenFile(rd.reviewLedger, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // appended by the tools container
		if err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("create the review ledger: %w", err))
		}
		_ = f.Close()
	}
	if err := stageInputs(p.InputPaths, rd.inputs); err != nil {
		return nil, err
	}
	if err := stageIntake(p, rd); err != nil {
		return nil, err
	}
	if err := copyTree(p.WorkspaceSrc, rd.workspace); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("copy workspace: %w", err))
	}
	if p.CheckWorkspace != nil {
		if err := p.CheckWorkspace(rd.workspace); err != nil {
			return nil, Classify(ExitVerify, err)
		}
	}
	// After the workspace check: a pack the release ships is staged from
	// the copy just verified.
	if err := stageKnowledge(p.Knowledge, rd.knowledge, rd.workspace); err != nil {
		return nil, err
	}
	if err := stageVoice(p.Voice, rd.knowledge); err != nil {
		return nil, err
	}
	// Gateway files now; harness files once the infrastructure is up, so a
	// host-run harness sees the published addresses (Run / StartSession).
	if err := renderGatewayConfig(p, rd); err != nil {
		return nil, err
	}
	// The plan this run executes, for audit: the verified plan in memory,
	// in the canonical encoding the receipts' runPlanSha256 hashes — not
	// plan.json re-read from disk, which could have changed since it was
	// verified and does not carry the run's own additions (the MCP
	// vulnerability verdict), so its hash would not be the receipt's
	// (agentry-cli#266).
	if b, err := p.Plan.MarshalCanonical(); err == nil {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "run-plan.json"), b, 0o644)
	}
	// Everything the gateway will hand to a tool container, labelled once
	// the directories exist and hold their content. Harness files are
	// rendered later into cfg/, and a new file takes its directory's type,
	// so they are covered by this too.
	relabelForToolContainers(context.Background(), p.Stderr, relabelTargetsFor(rd)...)
	return rd, nil
}

// intakeFile is the intake answers' name under /inputs and in the
// receipts.
const intakeFile = "intake.json"

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
	if _, err := os.Lstat(filepath.Join(rd.inputs, intakeFile)); err == nil {
		return Classify(ExitCLI, fmt.Errorf("an input named %s is reserved for the intake answers", intakeFile))
	}
	doc, err := json.MarshalIndent(p.Intake.Document(spec.InputsMount), "", "  ")
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("encode intake: %w", err))
	}
	doc = append(doc, '\n')
	if err := os.WriteFile(filepath.Join(rd.inputs, intakeFile), doc, 0o644); err != nil { //nolint:gosec // agent-readable by design; carries no secret values
		return Classify(ExitCLI, fmt.Errorf("stage intake: %w", err))
	}
	if err := os.WriteFile(filepath.Join(rd.receiptsDir, intakeFile), doc, 0o644); err != nil { //nolint:gosec // receipt
		return Classify(ExitCLI, fmt.Errorf("intake receipt: %w", err))
	}
	return nil
}

// renderGatewayConfig renders cfg/gateway: the plan's gateway files and
// what the run adds to them. The gateway alone reads them, so their host
// paths are spelled its way (gatewayReplacer, agentry-cli#414).
func renderGatewayConfig(p Params, rd *runDirs) error {
	gwRep := rd.gatewayReplacer()
	if err := renderPlanFiles(p.Plan, p.PlanDir, rd, gatewayPath, "gateway"); err != nil {
		return err
	}
	dir := filepath.Join(rd.cfg, "gateway")
	if err := stampOwner(filepath.Join(dir, "toolspec.json")); err != nil {
		return Classify(ExitCLI, fmt.Errorf("stamp owner into toolspec: %w", err))
	}
	if err := grantKnowledge(dir, gwRep.Replace(spec.PHKnowledge), p.Knowledge); err != nil {
		return Classify(ExitCLI, fmt.Errorf("grant the knowledge tools: %w", err))
	}
	if err := grantVoice(dir, gwRep.Replace(spec.PHKnowledge), p.Voice); err != nil {
		return Classify(ExitCLI, fmt.Errorf("grant the Writing Voice: %w", err))
	}
	if err := grantWeb(dir, p.Web); err != nil {
		return Classify(ExitCLI, fmt.Errorf("configure the web tools: %w", err))
	}
	if err := grantReview(dir, p.Review); err != nil {
		return Classify(ExitCLI, fmt.Errorf("configure the review grant: %w", err))
	}
	if err := writeResolvConf(dir, p.Plan); err != nil {
		return Classify(ExitCLI, fmt.Errorf("write the run's resolv.conf: %w", err))
	}
	return nil
}

// grantedTools is the tools server's grant for this run: the GrantedTools
// of the run's own rendered tools spec, after every run-time grant
// (knowledge, the voice, web, review), the same list setAllowlist gives
// the gateway. A session's driver permits exactly these, never the
// compiled plan's Tools, which no run-time attachment moves
// (agentry-cli#576).
func grantedTools(gatewayDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(gatewayDir, "toolspec.json")) //nolint:gosec // our own rendered config
	if err != nil {
		return nil, err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return nil, fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	return ts.GrantedTools(), nil
}

// renderPlanFiles copies plan/{gateway,harness}/** into the run cfg dir,
// substituting placeholders, with host paths spelled by hostPath
// (nativePath for the harness, gatewayPath for the gateway). Rendered files, not the installed ones, are
// what containers mount — the install stays pristine and reusable. Each
// file is held to the plan's hash of it as it is read: the plan was
// verified before the run started, and a file changed or added since is
// refused rather than mounted (agentry-cli#266).
func renderPlanFiles(plan *spec.Plan, planDir string, rd *runDirs, hostPath func(string) string, subs ...string) error {
	rep := rd.replacerWith(hostPath)
	// A TOML file (codex's config.toml) holds its paths in basic strings,
	// where a Windows path's backslashes are escapes: "C:\Users\…" does
	// not parse (agentry-cli#420). The host paths substituted into one are
	// escaped with the adapter's own escape; the other placeholders (the
	// run id, loopback URLs, a volume name) carry nothing to escape.
	tomlRep := rd.replacerWith(func(p string) string { return adapter.TOMLEscape(hostPath(p)) })
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
			if want, ok := plan.Files[filepath.ToSlash(rel)]; !ok || sha256Hex(b) != want {
				return &planFileChangedError{rel: filepath.ToSlash(rel)}
			}
			dest := filepath.Join(rd.cfg, rel)
			if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
				return mkErr
			}
			r := rep
			if filepath.Ext(rel) == ".toml" {
				r = tomlRep
			}
			return os.WriteFile(dest, []byte(r.Replace(string(b))), 0o644)
		})
		var changed *planFileChangedError
		if errors.As(err, &changed) {
			return Classify(ExitVerify, err)
		}
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("render plan files: %w", err))
		}
	}
	return nil
}

// planFileChangedError is a generated plan file that no longer matches
// the verified plan, or that the plan never listed.
type planFileChangedError struct{ rel string }

func (e *planFileChangedError) Error() string {
	return fmt.Sprintf("installed plan file %s changed after the plan was verified — refusing to run it; remove the plan directory to recompile", e.rel)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// The web tools' keys (web.go), for the tools container alone; each
	// is written even when empty.
	for k, v := range webSecrets(p) {
		values[k] = v
	}
	// The review's key (review.go), for the tools container alone.
	for k, v := range reviewSecrets(p) {
		values[k] = v
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
			val, err := hostSecret(v)
			if err != nil {
				return nil, Classify(ExitPolicy, err)
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
		source := rep.Replace(m.Source)
		out = append(out, dockerx.Mount{
			Source:   source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
			// A named volume (the session store) is not a host path and
			// takes no relabel: docker labels its own volumes.
			Relabel: runScopedMount(m.Source) && strings.HasPrefix(source, "/"),
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
// confined tool container is refused every read: the QuickAgent's fs_read
// comes back denied, the model retries, and the run dies on the turn budget
// instead of on anything that names the real cause (agentry-cli#85).
//
// Best effort on purpose. A host without SELinux has nothing to do here,
// and where the relabel cannot be done the run continues and fails with the
// tool's own error — a second failure invented here would only obscure it.
func relabelForToolContainers(ctx context.Context, stderr io.Writer, targets ...relabelTarget) {
	if _, err := os.Stat(selinuxRoot); err != nil {
		return
	}
	chcon, err := exec.LookPath("chcon")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: SELinux is enabled but chcon is not installed — the tool containers may be refused access to this run's directories\n")
		return
	}
	for _, t := range targets {
		if t.dir == "" {
			continue
		}
		args := []string{"-t", containerFileType}
		if t.recursive {
			args = append(args, "-R")
		}
		args = append(args, t.dir)
		out, cerr := exec.CommandContext(ctx, chcon, args...).CombinedOutput() //nolint:gosec // our own run directories
		if cerr != nil {
			_, _ = fmt.Fprintf(stderr, "note: could not label %s for the tool containers: %v %s\n", t.dir, cerr, bytes.TrimSpace(out))
		}
	}
}

// relabelTarget is one directory to label, and whether its contents go with
// it. Recursing is right for the directories the runner made under its own
// run root, and wrong for the output folder: `--out` is the operator's
// choice and may already hold files of theirs, which are none of this run's
// business (agentry-cli#88). There, the directory itself is labelled so a
// tool container can traverse into what it writes, and only reports/ — which
// the runner creates — is labelled through.
type relabelTarget struct {
	dir       string
	recursive bool
}

// relabelTargets is what a run needs labelled, and how deep. Kept separate
// from the labelling itself so the policy — recurse through what the runner
// made, never through what the operator brought — can be asserted without
// an SELinux host to run on.
func relabelTargets(rd *runDirs) []relabelTarget {
	return []relabelTarget{
		{rd.workspace, true},
		{rd.inputs, true},
		{rd.inputsText, true},
		{rd.knowledge, true},
		{rd.cfg, true},
		{rd.serversDir, true},
		{rd.outDir, false},
		{filepath.Join(rd.outDir, "reports"), true},
	}
}

// relabelTargetsFor adds, for a run with the web tools, the web ledger: the
// one file of the receipts the tools server appends to (web.go).
func relabelTargetsFor(rd *runDirs) []relabelTarget {
	t := relabelTargets(rd)
	if rd.webLedger != "" {
		t = append(t, relabelTarget{rd.webLedger, false})
	}
	if rd.reviewLedger != "" {
		t = append(t, relabelTarget{rd.reviewLedger, false})
	}
	return t
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
		spec.PHInputsText, spec.PHKnowledge, spec.PHServers, spec.PHSession,
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

// ownInstructionWhenEmpty stands in for an empty task on the autonomous
// path: the harness is sent the agent's own instruction, read from the
// run's checked workspace copy, as its first message. The run setup
// promises that ("its own instruction runs"), and an autonomous harness
// refuses an empty stdin (docker-agent exits 1 on "no message received on
// stdin"; agentry-cli#449). A task that is given is sent as it is.
//
// The second result is the receipt's word for what was sent in the task's
// place, empty when the task went as given.
func ownInstructionWhenEmpty(task []byte, plan *spec.Plan, workspace string) ([]byte, string, error) {
	if len(bytes.TrimSpace(task)) > 0 || !takesStdinTask(plan) {
		return task, "", nil
	}
	s, err := compile.ReadInstruction(workspace, plan.Instruction)
	if err != nil {
		// The plan names the file and its digest: a copy that lacks it or
		// disagrees with it is the release's fault, not the person's.
		return nil, "", Classify(ExitVerify, err)
	}
	return []byte(s), fmt.Sprintf("none given: sent the agent's own instruction, %s (sha256:%s with the System Prompt)",
		plan.Instruction.SourcePath, plan.Instruction.SHA256), nil
}

// takesStdinTask reports whether the plan's harness reads its task on
// stdin (the autonomous path, container or host process).
func takesStdinTask(plan *spec.Plan) bool {
	if plan.Harness.Host != nil {
		return plan.Harness.Host.StdinTask
	}
	return plan.Harness.Agent.StdinTask
}

// writeMeta records run identity and timing (timestamps live here, never
// in the plan — determinism, §R4).
func writeMeta(receiptsDir, runID string, p Params, start time.Time, taskSent string) {
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
	if taskSent != "" {
		// task.txt keeps the task as given; this says what went instead.
		meta["task"] = taskSent
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
	// networks are the ones the run made, removed again after the sweep.
	networks []string
	// leftFiles are what removeSensitive could not delete, with why:
	// reportLeftFiles tells the person and the receipt (agentry-cli#422).
	leftFiles []leftFile
	d         dockerx.Client
	stderr    io.Writer
	funcs     []func(context.Context)
	runID     string
}

func (t *teardown) add(f func(context.Context)) { t.funcs = append(t.funcs, f) }

// removeSensitivePath is removal.Remove, a variable so a test can hold a
// file the way Docker Desktop can.
var removeSensitivePath = removal.Remove

type leftFile struct {
	path string
	err  error
}

// removeSensitive deletes what must not outlive the run (secrets.env, the
// run's copies of knowledge packs), retrying on Windows while Docker
// Desktop lets go of it, and records it if it still stands.
func (t *teardown) removeSensitive(path string, all bool) {
	if err := removeSensitivePath(path, all); err != nil {
		t.leftFiles = append(t.leftFiles, leftFile{path, err})
	}
}

// reportLeftFiles says what teardown could not delete, after the whole
// teardown (the sweep included) has run: each is tried once more, since a
// container only the sweep removed may have held it, and what is still
// there becomes a warning on stderr, a teardown line in the receipt
// (result.json is written again with it) and an error, so a run that
// leaves a secret or a pack's pages on disk does not end as if it had not.
func (t *teardown) reportLeftFiles(env *receipt.Envelope) error {
	var still []string
	for _, l := range t.leftFiles {
		err := removeSensitivePath(l.path, true)
		if err == nil || removal.Gone(l.path) {
			continue
		}
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err // the path is named already
		}
		still = append(still, fmt.Sprintf("%s (%v)", l.path, err))
	}
	if len(still) == 0 {
		return nil
	}
	what, them := "it", "it"
	if len(still) > 1 {
		what, them = "they", "them"
	}
	msg := "could not delete " + strings.Join(still, "; ") + ": " + what +
		" may hold secret values or a knowledge pack's pages in the clear; delete " + them + " by hand"
	_, _ = fmt.Fprintf(t.stderr, "warning: %s\n", msg)
	if env == nil {
		return Classify(ExitCLI, errors.New(msg))
	}
	if env.CapabilitiesEnforced == nil {
		env.CapabilitiesEnforced = map[string]string{}
	}
	env.CapabilitiesEnforced["teardown"] = msg
	switch {
	case env.Status == "ok":
		env.Status, env.ExitCode, env.Error = "error", ExitCLI, msg
	case env.Error == "":
		env.Error = msg
	default:
		env.Error += "; and " + msg
	}
	if env.OutDir != "" {
		if err := env.Write(env.OutDir); err != nil {
			_, _ = fmt.Fprintf(t.stderr, "warning: %v\n", err)
		}
	}
	return Classify(ExitCLI, errors.New(msg))
}

func (t *teardown) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := len(t.funcs) - 1; i >= 0; i-- {
		t.funcs[i](ctx)
	}
	if t.runID != "" {
		if leftover, err := t.d.PSByLabel(ctx, runLabel+"="+t.runID); err == nil {
			for _, name := range leftover {
				_ = t.d.RemoveForce(ctx, name)
			}
		}
		// The run's networks, again, now that every container carrying the
		// run's label is gone (the gateway's among them, labelled by its
		// filter): one left attached earlier would have kept its network.
		for _, n := range t.networks {
			_ = t.d.NetworkRemove(ctx, n)
		}
		// Volumes carry the same label (the session store): after the
		// containers, never before, since a volume in use cannot go.
		if leftover, err := t.d.VolumesByLabel(ctx, runLabel+"="+t.runID); err == nil {
			for _, name := range leftover {
				_ = t.d.VolumeRemove(ctx, name)
			}
		}
	}
}

// Naming helpers: every run-scoped docker object carries the run id in its
// name and an agentry.run label for sweeps, plus the owner labels (package
// owner) that let a later run tell a crashed run's leftovers from a live
// sibling's (SweepAbandoned).
func containerName(runID, role string) string { return "agentry-" + runID + "-" + role }
func networkName(runID string) string         { return "agentry-" + runID + "-net" }
func proxyEgressNetwork(runID string) string  { return "agentry-" + runID + "-egress" }

func sessionVolumeName(runID string) string { return "agentry-" + runID + "-session" }
func runLabels(runID string) map[string]string {
	return owner.With(map[string]string{runLabel: runID})
}

// runLabel keys every run-scoped object by its run id.
const runLabel = "agentry.run"

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

// keyringLookup reads a keychain account (spec.EnvVar.FromKeyring); a seam
// for tests, which have no keychain.
var keyringLookup = modelkey.LookupAccount

// keyringOnce memoises keychain reads for the process: the redactor and
// the run's own resolution ask for the same entry, and one read means one
// unlock prompt and one value — the redactor can never have learned a
// different key than the run injects.
var keyringOnce = struct {
	sync.Mutex
	vals map[string]string
	errs map[string]error
}{vals: map[string]string{}, errs: map[string]error{}}

func lookupKeyringOnce(account string) (string, error) {
	keyringOnce.Lock()
	defer keyringOnce.Unlock()
	if v, ok := keyringOnce.vals[account]; ok {
		return v, nil
	}
	if err, ok := keyringOnce.errs[account]; ok {
		return "", err
	}
	v, err := keyringLookup(account)
	if err != nil {
		keyringOnce.errs[account] = err
		return "", err
	}
	keyringOnce.vals[account] = v
	return v, nil
}

// resetKeyringOnce forgets memoised reads (tests).
func resetKeyringOnce() {
	keyringOnce.Lock()
	defer keyringOnce.Unlock()
	keyringOnce.vals = map[string]string{}
	keyringOnce.errs = map[string]error{}
}

// checkKind holds the value a plan resolved against the credential kind
// the plan was compiled for (agentry-cli#171): the plan fixed the
// harness's argv and variable by that kind, so a value of another kind
// would run a plan that is wrong for it. A credential of a shape this
// build does not know reads as an API key, the endpoint being the judge.
// The redactor (hostSecretValues) never sees a value refused here, which
// is fine: resolveEnv refuses on the same error before any process
// starts.
func checkKind(v spec.EnvVar, val, from string) error {
	if v.ExpectedKind == nil {
		return nil
	}
	found := policy.CredentialKindOf(v.ExpectedKind.Provider, val)
	if found == v.ExpectedKind.Kind {
		return nil
	}
	return fmt.Errorf("model key for provider %s: the policy says the credential is %s (apiKeyKind) but the value in %s is %s — record the kind: `agentry setup --api-key-kind %s`, or store the key again (setup and the app read the kind from it)",
		v.ExpectedKind.Provider, v.ExpectedKind.Kind, from, found, found)
}

// hostSecret resolves a FromHostEnv variable: the environment when the
// variable is set, else the keychain account the plan names (policy
// apiKeySource keyring), else a refusal that names the provider, the
// variable and the fix — and holds the value against the kind the plan
// expects (checkKind).
func hostSecret(v spec.EnvVar) (string, error) {
	if val, ok := os.LookupEnv(v.FromHostEnv); ok && val != "" {
		if err := checkKind(v, val, "$"+v.FromHostEnv); err != nil {
			return "", err
		}
		return val, nil
	}
	if v.FromKeyring != "" {
		val, err := lookupKeyringOnce(v.FromKeyring)
		if err != nil {
			return "", fmt.Errorf("model key for provider %s: $%s is not set and %w", modelkey.ProviderOf(v.FromKeyring), v.FromHostEnv, err)
		}
		if err := checkKind(v, val, "the OS keychain entry "+v.FromKeyring); err != nil {
			return "", err
		}
		return val, nil
	}
	if v.Name != v.FromHostEnv {
		return "", fmt.Errorf("required environment variable %s is not set (the policy names it; the harness reads it as %s)", v.FromHostEnv, v.Name)
	}
	return "", fmt.Errorf("required environment variable %s is not set (the policy names it)", v.FromHostEnv)
}
