// The built-in Drafter's web tools at run time. The compiled plan grants
// the tools (spec.CapSummary.WebTools, toolsrv.Web.Enabled); where a run
// scans and searches is the run's own, since one plan serves a draft made on
// this computer with the person's keys and one made in Agentry with the
// job's credential. The engine stamps that half into the run's copy of the
// tools spec, rebuilds the tools server's line of the gateway allowlist from
// it, writes the keys into the run's gateway secrets file for the tools
// container alone, and counts the tools' ledger into the receipt.
package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// WebRun is a web-tools run's configuration and keys.
type WebRun struct {
	// Scan is toolsrv.WebScanRegistry (hosted: the registry's route, with
	// JobToken) or toolsrv.WebScanJev (local: the person's JevKey).
	Scan string
	// Search is toolsrv.WebScanRegistry, a provider the person holds
	// SearchKey for, or "" (web_search is not granted).
	Search string
	// Registry is the registry's root, for registry_search and, hosted,
	// the model-input routes.
	Registry string
	// The keys. They reach the tools container alone, through the run's
	// gateway secrets file, which is deleted at teardown; the receipts are
	// masked of them. None reaches the agent container.
	JobToken, JevKey, SearchKey string
	// MaxPages, MaxSearches and MaxRegistryCalls, when set, bound the run's
	// calls below the tools' defaults.
	MaxPages, MaxSearches, MaxRegistryCalls int
	// Given are the addresses a plan whose web_read is held to them may
	// read: the person's own, from their command. Set only for such a plan.
	Given []string
}

// WebLedgerFile is the web ledger in the receipts directory.
const WebLedgerFile = "web.ndjson"

// checkWeb holds a run to its plan: a plan with the web tools runs only with
// a scanner and its key, and a plan without them takes none.
func checkWeb(p Params) error {
	has := p.Plan.Capabilities.WebTools
	switch {
	case !has && p.Web != nil:
		return Classify(ExitCLI, errors.New("this plan has no web tools, so a run cannot be given their configuration"))
	case !has:
		return nil
	case p.Web == nil:
		return Classify(ExitPolicy, errors.New("the web tools run only with AI Review reading every result first, and this run names no scanner"))
	}
	switch p.Web.Scan {
	case toolsrv.WebScanRegistry:
		if p.Web.JobToken == "" || p.Web.Registry == "" {
			return Classify(ExitPolicy, errors.New("a hosted draft's scans need the registry and the job's credential"))
		}
	case toolsrv.WebScanJev:
		if p.Web.JevKey == "" {
			return Classify(ExitPolicy, errors.New("a local draft's scans need your own Jev key"))
		}
	default:
		return Classify(ExitPolicy, fmt.Errorf("unknown scanner %q", p.Web.Scan))
	}
	if len(p.Web.Given) > 0 && !p.Plan.Capabilities.WebGivenOnly {
		return Classify(ExitCLI, errors.New("this plan's web_read is not held to the addresses it is given, so it cannot be given any"))
	}
	if p.Plan.Capabilities.WebGivenOnly && p.Web.Search != "" {
		return Classify(ExitCLI, errors.New("a plan whose web_read is held to the addresses it was given has no web_search"))
	}
	switch p.Web.Search {
	case "":
	case toolsrv.WebScanRegistry:
		if p.Web.Scan != toolsrv.WebScanRegistry {
			return Classify(ExitPolicy, errors.New("the registry's search is a hosted draft's"))
		}
	default:
		if p.Web.SearchKey == "" {
			return Classify(ExitPolicy, fmt.Errorf("a search through %s needs your own key for it", p.Web.Search))
		}
	}
	return nil
}

// webSecrets are the web keys as the gateway secrets file names them. Every
// one is written, empty when this run has none, so the gateway sets each
// variable the catalog names to what the run holds rather than to a
// placeholder of its own.
func webSecrets(p Params) map[string]string {
	if p.Web == nil {
		return nil
	}
	return map[string]string{
		compile.SecretKey(toolsrv.EnvWebJobToken):  p.Web.JobToken,
		compile.SecretKey(toolsrv.EnvWebJevKey):    p.Web.JevKey,
		compile.SecretKey(toolsrv.EnvWebSearchKey): p.Web.SearchKey,
	}
}

// grantWeb stamps the run's half of toolsrv.Web into the rendered tools spec
// and rebuilds the tools server's allowlist line from it, as grantKnowledge
// does for the packs.
func grantWeb(gatewayDir string, w *WebRun) error {
	if w == nil {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if !ts.Web.Enabled {
		return errors.New("this run's rendered tools spec grants no web tools; remove the plan directory to recompile it with this CLI")
	}
	ts.Web.Scan, ts.Web.Search, ts.Web.Registry = w.Scan, w.Search, w.Registry
	ts.Web.MaxPages, ts.Web.MaxSearches, ts.Web.MaxRegistryCalls = w.MaxPages, w.MaxSearches, w.MaxRegistryCalls
	ts.Web.Given = append([]string(nil), w.Given...)
	ts.Web.Ledger = path.Join(spec.OutMount, "receipts", WebLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(specPath, append(out, '\n'), 0o644); err != nil { //nolint:gosec // read by the tools container
		return err
	}
	return setAllowlist(filepath.Join(gatewayDir, "tools.yaml"), ts.GrantedTools())
}

// ReadWebLedger reads a run's web ledger (receipts/web.ndjson): its
// entries, and how many lines did not decode. A missing ledger is none.
func ReadWebLedger(path string) ([]toolsrv.LedgerEntry, int, error) {
	f, err := os.Open(path) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.LedgerEntry
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e toolsrv.LedgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			bad++
			continue
		}
		out = append(out, e)
	}
	return out, bad, sc.Err()
}

