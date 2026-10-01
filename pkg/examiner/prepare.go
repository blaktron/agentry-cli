package examiner

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

	"github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/validate"
)

// The pre-step (plan §6.1, kind 1): what the registry runs that a client
// has the code for, run here over the files a submission would carry, and
// what the registry publishes about its gate and about each declared
// server. Then the inputs are staged for the run: the manifest's canonical
// JSON, since the runner image has no YAML parser, and the package's files,
// both copied, so the run reads a copy and the person's folder is never
// mounted.

// The staged inputs' names, as the run sees them under /inputs.
const (
	InputManifest = "manifest.json"
	InputPackage  = "package"
)

// textScanCap is the most of one text file the registry's stages read
// (stage S1's cap); a longer file is reported and sent to review.
const textScanCap = 1 << 20

// policyTimeout bounds the registry's answer for its baseline policy.
const policyTimeout = 15 * time.Second

// Options are how Prepare reaches the registry and loads its rules.
type Options struct {
	// Registry is the registry's base URL; empty asks it nothing, and the
	// report says so.
	Registry string
	// HTTP is the client for the registry; nil is a client with a timeout.
	HTTP *http.Client
	// Rules loads the S4 rule bundle (validate.Options.Rules); nil is the
	// bundle embedded in this binary.
	Rules validate.RulesLoader
	// Log takes the narration; nil discards it.
	Log io.Writer
}

// Prepared is the pre-step's result: kind 1 of the report, the policy the
// outcome is decided under, and the staged inputs.
type Prepared struct {
	// Dir is the QuickAgent's folder; Ref is what was examined, its
	// ns/name@version as the manifest declares it.
	Dir, Ref string
	// ManifestJSON is the manifest's canonical JSON, as the registry reads
	// it.
	ManifestJSON []byte
	// Validation is `agentry validate --json`'s report over the folder.
	Validation validate.Report
	// Findings are kind 1 as the gate weighs them: the inventory, the
	// hidden-content and injection rules, a file past the scan cap or not
	// read, and each declared server's advisories.
	Findings []Finding
	// Servers are the registry's records of the declared servers.
	Servers []mcpinstall.VulnStatus
	Policy  PolicyUsed
	// Stage holds the staged inputs; Inputs are the paths a run is handed.
	// Cleanup removes them.
	Stage  string
	Inputs []string
	// Files is how many files were staged.
	Files int
}

// PolicyUsed is the publication policy the outcome is decided under, and
// where it came from.
type PolicyUsed struct {
	gatecases.Policy
	// Source is "registry" (its public baseline, GET /platform/policy) or
	// "compiled" (the baseline compiled into the core this CLI was built
	// with, when the registry did not answer).
	Source string `json:"source"`
	// Error says why the registry's was not used.
	Error string `json:"error,omitempty"`
}

// Cleanup removes the staged inputs.
func (p *Prepared) Cleanup() {
	if p != nil && p.Stage != "" {
		_ = os.RemoveAll(p.Stage)
	}
}

// Prepare runs the pre-step over the QuickAgent in dir. An error is a
// review that cannot start (no manifest, a rule bundle that will not load,
// a folder that cannot be copied), never a finding.
func Prepare(ctx context.Context, dir string, o Options) (*Prepared, error) {
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: policyTimeout}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m, err := validate.ReadManifest(abs)
	if err != nil {
		return nil, err
	}
	report, err := validate.Workspace(abs, validate.Options{Rules: o.Rules, Out: logw, ErrOut: logw})
	if err != nil {
		return nil, err
	}
	canonical, err := manifest.CanonicalJSON(m)
	if err != nil {
		return nil, fmt.Errorf("the manifest's canonical JSON: %w", err)
	}
	p := &Prepared{Dir: abs, Ref: fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version), ManifestJSON: canonical, Validation: report}
	p.Findings = ruleFindings(report)

	if p.Stage, err = os.MkdirTemp("", "agentry-examiner-*"); err != nil {
		return nil, err
	}
	truncated, n, err := stagePackage(abs, m, filepath.Join(p.Stage, InputPackage))
	if err == nil {
		err = os.WriteFile(filepath.Join(p.Stage, InputManifest), canonical, 0o644) //nolint:gosec // staged input, read-only in the run
	}
	if err != nil {
		p.Cleanup()
		return nil, fmt.Errorf("stage the QuickAgent's files: %w", err)
	}
	p.Files = n
	p.Inputs = []string{filepath.Join(p.Stage, InputManifest), filepath.Join(p.Stage, InputPackage)}
	p.Findings = append(p.Findings, truncated...)

	if servers := m.Capabilities.MCPServers; len(servers) > 0 && o.Registry != "" {
		p.Servers = mcpinstall.CheckVulnerabilities(ctx, client, o.Registry, servers)
		p.Findings = append(p.Findings, serverFindings(p.Servers)...)
	}
	p.Policy = fetchPolicy(ctx, client, o.Registry)
	return p, nil
}

