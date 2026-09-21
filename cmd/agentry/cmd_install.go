// `agentry install` (§11): resolve → download → verify digest AND the DSSE
// attestation signature (§8.8) → show the declared capability contract,
// diffed against whatever this machine last installed → extract → export to
// one §5.6 target (claude-skill or agents-md).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/term"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/export"
	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/pathguard"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// installOpts carries the install command's flags.
type installOpts struct {
	target        string
	provider      string
	dir           string
	yes           bool
	allowUnsigned bool
	// allowUnscanned admits a version the registry published without a
	// scan (M22, agentry-cli#146): its own consent, which --allow-unsigned
	// does not give.
	allowUnscanned bool
	// plan stops after the consent preview and writes nothing (agentry-cli#102):
	// the same resolve, download, digest and attestation checks, then the
	// contract, its diff against what this machine accepted, the target and
	// the directory, as JSON with --json — so a client can show the person
	// exactly what an install would do and confirm it against a digest.
	plan bool
}

// providerTargets maps the per-provider install entry points
// (RUNNER-PLAN-2 §P5) onto the §5.6 targets: the BriefAgent then runs on
// that application's own MCP servers and built-in tools, never through the
// docker gateway path.
var providerTargets = map[string]string{
	"claude":   manifest.InterfaceClaudeSkill,
	"codex":    manifest.InterfaceAgentsMD,
	"openclaw": manifest.InterfaceClaudeSkill, // OpenClaw consumes Claude-format skills today
}

