// Package installdir is the runner home's per-release directory: where a
// verified release is laid down for runs, and since agentry-cli#190 where
// `agentry install` and the desktop put a QuickAgent by default (plan
// desktop-one-click-install.md, D3/D10). One layout, one metadata file,
// one copy of the code — a run finds an installed release by its digest
// and skips the fetch; an install is what a run's first fetch would have
// left behind, plus the export a coding agent would read.
//
//	<runner home>/agents/<ns>/<name>/            the agent's directory (installed.yaml's dir)
//	  <version>-<12 of the digest>/              one release
//	    install.json                             Meta: what was verified, once
//	    integrity.json                           the record that install.json and workspace/ are as laid down (integrity.go)
//	    workspace/                               the release's files, pristine
//	    export/                                  the coding-agent shape (writeExport)
//	    plan/  runs/                             the run's own, ephemeral
package installdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/dirpublish"
	"github.com/blaktron/agentry-cli/pkg/export"
	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/pathguard"
	"github.com/blaktron/agentry-cli/pkg/runner/integritykey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Meta is <release dir>/install.json: what was verified, once, so repeat
// runs of the same digest skip the network without skipping the truth
// about how it was admitted. The JSON shape is the one runs have read
// since the cache existed; Export is the one addition (agentry-cli#190).
type Meta struct {
	Ref         string              `json:"ref"`
	Namespace   string              `json:"namespace"`
	Name        string              `json:"name"`
	Version     string              `json:"version"`
	Digest      string              `json:"digest"`
	Source      string              `json:"source"`
	Attestation receipt.Attestation `json:"attestation"`
	// Export is the coding-agent shape written under export/ by an
	// install ("claude-skill" or "agents-md"); empty for a run's own
	// fetch, which writes none.
	Export string `json:"export,omitempty"`
}

// Target is the install target that names this directory, beside the two
// coding-agent exports in installed.yaml's target column.
const Target = "runner"

// ConsentRegistry is what the runner's record carries in installed.yaml's
// registry column: a run resolves through the configured registry and its
// record names none, so the sentinel is the target's own name — the key
// reads runner/runner. `list --check` asks the configured registry for
// such a record.
const ConsentRegistry = "runner"

// ConsentKey is the installed.yaml key the runner's record lives under —
// the run's consent record since agentry-cli#124, and since agentry-cli#190
// the install's record too, so a consent given by either covers the other
// and there is one record per QuickAgent.
func ConsentKey(home, namespace, name string) installed.Key {
	return installed.Key{
		Registry: ConsentRegistry, Namespace: namespace, Name: name,
		Target: Target, Dir: AgentDir(home, namespace, name),
	}
}

// AgentsRoot is the directory every agent's directory sits directly under.
func AgentsRoot(home string) string { return filepath.Join(home, "agents") }

// AgentDir is the agent's directory under the runner home, stable across
// versions: the one installed.yaml records, and the one an uninstall
// removes.
func AgentDir(home, namespace, name string) string {
	return filepath.Join(AgentsRoot(home), namespace, name)
}

// ReleaseDir is one release's directory under AgentDir.
func ReleaseDir(home, namespace, name, version, digest string) string {
	return filepath.Join(AgentDir(home, namespace, name), ReleaseName(version, digest))
}

// ReleaseName is a release directory's name, <version>-<12 of the digest>:
// one spelling for every client, so a run finds what an install laid down.
func ReleaseName(version, digest string) string {
	return fmt.Sprintf("%s-%s", version, DigestSuffix(digest))
}

