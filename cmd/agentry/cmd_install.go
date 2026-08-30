// `agentry install` (§11): resolve → download → verify digest AND the DSSE
// attestation signature (§8.8) → show the declared capability contract,
// diffed against whatever this machine last installed → extract → export to
// one §5.6 target (claude-skill, mcp, or agents-md).
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/export"
	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// installOpts carries the install command's flags.
type installOpts struct {
	target        string
	dir           string
	yes           bool
	allowUnsigned bool
}

// newInstallCmd builds the install command.
func newInstallCmd() *cobra.Command {
	var opts installOpts
	cmd := &cobra.Command{
		Use:   "install <ns/name[@range]>",
		Short: "Resolve, download, verify, and export an agent to a runtime format",
		Long: "Resolve ns/name[@range] (§9.4), download the release archive, verify its\n" +
			"sha256 digest AND its DSSE attestation signature (§8.8), show the declared\n" +
			"capability contract (§5.4) diffed against the installed version, and export\n" +
			"to one install target (§5.6):\n\n" +
			"  claude-skill  {--dir | ~/.claude/skills}/<name>/ with SKILL.md frontmatter\n" +
			"  mcp           extract to {--dir | ./agents/<name>} and merge .mcp.json\n" +
			"  agents-md     extract to {--dir | ./agents/<name>} and update ./AGENTS.md\n\n" +
			"Signatures: a release the registry has attested must verify against the\n" +
			"signing keys pinned for that registry, or the install aborts. A registry\n" +
			"that publishes no attestation yields an unsigned install, which requires\n" +
			"--allow-unsigned (in CI as well as at a terminal).\n\n" +
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
	cmd.Flags().StringVar(&opts.target, "target", "", "install target: claude-skill | mcp | agents-md (default: by agent kind)")
	cmd.Flags().StringVar(&opts.dir, "dir", "", "override the target directory (see help for per-target defaults)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "skip the capability confirmation prompt (CI)")
	cmd.Flags().BoolVar(&opts.allowUnsigned, "allow-unsigned", false,
		"install a release the registry has published no attestation for")
	return cmd
}

// validTargets is the §5.6 closed set.
var validTargets = []string{manifest.InterfaceClaudeSkill, manifest.InterfaceMCP, manifest.InterfaceAgentsMD}

// validateTarget checks an explicitly requested target. It runs BEFORE any
// network call: a typo in --target should cost a usage message, not a
// download, a capability prompt, and an extraction that is then thrown away.
func validateTarget(target string) error {
	if target == "" {
		return nil // resolved from the agent's kind once the manifest arrives
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
	if err := verifyInstallAttestation(cmd, reg, token, res, computed, opts.allowUnsigned); err != nil {
		return err
	}

	target := opts.target
	if target == "" {
		target = defaultTarget(m.Kind)
	}
	warnUndeclaredTarget(errOut, m, target)
	paths, err := resolveInstallPaths(target, opts.dir, m.Name)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s@%s (%s)\n", res.Namespace, res.Name, res.Version, m.Kind)
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
	if err := confirmCapabilityChange(cmd, &state, key, res.Version, nextCaps, m.Capabilities, opts.yes); err != nil {
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

	switch target {
	case manifest.InterfaceClaudeSkill:
		err = exportClaudeSkill(out, staging, paths, m, res.Namespace, res.Version)
	case manifest.InterfaceMCP:
		err = exportMCP(out, staging, paths, m, res.Namespace, res.Version)
	case manifest.InterfaceAgentsMD:
		err = exportAgentsMD(out, staging, paths, m, res.Namespace, res.Version)
	default:
		// Unreachable: validateTarget and defaultTarget between them admit
		// only the three above. Kept so a fourth interface cannot be added
		// without this switch failing loudly.
		return fmt.Errorf("unknown --target %q (want %s)", target, strings.Join(validTargets, ", "))
	}
	if err != nil {
		return err
	}

	// Record only after the files actually landed: a manifest claiming an
	// install that failed would suppress the next run's capability prompt.
	state.Upsert(key, res.Version, nextCaps)
	if err := installed.Save(state); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %v — the next install of this agent will re-ask about capabilities\n", err)
	}
	return nil
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
func verifyInstallAttestation(cmd *cobra.Command, reg, token string, res *apiclient.ResolveResult, archiveDigest string, allowUnsigned bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

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
	att := trust.VerifyAttestation(cmd.Context(), httpClient, reg, token, attURL, archiveDigest)

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
		_, _ = fmt.Fprintf(errOut,
			"warning: unsigned — registry offers no attestation for %s/%s@%s; installing because --allow-unsigned was given\n",
			res.Namespace, res.Name, res.Version)
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
func confirmCapabilityChange(cmd *cobra.Command, state *installed.State, key installed.Key,
	version string, next installed.CapSnapshot, caps manifest.Capabilities, yes bool,
) error {
	out := cmd.OutOrStdout()
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
		"this update widens the agent's declared capabilities; re-run with --yes to accept in non-interactive mode",
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
		"this agent declares network egress and/or exec — type \"yes\" to continue: ",
		"this agent declares network egress and/or exec; re-run with --yes to accept in non-interactive mode",
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
	// mcpConfig is the .mcp.json to merge into (mcp target only).
	mcpConfig string
	// extractedRel re-roots MCP args at the extraction path when the config
	// lives above it (default layout only); empty when config and files are
	// siblings.
	extractedRel string
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
		return installPaths{dest: filepath.Join(home, ".claude", "skills", name), managed: true}, nil

	case manifest.InterfaceMCP:
		if dir != "" {
			return installPaths{dest: dir, mcpConfig: filepath.Join(dir, ".mcp.json")}, nil
		}
		return installPaths{
			dest: filepath.Join("agents", name), managed: true,
			mcpConfig: ".mcp.json", extractedRel: path.Join("agents", name),
		}, nil

	case manifest.InterfaceAgentsMD:
		if dir != "" {
			return installPaths{dest: dir}, nil
		}
		return installPaths{dest: filepath.Join("agents", name), managed: true}, nil

	default:
		return installPaths{}, fmt.Errorf("unknown --target %q (want %s)", target, strings.Join(validTargets, ", "))
	}
}

// prepareDest clears a CLI-managed destination so files an older version
// shipped cannot survive an upgrade as orphans. A --dir destination is left
// alone and merged into; the help text says so.
func prepareDest(p installPaths) error {
	if !p.managed {
		return nil
	}
	if err := os.RemoveAll(p.dest); err != nil {
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
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
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
	skillPath := filepath.Join(dest, filepath.FromSlash(skillRel))
	content, err := os.ReadFile(skillPath) //nolint:gosec // path derives from the verified archive
	if err != nil {
		return fmt.Errorf("read skill entrypoint %s: %w", skillRel, err)
	}
	withFM := export.EnsureSkillFrontmatter(content, m.Name, m.Summary)
	if err := os.WriteFile(skillPath, withFM, 0o644); err != nil { //nolint:gosec // skill files are world-readable content
		return fmt.Errorf("write skill entrypoint: %w", err)
	}

	_, _ = fmt.Fprintf(out, "installed %s/%s@%s as Claude skill: %s\n", ns, m.Name, version, dest)
	return nil
}

// exportMCP extracts to {dir|./agents/<name>} and merges a server entry into
// the client config: {dir}/.mcp.json when --dir is given, else ./.mcp.json
// with args re-rooted at the extraction path (§5.6 mcp row).
func exportMCP(out io.Writer, staging string, p installPaths, m *manifest.Manifest, ns, version string) error {
	dest, configPath, extractedRel := p.dest, p.mcpConfig, p.extractedRel
	if err := prepareDest(p); err != nil {
		return err
	}
	if err := copyTree(staging, dest); err != nil {
		return err
	}

	entry, err := export.MCPEntryFromManifest(m, extractedRel, treeFiles(staging))
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(configPath) //nolint:gosec // fixed config name under a user-chosen dir
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	merged, err := export.MergeMCPConfig(existing, m.Name, entry)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, merged, 0o644); err != nil { //nolint:gosec // client config, not a secret
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	_, _ = fmt.Fprintf(out, "installed %s/%s@%s as MCP server: %s\n", ns, m.Name, version, dest)
	_, _ = fmt.Fprintf(out, "merged server entry %q into %s", m.Name, configPath)
	if len(m.Capabilities.Secrets) > 0 {
		_, _ = fmt.Fprintf(out, " — fill in env: %s", strings.Join(m.Capabilities.Secrets, ", "))
	}
	_, _ = fmt.Fprintln(out)
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
	updated := export.UpsertAgentsMDSection(existing, ns, m.Name, version, m.Summary, dest)
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