// ruleFindings are validate's inventory and rule findings as the core's
// stages rate them, and its unread files as S1's unreadable-file finding.
// The manifest's own rule errors are not findings: Submit refuses them
// before the gate (Expected).
func ruleFindings(r validate.Report) []Finding {
	var out []Finding
	for _, f := range r.Inventory {
		out = append(out, Finding{Kind: KindRegistryRules, Scanner: f.Scanner, Severity: f.Severity, Confidence: inventoryConfidence[f.Scanner],
			Title: f.Title, File: f.FilePath, Detail: f.Detail})
	}
	for _, f := range r.Findings {
		out = append(out, Finding{Kind: KindRegistryRules, Scanner: f.RuleID, Severity: f.Severity, Confidence: f.Confidence,
			Title: f.Title, File: f.FilePath, Line: f.LineStart, Excerpt: f.Excerpt})
	}
	for _, e := range r.ReadErrors {
		out = append(out, Finding{Kind: KindRegistryRules, Scanner: "S1/unreadable-file", Severity: "medium", Confidence: "high",
			Title: "File in package could not be read for scanning", Detail: e})
	}
	return out
}

// inventoryConfidence is the confidence stage S1 gives each inventory
// finding (validate's subset carries severities alone).
var inventoryConfidence = map[string]string{
	"S1/binary-executable": "high",
	"S1/nested-archive":    "high",
	"S1/non-utf8":          "medium",
	"S1/opaque-binary":     "low",
}

// serverFindings are each declared server's advisories as stage S7 rates
// them: the advisory's own severity, at high confidence. A server the
// registry has no answer for here gives none; the report says it was not
// determined (Finish).
func serverFindings(servers []mcpinstall.VulnStatus) []Finding {
	var out []Finding
	for _, s := range servers {
		if s.Status != "vulnerable" && s.Status != "error" {
			continue
		}
		for _, a := range s.Advisories {
			title := "Known vulnerability in " + s.Ref + ": " + a.ID
			if len(a.Aliases) > 0 {
				title += " (" + strings.Join(a.Aliases, ", ") + ")"
			}
			out = append(out, Finding{Kind: KindRegistryRules, Scanner: "S7/mcp-vuln-" + a.ID, Severity: a.Severity, Confidence: "high",
				Title: title, Detail: a.Summary})
		}
	}
	return out
}

// stagePackage copies the files a submission would carry (the manifest's
// publish.include over the folder, links left out as the archive leaves
// them out) into dest, and reports each text file past the scan cap as
// stage S1 does.
func stagePackage(dir string, m *manifest.Manifest, dest string) ([]Finding, int, error) {
	files, err := archive.WalkWorkspace(dir)
	if err != nil {
		return nil, 0, err
	}
	var findings []Finding
	n := 0
	for _, fi := range manifest.ResolveInclude(m.Publish.Include, files) {
		if fi.Symlink {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(fi.Path))) //nolint:gosec // a path the walk returned
		if err != nil {
			continue // validate reported it as unreadable
		}
		target := filepath.Join(dest, filepath.FromSlash(fi.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, 0, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil { //nolint:gosec // staged input, read-only in the run
			return nil, 0, err
		}
		n++
		if len(data) > textScanCap && isText(fi.Path, data) {
			findings = append(findings, Finding{Kind: KindRegistryRules, Scanner: "S1/text-scan-truncated", Severity: "medium", Confidence: "high",
				Title: "Text file exceeds the scan cap — content past 1 MiB was not scanned", File: fi.Path})
		}
	}
	if n == 0 {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, 0, err
		}
	}
	return findings, n, nil
}

// isText is stage S1's classification of a text file.
func isText(path string, data []byte) bool {
	return !archive.IsExecutable(data) && !archive.IsArchiveData(data) && !archive.IsImagePath(path) && archive.LooksText(archive.Head(data))
}

// fetchPolicy is the registry's public baseline policy, or the compiled one
// with why.
func fetchPolicy(ctx context.Context, client *http.Client, registry string) PolicyUsed {
	fallback := func(why string) PolicyUsed {
		t, err := gatecases.Load()
		if err != nil {
			return PolicyUsed{Source: "compiled", Error: why + "; " + err.Error()}
		}
		return PolicyUsed{Policy: t.Baseline, Source: "compiled", Error: why}
	}
	if registry == "" {
		return fallback("no registry was asked")
	}
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(registry, "/")+"/api/v1/platform/policy", nil)
	if err != nil {
		return fallback(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fallback("the registry did not answer: " + err.Error())
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fallback(fmt.Sprintf("the registry answered %d", res.StatusCode))
	}
	var pol gatecases.Policy
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&pol); err != nil {
		return fallback("the registry's policy did not decode: " + err.Error())
	}
	if err := checkPolicy(pol); err != nil {
		return fallback("the registry's policy: " + err.Error())
	}
	return PolicyUsed{Policy: pol, Source: "registry"}
}

// checkPolicy refuses a policy whose threshold or wildcard rule is not a
// value the gate knows, rather than reading it as the fallback.
func checkPolicy(p gatecases.Policy) error {
	if _, ok := severityRanks[p.MaxAutoPublishSeverity]; p.MaxAutoPublishSeverity != "" && !ok {
		return fmt.Errorf("maxAutoPublishSeverity %q is not a severity", p.MaxAutoPublishSeverity)
	}
	switch p.WildcardEgress {
	case "", "review", "reject":
	default:
		return errors.New("wildcardEgress is " + p.WildcardEgress)
	}
	return nil
}