// DigestSuffix shortens sha256:… for directory names.
func DigestSuffix(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// Write lands workspace/ and install.json under dir, sealed with the
// integrity record (integrity.go). A half-written install must not look
// installed, and two first installs of one release at once (a CLI run and
// a desktop run on a fresh runner home) must not share a half-built tree
// (agentry-cli#294): the tree is built aside and published by rename
// (dirpublish). dir is named for the release's digest, so a whole install
// of that digest already there that its record vouches for, whoever laid
// it down, is the same release: it is kept, with any run's plan inside
// it, and this copy discarded. An install that changed after it was laid
// down is refused (*ChangedError), never replaced: that is Repair's, on the
// person's word. Anything else at dir, an install without a record
// included, is replaced. A local tree's .git and agentry-out are not part
// of the release and are skipped.
func Write(dir, srcTree string, meta *Meta) error {
	return layDown(dir, srcTree, meta, keepAny, nil, false)
}

// Replace is Write for a tree whose source is on this machine and is the
// truth (a --local directory, a built-in package): an installed copy that
// changed is replaced rather than refused, since there is no signed release
// for it to be faithful to, only the source it is copied from.
func Replace(dir, srcTree string, meta *Meta) error {
	return layDown(dir, srcTree, meta, keepAny, nil, true)
}

func keepAny(*Meta) bool { return true }

// layDown builds the release tree for dir aside, lets addToTree add to it
// (Install's export), seals it, and publishes the whole, or leaves dir as
// it was. A whole install of the same digest at dir, which its record
// vouches for and keep accepts, wins over this copy; a nil keep never
// keeps one (Repair replaces whatever is there). An install at dir that
// changed after it was laid down is refused unless replaceChanged.
func layDown(dir, srcTree string, meta *Meta, keep func(got *Meta) bool, addToTree func(tree string) error, replaceChanged bool) error {
	home, err := HomeOf(dir)
	if err != nil {
		return err
	}
	key, err := integritykey.Load(home)
	if err != nil {
		return err
	}
	if !replaceChanged {
		var changed *ChangedError
		if _, err := Inspect(dir, meta.Digest); errors.As(err, &changed) {
			return changed
		}
	}
	var settled func() bool
	if keep != nil {
		settled = func() bool {
			got, err := Inspect(dir, meta.Digest)
			return err == nil && keep(got)
		}
	}
	_, err = dirpublish.ReplaceUnless(dir, func(tree string) error {
		if err := export.CopyTreeSkipping(srcTree, filepath.Join(tree, "workspace"), notPartOfARelease); err != nil {
			return err
		}
		if err := writeMeta(tree, meta); err != nil {
			return err
		}
		if addToTree != nil {
			if err := addToTree(tree); err != nil {
				return err
			}
		}
		// The staging directory sits beside the release, where anyone who
		// can write the agent's directory could add to it before it is
		// sealed: the copy is held to the verified source first, so what is
		// sealed is the release and nothing else.
		if err := sameFiles(srcTree, filepath.Join(tree, "workspace")); err != nil {
			return err
		}
		// Last: the record covers install.json as the export left it.
		return seal(key, tree)
	}, settled)
	if err != nil {
		return fmt.Errorf("lay down the release: %w", err)
	}
	return nil
}

func writeMeta(dir string, meta *Meta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.json"), append(b, '\n'), 0o644); err != nil { //nolint:gosec // metadata, not secret
		return fmt.Errorf("write install metadata: %w", err)
	}
	return nil
}

// Load reuses a verified install iff its recorded digest matches what the
// registry (or the tree hash) says now, its workspace is there, and its
// integrity record vouches for the tree (Inspect). Anything else is not
// an install to reuse: a caller that lays the release down again meets a
// changed install as Write's refusal. A caller that must tell the cases
// apart (refuse a changed install before fetching anything) uses Inspect.
func Load(dir, wantDigest string) (*Meta, bool) {
	meta, err := Inspect(dir, wantDigest)
	if err != nil {
		return nil, false
	}
	return meta, true
}

// StyleFor is the export shape an install writes beside the release (plan
// desktop-one-click-install.md, D2): the coding agent the policy names
// (claude before codex when both are), else the provider — Anthropic's
// keys drive Claude Code, the rest are OpenAI-compatible or generic — else
// AGENTS.md, the shape any coding agent reads.
func StyleFor(pol *policy.Policy) string {
	if pol == nil {
		return manifest.InterfaceAgentsMD
	}
	if _, ok := pol.Harness["claude"]; ok {
		return manifest.InterfaceClaudeSkill
	}
	if _, ok := pol.Harness["codex"]; ok {
		return manifest.InterfaceAgentsMD
	}
	if strings.EqualFold(strings.TrimSpace(pol.Model.Provider), policy.ProviderAnthropic) {
		return manifest.InterfaceClaudeSkill
	}
	return manifest.InterfaceAgentsMD
}

// StyleForHome is StyleFor applied to the runner home's policy, with the
// one honest outcome for each case: no policy yet is the generic shape and
// nothing to say (an install must not create one, that is setup's and a
// run's); a policy that cannot be read is the generic shape too, and the
// warning says so, so an install goes on rather than stopping on a file it
// does not need. Both clients show the warning the same way.
func StyleForHome(home string) (style, warning string) {
	path := policy.PathIn(home)
	pol, _, err := policy.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StyleFor(nil), ""
		}
		return StyleFor(nil), fmt.Sprintf("operator policy %s could not be read (%v) — the export beside the install takes the generic AGENTS.md shape", path, err)
	}
	return StyleFor(pol), ""
}