// webRecord counts the run's web ledger into the receipt; nil for a run
// without the web tools. A line that does not decode is counted as
// refused, never as passed.
func webRecord(receiptsDir string, w *WebRun) *receipt.Web {
	if w == nil {
		return nil
	}
	rec := &receipt.Web{Scan: w.Scan, Search: w.Search}
	entries, bad, _ := ReadWebLedger(filepath.Join(receiptsDir, WebLedgerFile))
	rec.Refused = bad
	for _, e := range entries {
		switch {
		case e.Refused != "":
			rec.Refused++
			continue
		case e.Search:
			rec.Searches++
			continue
		}
		rec.Read++
		switch e.Verdict {
		case webscan.Pass:
			rec.Passed++
		case webscan.Withheld:
			rec.Withheld++
		default:
			rec.NotScanned++
		}
		switch e.Tool {
		case toolsrv.ToolWebRead:
			rec.Pages++
		case toolsrv.ToolWebSearch:
			rec.SearchResults++
		case toolsrv.ToolRegistrySearch:
			rec.RegistryCalls++
		}
		rec.ScanRequests += e.Requests
		rec.ScanInputTokens += e.InputTokens
		if e.Model != "" {
			rec.Model, rec.QuestionSet = e.Model, e.QuestionSet
		}
	}
	return rec
}

// webLine is the receipt's enforcement line for the counts.
func webLine(rec *receipt.Web) string {
	where := "the person's own Jev key"
	if rec.Scan == toolsrv.WebScanRegistry {
		where = "the registry's model-input route, with the job's credential"
	}
	line := fmt.Sprintf("AI Review read every text the web tools brought back before the model, through %s: %d read (%d pages, %d search results of %d searches, %d registry answers), %d passed, %d withheld as flagged, %d withheld as not scanned; %d calls refused before reading anything",
		where, rec.Read, rec.Pages, rec.SearchResults, rec.Searches, rec.RegistryCalls, rec.Passed, rec.Withheld, rec.NotScanned, rec.Refused)
	if rec.Model != "" {
		line += "; judge " + rec.Model + ", question set " + rec.QuestionSet
	}
	return line + "; each address and verdict is in receipts/" + WebLedgerFile + " and gateway.log"
}

// resolvConfFile is the run's own resolv.conf for the web tools' server,
// beside its tools spec in the gateway's configuration, and for the
// classes under the run's runtime that resolve host names.
const resolvConfFile = "resolv.conf"

// runResolvers are the name servers of the run's own resolv.conf: the web
// tools' or, under a runtime, those of the llm-proxy and the relays. Both
// are the policy's docker.resolvers.
func runResolvers(plan *spec.Plan) []string {
	if rs := plan.WebResolvers(); len(rs) > 0 {
		return rs
	}
	return plan.SandboxResolvers()
}

// writeResolvConf writes, for a run with name servers in its plan (policy
// docker.resolvers), the resolv.conf read in place of docker's: the docker
// filter binds it read-only into the web tools' server and, under a
// runtime, into each relay, and the engine into the llm-proxy. A sandboxed
// runtime does not reach docker's embedded DNS. Readable by each of those
// containers' users; nothing for a run without resolvers.
func writeResolvConf(gatewayDir string, plan *spec.Plan) error {
	rs := runResolvers(plan)
	if len(rs) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(gatewayDir, resolvConfFile), policy.ResolvConf(rs), 0o644) //nolint:gosec // read by the tools server's user
}

// resolversLine is the receipt's account of how the web tools resolve.
func resolversLine(plan *spec.Plan, rs []string) string {
	who := pastHostServer(plan)
	if !filterSupported {
		return "docker.resolvers names " + strings.Join(rs, ", ") + ", which only the per-run docker filter binds into " + who + "; without it (a platform the filter does not run on) the server keeps docker's DNS"
	}
	return who + " resolves host names through " + strings.Join(rs, ", ") + " (docker.resolvers), from a resolv.conf of the run's own bound read-only in place of docker's, whose embedded DNS a sandboxed runtime does not reach"
}

// pastHostServer names, for the receipt, the tools server that reaches
// past the host (spec.CapSummary.ToolsReachPastHost).
func pastHostServer(plan *spec.Plan) string {
	if plan.Capabilities.WebTools {
		return "the web tools' server"
	}
	return "the tools server"
}