// providerNames lists the accepted --provider values, sorted for messages.
func providerNames() []string {
	names := make([]string, 0, len(providerTargets))
	for n := range providerTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveProviderTarget applies --provider, enforcing mutual exclusion
// with --target before any network call.
func resolveProviderTarget(opts *installOpts) error {
	if opts.provider == "" {
		return nil
	}
	if opts.target != "" {
		return errors.New("--provider and --target are mutually exclusive (a provider IS a target choice)")
	}
	t, ok := providerTargets[strings.ToLower(opts.provider)]
	if !ok {
		return fmt.Errorf("unknown --provider %q (want %s)", opts.provider, strings.Join(providerNames(), ", "))
	}
	opts.target = t
	return nil
}

// newInstallCmd builds the install command.
func newInstallCmd() *cobra.Command {
	var opts installOpts
	cmd := &cobra.Command{
		Use:   "install <ns/name[@range]>",
		Short: "Resolve, download, verify, and export a BriefAgent to a runtime format",
		Long: "Resolve ns/name[@range] (§9.4), download the release archive, verify its\n" +
			"sha256 digest AND its DSSE attestation signature (§8.8), show the declared\n" +
			"capability contract (§5.4) diffed against the installed version, and export\n" +
			"to one install target (§5.6):\n\n" +
			"  claude-skill  {--dir | ~/.claude/skills}/<name>/ with SKILL.md frontmatter\n" +
			"  agents-md     extract to {--dir | ./agents/<name>} and update ./AGENTS.md\n\n" +
			"Signatures: a release the registry has attested must verify against the\n" +
			"signing keys pinned for that registry, or the install aborts. A registry\n" +
			"that publishes no attestation yields an unsigned install, which requires\n" +
			"--allow-unsigned (in CI as well as at a terminal). A version the registry\n" +
			"admitted WITHOUT a scan — a private organization agent its administrator\n" +
			"took off the pipeline — is unscanned: no findings, no report and no\n" +
			"attestation stand behind it, and it requires --allow-unscanned, which is\n" +
			"its own decision (--allow-unsigned does not give it).\n\n" +
			"Capabilities: declared network egress or exec requires typed confirmation,\n" +
			"and so does any capability WIDENED since the version currently installed\n" +
			"here (a new egress host, new exec commands, escalated filesystem access,\n" +
			"a new secret). Narrowed or unchanged contracts install without asking.\n" +
			"Pass --yes for CI.\n\n" +
			"Reinstall: a directory the CLI chose (the per-target defaults above) is\n" +
			"CLEARED first, so files dropped by an older version never linger. A\n" +
			"directory you named with --dir is MERGED into, never cleared — it is your\n" +
			"directory and may hold things this command did not put there.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.target, "target", "", "install target: claude-skill | agents-md (default: the first of the release's runtime.interfaces)")
	cmd.Flags().StringVar(&opts.provider, "provider", "", "install for an agent application: claude | codex | openclaw (picks the target; RUNNER-PLAN-2 §P5)")
	cmd.Flags().StringVar(&opts.dir, "dir", "", "override the target directory (see help for per-target defaults)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "skip the capability confirmation prompt (CI)")
	cmd.Flags().BoolVar(&opts.allowUnsigned, "allow-unsigned", false,
		"install a release the registry has published no attestation for")
	cmd.Flags().BoolVar(&opts.allowUnscanned, "allow-unscanned", false,
		"install a release the registry admitted without a scan (no findings, no report, no attestation)")
	cmd.Flags().BoolVar(&opts.plan, "plan", false,
		"preview only: resolve, download and verify, then show the contract, its diff and where it would go; writes nothing but a first-use pin of the registry's keys (with --json, the machine-readable shape)")
	return cmd
}

// installPlan is the `install --plan --json` shape (agentry-cli#102): what an
// install would do, with nothing written. `contract` is the manifest's
// capabilities as canonical JSON (sorted keys), and `contractDigest` its
// sha256, so a client can show the contract, hand the digest to the person,
// and confirm an install against the digest of exactly what was shown.
type installPlan struct {
	Ref       string   `json:"ref"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Digest    string   `json:"digest"`
	Modes     []string `json:"modes"`
	Summary   string   `json:"summary,omitempty"`
	Yanked    bool     `json:"yanked,omitempty"`
	// Contract is capabilities as canonical JSON; ContractDigest its sha256.
	Contract       json.RawMessage `json:"contract"`
	ContractDigest string          `json:"contractDigest"`
	// Attestation is verified | unsigned | unscanned | mismatch |
	// unverifiable, with the key id when verified; Signed is Attestation ==
	// verified. An install refuses mismatch and unverifiable outright,
	// unsigned without --allow-unsigned, and unscanned without
	// --allow-unscanned.
	Attestation string `json:"attestation"`
	KeyID       string `json:"keyId,omitempty"`
	Signed      bool   `json:"signed"`
	// Installed is what this machine last accepted for the same registry,
	// namespace, name, target and directory, when there is a record.
	Installed *installPlanPrior `json:"installed,omitempty"`
	// Diff is the capability diff against Installed (empty on a first install).
	Diff installed.Diff `json:"diff"`
	// NeedsConfirmation says whether an install would ask (or need --yes):
	// a first install that declares network egress or exec, or an update
	// that widens the contract. Reason says which.
	NeedsConfirmation bool   `json:"needsConfirmation"`
	Reason            string `json:"reason,omitempty"`
	// Target and Dir are where an install would write; Managed is true when
	// the CLI chose Dir (and would clear it on reinstall).
	Target  string `json:"target"`
	Dir     string `json:"dir"`
	Managed bool   `json:"managed"`
	// Refusal is set when an install would stop before writing: the
	// attestation verdict or the missing --allow-unsigned. Empty otherwise.
	Refusal string `json:"refusal,omitempty"`
}

type installPlanPrior struct {
	Version string `json:"version"`
	Dir     string `json:"dir"`
}

// installResult is the `install --json` shape: what was written where, and
// the contract recorded as accepted.
type installResult struct {
	Ref            string          `json:"ref"`
	Namespace      string          `json:"namespace"`
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	Digest         string          `json:"digest"`
	Target         string          `json:"target"`
	Dir            string          `json:"dir"`
	Managed        bool            `json:"managed"`
	Attestation    string          `json:"attestation"`
	Signed         bool            `json:"signed"`
	Contract       json.RawMessage `json:"contract"`
	ContractDigest string          `json:"contractDigest"`
	// Recorded is true when the shared install state took the record.
	Recorded bool `json:"recorded"`
}

// contractJSON renders capabilities as canonical JSON with its sha256: the
// same form pkg/manifest.CanonicalJSON gives a whole manifest — keys sorted
// at every level (encoding/json sorts map keys), nulls and empty objects and
// arrays pruned — applied to the capabilities value alone, so two identical
// contracts digest identically whatever their source's field order.
func contractJSON(caps manifest.Capabilities) (json.RawMessage, string, error) {
	raw, err := json.Marshal(caps)
	if err != nil {
		return nil, "", fmt.Errorf("canonical contract: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, "", fmt.Errorf("canonical contract: %w", err)
	}
	b, err := json.Marshal(pruneEmptyJSON(v))
	if err != nil {
		return nil, "", fmt.Errorf("canonical contract: %w", err)
	}
	sum := sha256.Sum256(b)
	return b, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// pruneEmptyJSON mirrors pkg/manifest's pruneEmpty: nulls, empty objects and
// empty arrays leave object values; array elements stay as they are.
func pruneEmptyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			pv := pruneEmptyJSON(val)
			if pv == nil {
				continue
			}
			if m, ok := pv.(map[string]any); ok && len(m) == 0 {
				continue
			}
			if a, ok := pv.([]any); ok && len(a) == 0 {
				continue
			}
			out[k] = pv
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = pruneEmptyJSON(val)
		}
		return out
	default:
		return v
	}
}

// validTargets is the §5.6 closed set. The mcp target left with the
// withdrawn v1 tool-server package kind (DEV-088).
var validTargets = []string{manifest.InterfaceClaudeSkill, manifest.InterfaceAgentsMD}

// validateTarget checks an explicitly requested target. It runs BEFORE any
// network call: a typo in --target should cost a usage message, not a
// download, a capability prompt, and an extraction that is then thrown away.
func validateTarget(target string) error {
	if target == "" {
		return nil // resolved from the manifest's interfaces once it arrives
	}
	for _, t := range validTargets {
		if target == t {
			return nil
		}
	}
	return fmt.Errorf("unknown --target %q (want %s)", target, strings.Join(validTargets, ", "))
}

// runInstall executes the install flow.
func runInstall(cmd *cobra.Command, ref string, opts installOpts) error {
	if _, err := manifest.ParseRef(ref); err != nil {
		return err
	}
	if err := resolveProviderTarget(&opts); err != nil {
		return err
	}
	if err := validateTarget(opts.target); err != nil {
		return err
	}
	reg := registryURL()
	token := credentialFor(reg)
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if flags.jsonOut {
		// §R8: stdout carries the result; the narration moves to stderr.
		out = errOut
	}

	res, err := resolveRef(cmd.Context(), client, ref, true)
	if err != nil {
		return err
	}
	if res.Yanked {
		_, _ = fmt.Fprintf(errOut, "warning: %s/%s@%s is YANKED — installing only because the version was pinned exactly\n",
			res.Namespace, res.Name, res.Version)
	}
	m, err := manifestFromResolve(res)
	if err != nil {
		return err
	}
	// The resolve response's manifest is covered by neither the digest nor
	// the attestation (both are over the archive), and the install directory
	// and the skill file are derived from it: hold it to the publish rules
	// before any path is, and require it to name the resolved release
	// (security review 2026-09-20 §4.7).
	if err := requireValidManifest(cmd, m, fmt.Sprintf("the registry's manifest for %s/%s@%s", res.Namespace, res.Name, res.Version)); err != nil {
		return err
	}
	if m.Name != res.Name || m.Namespace != res.Namespace {
		return fmt.Errorf("the registry's manifest names %s/%s but the release is %s/%s — refusing to install", m.Namespace, m.Name, res.Namespace, res.Name)
	}

	archive, err := downloadArchive(cmd.Context(), reg, token, res)
	if err != nil {
		return err
	}
	computed := sha256Digest(archive)
	if computed != res.Digest {
		return fmt.Errorf("digest mismatch: registry says %s, archive is %s — refusing to install", res.Digest, computed)
	}

	// Provenance (§8.8, §11 "verify digest+signature"). The digest above only
	// says the bytes match what the registry SAID; the attestation says who
	// vouched for them. Verified against the digest we just computed, so a
	// valid signature over some other archive proves nothing here.
	att := installAttestation(cmd, reg, token, res, computed)
	attErr := judgeAttestation(out, errOut, att, res, opts.allowUnsigned, opts.allowUnscanned, opts.plan)
	if attErr != nil && !opts.plan {
		return attErr
	}

	target := opts.target
	if target == "" {
		target = defaultTarget(m)
	}
	warnUndeclaredTarget(errOut, m, target)
	paths, err := resolveInstallPaths(target, opts.dir, m.Name)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s@%s (%s)\n", res.Namespace, res.Name, res.Version, modesLabel(m.SupportedModes()))
	if m.Summary != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", m.Summary)
	}
	_, _ = fmt.Fprintln(out, "capabilities:")
	for _, line := range capabilityLines(m.Capabilities) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}

	key := installed.Key{
		Registry: reg, Namespace: res.Namespace, Name: m.Name,
		Target: target, Dir: paths.dest,
	}
	state, serr := installed.Load()
	if serr != nil {
		// Bookkeeping trouble must not block an install; it costs a prompt.
		_, _ = fmt.Fprintf(errOut, "warning: %v — treating this as a first install\n", serr)
		state = installed.State{}
	}
	nextCaps := installed.Snapshot(m.Capabilities)
	if opts.plan {
		return writeInstallPlan(cmd, res, m, computed, att, attErr, &state, key, nextCaps, target, paths)
	}
	if err := confirmCapabilityChange(cmd, out, &state, key, res.Version, nextCaps, m.Capabilities, opts.yes); err != nil {
		return err
	}

	staging, err := os.MkdirTemp("", "agentry-install-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// The hardened extractor (internal/scan): it is the one the pipeline runs
	// on submission, so an archive that survived the server's unpack is
	// unpacked here by the same rules — path escapes, symlinks, device nodes,
	// file-count and compression-ratio bombs all refused rather than written.
	if err := pkgarchive.Extract(bytes.NewReader(archive), staging, int64(len(archive)), installExtractLimits()); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}

	// The archive is the verified bytes; when it carries its manifest, that
	// is the one the export reads (a v1 release may ship without one, and
	// then the resolve manifest validated above stands).
	if am, err := manifestFromInstallArchive(staging, m, res); err != nil {
		return err
	} else {
		m = am
	}

	switch target {
	case manifest.InterfaceClaudeSkill:
		err = exportClaudeSkill(out, staging, paths, m, res.Namespace, res.Version)
	case manifest.InterfaceAgentsMD:
		err = exportAgentsMD(out, staging, paths, m, res.Namespace, res.Version)
	default:
		// Unreachable: validateTarget and defaultTarget between them admit
		// only the two above. Kept so a third interface cannot be added
		// without this switch failing loudly.
		return fmt.Errorf("unknown --target %q (want %s)", target, strings.Join(validTargets, ", "))
	}
	if err != nil {
		return err
	}

	// Record only after the files actually landed: a manifest claiming an
	// install that failed would suppress the next run's capability prompt.
	state.Upsert(key, res.Version, nextCaps, paths.managed)
	recorded := true
	if err := installed.Save(state); err != nil {
		recorded = false
		_, _ = fmt.Fprintf(errOut, "warning: %v — the next install of this agent will re-ask about capabilities\n", err)
	}
	if flags.jsonOut {
		contract, cdigest, err := contractJSON(m.Capabilities)
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), installResult{
			Ref: res.Namespace + "/" + res.Name + "@" + res.Version, Namespace: res.Namespace, Name: m.Name,
			Version: res.Version, Digest: computed, Target: target, Dir: paths.dest, Managed: paths.managed,
			Attestation: string(att.State), Signed: att.State == trust.StateVerified,
			Contract: contract, ContractDigest: cdigest, Recorded: recorded,
		})
	}
	return nil
}

// writeInstallPlan is the --plan exit: the preview as text or JSON, nothing
// written, and exit 0 whether or not an install would be refused — the
// refusal is part of what the plan reports.
func writeInstallPlan(cmd *cobra.Command, res *apiclient.ResolveResult, m *manifest.Manifest, digest string,
	att trust.AttestationResult, attErr error, state *installed.State, key installed.Key,
	next installed.CapSnapshot, target string, paths installPaths,
) error {
	contract, cdigest, err := contractJSON(m.Capabilities)
	if err != nil {
		return err
	}
	plan := installPlan{
		Ref: res.Namespace + "/" + res.Name + "@" + res.Version, Namespace: res.Namespace, Name: m.Name,
		Version: res.Version, Digest: digest, Modes: modeStrings(m.SupportedModes()), Summary: m.Summary, Yanked: res.Yanked,
		Contract: contract, ContractDigest: cdigest,
		Attestation: string(att.State), KeyID: att.KeyID, Signed: att.State == trust.StateVerified,
		Target: target, Dir: paths.dest, Managed: paths.managed,
	}
	if attErr != nil {
		plan.Refusal = attErr.Error()
	}
	if prev := state.Find(key); prev != nil {
		plan.Installed = &installPlanPrior{Version: prev.Version, Dir: prev.Dir}
		plan.Diff = installed.DiffCapabilities(prev.Capabilities, next)
		if plan.Diff.HasWidened() {
			plan.NeedsConfirmation = true
			plan.Reason = "this update widens the declared capabilities"
		}
	} else if capabilityNeedsConfirm(m.Capabilities) {
		plan.NeedsConfirmation = true
		plan.Reason = "a first install of a BriefAgent that declares network egress or exec"
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), plan)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "plan: %s -> %s (%s)\n", plan.Ref, plan.Dir, plan.Target)
	if plan.Installed != nil {
		_, _ = fmt.Fprintf(out, "installed: %s at %s\n", plan.Installed.Version, plan.Installed.Dir)
		for _, l := range plan.Diff.Widened {
			_, _ = fmt.Fprintf(out, "  + this update %s\n", l)
		}
		for _, l := range plan.Diff.Narrowed {
			_, _ = fmt.Fprintf(out, "  - this update %s\n", l)
		}
	}
	if plan.NeedsConfirmation {
		_, _ = fmt.Fprintf(out, "would ask: %s\n", plan.Reason)
	}
	if plan.Refusal != "" {
		_, _ = fmt.Fprintf(out, "would refuse: %s\n", plan.Refusal)
	}
	_, _ = fmt.Fprintln(out, "nothing written (--plan)")
	return nil
}

// emitJSON prints one JSON document, compact and newline-terminated. Compact
// on purpose: an embedded canonical `contract` keeps its exact bytes, so the
// digest beside it is the digest of what the reader holds.
func emitJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// installExtractLimits bounds what the CLI unpacks. Looser than the server's
// §19 publish quotas (which the archive already passed) because the client's
// job here is only to refuse a bomb, not to re-adjudicate the release.
func installExtractLimits() pkgarchive.ExtractLimits {
	return pkgarchive.ExtractLimits{
		MaxFileBytes:  extractMaxFileBytes,
		MaxTotalBytes: extractMaxTotalBytes,
		MaxFiles:      20000,
		MaxRatio:      500,
	}
}

// verifyInstallAttestation enforces the §11 install-time signature check.
//
// Three outcomes, and the difference between them is the whole point:
//   - verified   → proceed, naming the key
//   - mismatch / unverifiable → ABORT. A signature that does not check out is
//     worse evidence than no signature at all, because someone produced it.
//   - unsigned   → the registry offers no attestation. Proceed only behind
//     --allow-unsigned, in CI as well as at a terminal: a flag is a decision
//     someone wrote down, a TTY prompt is a decision someone clicked past.
func installAttestation(cmd *cobra.Command, reg, token string, res *apiclient.ResolveResult, archiveDigest string) trust.AttestationResult {
	return attestationOf(cmd.Context(), reg, token, res, archiveDigest)
}

// attestationOf is the one read of a release's provenance install, run and
// verify share: a version the registry admitted without a scan is
// unscanned before any attestation is looked for — the absence is the
// fact, not a missing signature — and every other version's attestation
// is fetched and verified against the digest just computed.
func attestationOf(ctx context.Context, reg, token string, res *apiclient.ResolveResult, archiveDigest string) trust.AttestationResult {
	if res.ScanStatus != nil && *res.ScanStatus == apiclient.ResolveResultScanStatusSkipped {
		return trust.Unscanned()
	}
	attURL := ""
	if res.AttestationUrl != nil {
		attURL = *res.AttestationUrl
	}
	if attURL == "" {
		// Wave 1 populates attestationUrl, but a registry that omits it may
		// still have an attestation at the canonical path — ask before
		// concluding "unsigned".
		attURL = fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/attestation", res.Namespace, res.Name, res.Version)
	}
	return trust.VerifyAttestation(ctx, httpClient, reg, token, attURL, archiveDigest)
}

// judgeAttestation narrates the verdict and returns the install's refusal,
// if any; --plan reports the refusal instead of stopping on it.
func judgeAttestation(out, errOut io.Writer, att trust.AttestationResult, res *apiclient.ResolveResult, allowUnsigned, allowUnscanned, plan bool) error {
	switch att.State {
	case trust.StateVerified:
		_, _ = fmt.Fprintf(out, "attestation:   verified (key %s)\n", att.KeyID)
		if att.RulesetVer != "" {
			_, _ = fmt.Fprintf(out, "  scanned with ruleset %s, judge %s\n", att.RulesetVer, orNone(att.JudgeModel))
		}
		if att.FirstUse {
			_, _ = fmt.Fprintln(errOut, "note: signing keys pinned on first use (TOFU) — run `agentry login` to pin deliberately")
		}
		return nil

	case trust.StateUnsigned:
		if !allowUnsigned {
			return fmt.Errorf("unsigned — registry offers no attestation for %s/%s@%s (%s); "+
				"re-run with --allow-unsigned to install it anyway",
				res.Namespace, res.Name, res.Version, att.Detail)
		}
		doing := "installing"
		if plan {
			doing = "an install would proceed"
		}
		_, _ = fmt.Fprintf(errOut,
			"warning: unsigned — registry offers no attestation for %s/%s@%s; %s because --allow-unsigned was given\n",
			res.Namespace, res.Name, res.Version, doing)
		return nil

	case trust.StateUnscanned:
		// Its own consent: an unscanned release is not an unsigned one,
		// and --allow-unsigned says nothing about it.
		if !allowUnscanned {
			return fmt.Errorf("unscanned — the registry admitted %s/%s@%s without a scan: no findings, no report and no attestation stand behind it; "+
				"re-run with --allow-unscanned to install it anyway, accepting that nothing about its contents was checked",
				res.Namespace, res.Name, res.Version)
		}
		doing := "installing"
		if plan {
			doing = "an install would proceed"
		}
		_, _ = fmt.Fprintf(errOut,
			"warning: unscanned — the registry admitted %s/%s@%s without a scan; %s because --allow-unscanned was given\n",
			res.Namespace, res.Name, res.Version, doing)
		return nil

	default: // "mismatch", "unverifiable"
		hint := ""
		if att.KeyRotated {
			hint = " (the registry advertises signing key ids you have not pinned — re-pin deliberately if this rotation is expected)"
		}
		return fmt.Errorf("attestation %s for %s/%s@%s: %s%s — refusing to install",
			att.State, res.Namespace, res.Name, res.Version, att.Detail, hint)
	}
}

// confirmCapabilityChange applies the §11 install gate.
//
// A first install asks about network egress or exec, as before. A REINSTALL
// compares against what this machine last accepted for this exact target
// directory: widened capabilities always ask (that is the case §11 names),
// narrowed or unchanged ones say so and continue — re-asking an identical
// question every upgrade is how a security prompt becomes a reflex.
//
// The banner writer is explicit because callers disagree about stdout:
// install narrates there, but `agentry run` reserves stdout for the result
// (RUNNER-PLAN §R8) and sends this to stderr.
func confirmCapabilityChange(cmd *cobra.Command, out io.Writer, state *installed.State, key installed.Key,
	version string, next installed.CapSnapshot, caps manifest.Capabilities, yes bool,
) error {
	prev := state.Find(key)
	if prev == nil {
		return confirmCapabilities(cmd, caps, yes)
	}

	diff := installed.DiffCapabilities(prev.Capabilities, next)
	_, _ = fmt.Fprintf(out, "installed: %s at %s\n", prev.Version, prev.Dir)
	switch {
	case diff.Empty():
		_, _ = fmt.Fprintf(out, "capability diff vs %s: unchanged\n", prev.Version)
		return nil
	case !diff.HasWidened():
		_, _ = fmt.Fprintf(out, "capability diff vs %s: narrowed\n", prev.Version)
		for _, line := range diff.Narrowed {
			_, _ = fmt.Fprintf(out, "  - this update %s\n", line)
		}
		return nil
	}

	_, _ = fmt.Fprintf(out, "capability diff vs %s: WIDENED\n", prev.Version)
	for _, line := range diff.Widened {
		_, _ = fmt.Fprintf(out, "  + this update %s\n", line)
	}
	for _, line := range diff.Narrowed {
		_, _ = fmt.Fprintf(out, "  - this update %s\n", line)
	}
	if yes {
		return nil
	}
	prompt := fmt.Sprintf("this update to %s widens what %s/%s may do — continue? type \"yes\": ",
		version, key.Namespace, key.Name)
	return promptYes(cmd, prompt,
		"this update widens the BriefAgent's declared capabilities; re-run with --yes to accept in non-interactive mode",
		"install aborted (widened capability contract not accepted)")
}

// confirmCapabilities enforces the §11 install gate: declared network egress
// or exec needs typed confirmation, skipped by --yes; non-interactive runs
// must pass --yes.
func confirmCapabilities(cmd *cobra.Command, caps manifest.Capabilities, yes bool) error {
	if yes || !capabilityNeedsConfirm(caps) {
		return nil
	}
	return promptYes(cmd,
		"this BriefAgent declares network egress and/or exec — type \"yes\" to continue: ",
		"this BriefAgent declares network egress and/or exec; re-run with --yes to accept in non-interactive mode",
		"install aborted (capability contract not accepted)")
}

// promptYes asks for a typed "yes" on an interactive terminal. Off a TTY it
// refuses with notTTYMsg rather than reading: a piped stdin cannot consent,
// and treating EOF as assent is how a confirmation gate stops being one.
func promptYes(cmd *cobra.Command, prompt, notTTYMsg, refusedMsg string) error {
	if !stdinIsTTY(cmd.InOrStdin()) {
		return errors.New(notTTYMsg)
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New(refusedMsg)
	}
	return nil
}

// installPaths is where one install target writes, resolved before anything
// is downloaded so the capability diff can be keyed on the real destination.
type installPaths struct {
	// dest is the directory the archive's files land in.
	dest string
	// managed is true when the CLI chose dest from the target's default.
	// Only a managed directory is cleared on reinstall — a --dir the user
	// named is theirs, and may hold files this command did not put there.
	managed bool
	// managedRoot is the directory a managed dest sits directly under
	// (~/.claude/skills, or ./agents): the one shape a managed clear may
	// have, so a dest derived from a bad name cannot be cleared whatever
	// it resolved to.
	managedRoot string
}

// resolveInstallPaths computes the destination for a target without touching
// the filesystem, so an unwritable home or an unknown target is reported
// before a download rather than after one.
func resolveInstallPaths(target, dir, name string) (installPaths, error) {
	switch target {
	case manifest.InterfaceClaudeSkill:
		if dir != "" {
			return installPaths{dest: filepath.Join(dir, name)}, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return installPaths{}, fmt.Errorf("locate home directory (pass --dir): %w", err)
		}
		root := filepath.Join(home, ".claude", "skills")
		return installPaths{dest: filepath.Join(root, name), managed: true, managedRoot: root}, nil

	case manifest.InterfaceAgentsMD:
		if dir != "" {
			return installPaths{dest: dir}, nil
		}
		return installPaths{dest: filepath.Join("agents", name), managed: true, managedRoot: "agents"}, nil

	default:
		return installPaths{}, fmt.Errorf("unknown --target %q (want %s)", target, strings.Join(validTargets, ", "))
	}
}

// manifestFromInstallArchive reads agent.yaml out of the extracted archive
// when it is there, validates it, and requires it to name the resolved
// release; absent (a legacy v1 release), the resolve manifest stands.
func manifestFromInstallArchive(staging string, fromResolve *manifest.Manifest, res *apiclient.ResolveResult) (*manifest.Manifest, error) {
	if _, err := os.Stat(filepath.Join(staging, "agent.yaml")); errors.Is(err, os.ErrNotExist) {
		return fromResolve, nil
	}
	m, err := manifestFromWorkspace(staging)
	if err != nil {
		return nil, fmt.Errorf("the archive's agent.yaml: %w — refusing to install", err)
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		return nil, fmt.Errorf("the archive's agent.yaml fails validation — refusing to install: %v", blocking[0])
	}
	if m.Name != res.Name || m.Namespace != res.Namespace {
		return nil, fmt.Errorf("the archive's manifest names %s/%s but the release is %s/%s — refusing to install", m.Namespace, m.Name, res.Namespace, res.Name)
	}
	return m, nil
}

// prepareDest clears a CLI-managed destination so files an older version
// shipped cannot survive an upgrade as orphans. A --dir destination is left
// alone and merged into; the help text says so.
func prepareDest(p installPaths) error {
	if !p.managed {
		return nil
	}
	// A managed destination is one directory directly under its managed
	// root and nothing else: never a root, never the home directory or an
	// ancestor of it, never a directory a traversing name resolved to.
	// The agents-md default is relative to the working directory, so both
	// are made absolute before the judgement (security review 2026-09-20
	// §4.7).
	dest, err := filepath.Abs(p.dest)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", p.dest, err)
	}
	root := p.managedRoot
	if root != "" {
		if root, err = filepath.Abs(root); err != nil {
			return fmt.Errorf("resolve %s: %w", p.managedRoot, err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}
	if err := pathguard.RefuseDangerousDir(dest, home); err != nil {
		return fmt.Errorf("refusing to clear %s: %v", p.dest, err)
	}
	if !pathguard.DirectlyUnder(root, dest) {
		return fmt.Errorf("refusing to clear %s: a managed install lives directly under its managed root (%q)", p.dest, p.managedRoot)
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("clear %s: %w", p.dest, err)
	}
	return nil
}

// stdinIsTTY reports whether in is an interactive terminal.
func stdinIsTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	// A terminal is a tty, not any character device: /dev/null is one too,
	// and a run with stdin from it must take the non-interactive path.
	return term.IsTerminal(int(f.Fd()))
}

// warnUndeclaredTarget warns when runtime.interfaces exists but omits the
// chosen target (§5.2: interfaces lists what §5.6 may emit).
func warnUndeclaredTarget(w io.Writer, m *manifest.Manifest, target string) {
	if len(m.Runtime.Interfaces) == 0 {
		return
	}
	for _, it := range m.Runtime.Interfaces {
		if it == target {
			return
		}
	}
	_, _ = fmt.Fprintf(w, "warning: manifest runtime.interfaces %v does not list target %q\n", m.Runtime.Interfaces, target)
}

// exportClaudeSkill writes the archive into {dir/<name>|~/.claude/skills/<name>},
// guaranteeing SKILL.md frontmatter (§5.6 claude-skill row).
func exportClaudeSkill(out io.Writer, staging string, p installPaths, m *manifest.Manifest, ns, version string) error {
	dest := p.dest
	if err := prepareDest(p); err != nil {
		return err
	}
	if err := copyTree(staging, dest); err != nil {
		return err
	}

	skillRel := m.Entrypoints.Skill
	if skillRel == "" {
		skillRel = "SKILL.md"
	}
	// The entrypoint is a manifest string (validated before the download,
	// and taken from the archive's own manifest when it carries one); the
	// file it names must be inside the directory just written, whatever
	// the string.
	skillPath := filepath.Join(dest, filepath.FromSlash(skillRel))
	if !pathguard.Under(dest, skillPath) {
		return fmt.Errorf("skill entrypoint %q resolves outside the install directory — refusing to write it", skillRel)
	}
	content, err := os.ReadFile(skillPath) //nolint:gosec // under dest, checked just above
	if err != nil {
		return fmt.Errorf("read skill entrypoint %s: %w", skillRel, err)
	}
	// Embedded mode (RUNNER-PLAN §R5): the host coding agent supplies the
	// tools, so the capability contract compiles to its allowed-tools
	// vocabulary here — the same contract the standalone runner enforces
	// through the gateway.
	withFM := export.SkillFrontmatterWithTools(content, m.Name, m.Summary, export.HostToolGrants(m.Capabilities))
	if err := os.WriteFile(skillPath, withFM, 0o644); err != nil { //nolint:gosec // skill files are world-readable content
		return fmt.Errorf("write skill entrypoint: %w", err)
	}

	_, _ = fmt.Fprintf(out, "installed %s/%s@%s as Claude skill: %s\n", ns, m.Name, version, dest)
	return nil
}

// exportAgentsMD extracts to {dir|./agents/<name>} and upserts the agent's
// section in ./AGENTS.md (§5.6 agents-md row).
func exportAgentsMD(out io.Writer, staging string, p installPaths, m *manifest.Manifest, ns, version string) error {
	dest := p.dest
	if err := prepareDest(p); err != nil {
		return err
	}
	if err := copyTree(staging, dest); err != nil {
		return err
	}

	const agentsPath = "AGENTS.md"
	existing, err := os.ReadFile(agentsPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", agentsPath, err)
	}
	// Embedded mode (RUNNER-PLAN §R5): the section states which host
	// built-ins the declared contract corresponds to.
	updated := export.UpsertAgentsMDSectionNote(existing, ns, m.Name, version, m.Summary, dest,
		export.HostToolsNote(m.Capabilities))
	if err := os.WriteFile(agentsPath, updated, 0o644); err != nil { //nolint:gosec // project doc, not a secret
		return fmt.Errorf("write %s: %w", agentsPath, err)
	}

	_, _ = fmt.Fprintf(out, "installed %s/%s@%s as prompt agent: %s (section updated in %s)\n", ns, m.Name, version, dest, agentsPath)
	return nil
}

// copyTree copies the extracted archive into dest, preserving each file's
// permission bits (the archive normalizes modes, D-15).
func copyTree(src, dest string) error {
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
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
		data, rerr := os.ReadFile(p) //nolint:gosec // reading our own staging dir
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		return fmt.Errorf("copy files to %s: %w", dest, err)
	}
	return nil
}

// treeFiles lists the slash-separated relative file paths under root, for
// the exporter's arg re-rooting.
func treeFiles(root string) map[string]bool {
	files := map[string]bool{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		files[filepath.ToSlash(rel)] = true
		return nil
	})
	return files
}