// Install is the one lay-down both clients call for the runner target: the
// verified release written as Write does, with the coding-agent shape in
// the given style (writeExport) inside it, published as one tree. It is called only once the digest, the
// attestation and the consent have been judged — it checks none of them.
// It refuses a Meta that carries a consent: the cache holds the verdict
// and never the consent, which lives in installed.yaml, so a run cannot
// read one out of install.json.
func Install(releaseDir, srcTree string, m *manifest.Manifest, meta *Meta, style string) error {
	if err := refuseConsent(meta); err != nil {
		return err
	}
	// The export is written into the staged tree, so the published
	// release is whole with its export, and a refused style leaves
	// whatever was installed before untouched. The same release already
	// installed with this export is kept as it is.
	keepIfSameExport := func(got *Meta) bool { return got.Export == style }
	return layDown(releaseDir, srcTree, meta, keepIfSameExport, func(tree string) error {
		return writeExport(tree, style, m, meta)
	}, false)
}

// Repair is Install on the person's word that an installed release which
// changed after it was laid down is to be replaced (`agentry install
// --repair`): the release, fetched and verified again by the caller, is
// laid down in place of whatever is at releaseDir, whether or not it
// changed, and sealed afresh. style is the export to write beside it, or
// empty for none (a release a run laid down, which has none).
func Repair(releaseDir, srcTree string, m *manifest.Manifest, meta *Meta, style string) error {
	if err := refuseConsent(meta); err != nil {
		return err
	}
	var addExport func(tree string) error
	if style != "" {
		addExport = func(tree string) error { return writeExport(tree, style, m, meta) }
	}
	return layDown(releaseDir, srcTree, meta, nil, addExport, true)
}

// refuseConsent refuses a Meta that carries a consent: install.json holds
// the verdict and never the consent, which lives in installed.yaml, so a
// run cannot read one out of it.
func refuseConsent(meta *Meta) error {
	if meta.Attestation.Consent != "" {
		return fmt.Errorf("install.json holds the attestation verdict, never a consent (got %q)", meta.Attestation.Consent)
	}
	return nil
}

// writeExport lays the coding-agent shape under <dir>/export from the
// verified workspace — the Claude skill as ~/.claude/skills would hold it
// (export/<name>/SKILL.md with the compiled allowed-tools), or AGENTS.md
// with the agent's section as a project root would (export/AGENTS.md and
// export/agents/<name>/) — and records the style in install.json. The host
// adapters can read it where it is; the container harness ignores it. It
// writes into a tree being laid down, before the tree is sealed: on a
// published release its rewrite of install.json would read as a change
// (agentry-cli#326), which is why it is not exported.
func writeExport(dir, style string, m *manifest.Manifest, meta *Meta) error {
	root := filepath.Join(dir, "export")
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clear export dir: %w", err)
	}
	workspace := filepath.Join(dir, "workspace")
	switch style {
	case manifest.InterfaceClaudeSkill:
		if err := export.WriteClaudeSkillTree(workspace, filepath.Join(root, m.Name), m); err != nil {
			return err
		}
	case manifest.InterfaceAgentsMD:
		dest := filepath.Join(root, "agents", m.Name)
		if err := export.WriteAgentsMDTree(workspace, dest, filepath.Join(root, "AGENTS.md"), m, meta.Namespace, meta.Version); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown export style %q (want %s or %s)", style, manifest.InterfaceClaudeSkill, manifest.InterfaceAgentsMD)
	}
	meta.Export = style
	return writeMeta(dir, meta)
}

// notPartOfARelease names the directories a local tree carries that a
// release does not: its version control, and the runner's own output.
func notPartOfARelease(name string) bool { return name == ".git" || name == "agentry-out" }

// ErrNotARelease is RemoveOtherVersions' refusal of a keep that is not a
// release directly under the agent's directory: a path the caller built
// wrong, never an older version that would not go, so a caller that says
// a failed replace as a warning still stops on it.
var ErrNotARelease = errors.New("not a release of this agent")

// RemoveOtherVersions removes every entry of agentDir but keep — the
// replace rule of an update (D9): one release per QuickAgent, the older
// one's directory gone once the new one is verified and recorded, while a
// staging or retired sibling another process is still using is left to
// it. Runs keep their receipts in the person's output folder, not here.
// keep must sit directly under agentDir, or nothing is removed: the
// caller's slip must not turn into a sweep of some other directory.
func RemoveOtherVersions(agentDir, keep string) error {
	if !pathguard.DirectlyUnder(agentDir, keep) {
		return fmt.Errorf("%w: refusing to clear %s: %s is not a release directly under it", ErrNotARelease, agentDir, keep)
	}
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", agentDir, err)
	}
	keepName := filepath.Base(keep)
	for _, e := range entries {
		// Another process laying a release down right now owns its
		// staging sibling; removing it could gut the tree it is about to
		// publish (agentry-cli#294). An abandoned one goes.
		if e.Name() == keepName || dirpublish.InFlight(e) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(agentDir, e.Name())); err != nil {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return nil
}
