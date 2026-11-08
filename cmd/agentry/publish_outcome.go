package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// What a publish ended in, said so a person knows what happened, why and
// where to look, and so a script can branch on it: the exit code tells held
// and rejected from published, and --json prints the same as data.

// Exit codes of `agentry publish` beyond 0 and the CLI's own: the same
// numbers `agentry review` gives the verdicts it predicts.
const (
	exitPublishHeld     = 20
	exitPublishRejected = 21
)

// publishResult is `agentry publish --json`.
type publishResult struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	// Outcome is published, held (waiting for a human reviewer), rejected,
	// ended (quarantined, blocked or yanked), no-record (the registry has no
	// QuickAgent of that name this credential can publish to), or
	// not-newer (refused before upload: the version is not newer than the
	// registry's).
	Outcome string `json:"outcome"`
	// Status is the version's state at the registry, when it has one.
	Status string `json:"status,omitempty"`
	// Reason is the registry's reason, or what refused the publish.
	Reason        string `json:"reason,omitempty"`
	ArchiveDigest string `json:"archiveDigest,omitempty"`
	// Unscanned says the registry admitted the version without a scan.
	Unscanned bool `json:"unscanned,omitempty"`
	// ReportURL is where the version's report is read.
	ReportURL string `json:"reportUrl,omitempty"`
	// Findings are the report's findings, for a rejected version.
	Findings []publishFinding `json:"findings"`
	// CreateURL is where the QuickAgent's record is created, for no-record.
	CreateURL string `json:"createUrl,omitempty"`
}

// publishFinding is one finding of a rejected version's report.
type publishFinding struct {
	Rule     string `json:"rule,omitempty"`
	Scanner  string `json:"scanner"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// reportURL is where a person reads a version's report: the version's page
// on a SecureAgentry site, else the registry's report route.
func reportURL(reg, ns, name, version string) string {
	base := strings.TrimRight(reg, "/")
	path := "/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version)
	if isSecureAgentry(reg) {
		return base + path
	}
	return base + "/api/v1/agents" + path + "/report"
}

// createURL is where the QuickAgent's record is made: the site's New
// QuickAgent page on SecureAgentry, else the registry's create route.
func createURL(reg string) string {
	base := strings.TrimRight(reg, "/")
	if isSecureAgentry(reg) {
		return base + "/new"
	}
	return "POST " + base + "/api/v1/agents"
}

// versionIsNew refuses, before anything is uploaded, a version the registry
// would refuse under V3: one already used, or not above every version on its
// channel. It reads the registry's versions and applies the same rule; a
// registry it cannot ask is left to judge for itself.
func versionIsNew(ctx context.Context, client *apiclient.ClientWithResponses, m *manifest.Manifest, tag string) error {
	resp, err := client.ListVersionsWithResponse(ctx, m.Namespace, m.Name)
	if err != nil || resp.JSON200 == nil {
		return nil
	}
	existing := map[manifest.Channel][]string{}
	for _, v := range *resp.JSON200 {
		ch := manifest.Channel(v.Channel)
		existing[ch] = append(existing[ch], v.Version)
	}
	issues := manifest.ValidateInContext(m, manifest.Context{
		Namespace: m.Namespace, Name: m.Name, TagVersion: tag, ExistingVersions: existing,
	})
	for _, e := range issues {
		if is, ok := manifest.AsIssue(e); ok && is.Rule == "V3" && is.Severity == manifest.SeverityError && is.Path == "version" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("%s/%s %s: %s; raise version in agent.yaml (nothing was uploaded)", m.Namespace, m.Name, m.Version, is.Message))
		}
	}
	return nil
}

// reportFindings reads a version's findings from its report, nil when the
// report cannot be read.
func reportFindings(ctx context.Context, client *apiclient.ClientWithResponses, ns, name, version string) []publishFinding {
	resp, err := client.GetVersionReportWithResponse(ctx, ns, name, version)
	if err != nil || resp.JSON200 == nil {
		return nil
	}
	out := make([]publishFinding, 0, len(resp.JSON200.Findings))
	for _, f := range resp.JSON200.Findings {
		pf := publishFinding{Scanner: f.Scanner, Severity: string(f.Severity), Title: f.Title}
		if f.FilePath != nil {
			pf.File = *f.FilePath
		}
		if f.LineStart != nil {
			pf.Line = *f.LineStart
		}
		if f.Detail != nil {
			for _, k := range []string{"ruleId", "rule"} {
				if s, ok := (*f.Detail)[k].(string); ok && s != "" {
					pf.Rule = s
					break
				}
			}
		}
		out = append(out, pf)
	}
	return out
}

// finishPublish says what the publish ended in, prints the --json result,
// and returns the error whose class is the exit code.
func finishPublish(ctx context.Context, client *apiclient.ClientWithResponses, out, stdout io.Writer, reg, ns, name string, final *apiclient.AgentVersion, err error) error {
	if final == nil {
		return err
	}
	r := publishResult{Namespace: ns, Name: name, Version: final.Version, Status: string(final.Status), Findings: []publishFinding{}}
	if final.StatusReason != nil {
		r.Reason = *final.StatusReason
	}
	if final.ArchiveDigest != nil {
		r.ArchiveDigest = *final.ArchiveDigest
	}
	r.Unscanned = final.ScanStatus != nil && *final.ScanStatus == apiclient.AgentVersionScanStatusSkipped
	r.ReportURL = reportURL(reg, ns, name, final.Version)
	switch final.Status {
	case "published":
		r.Outcome = "published"
		_, _ = fmt.Fprintf(out, "report: %s\n", r.ReportURL)
	case "needs_review":
		r.Outcome = "held"
		_, _ = fmt.Fprintf(out, "report: %s\n", r.ReportURL)
		err = engine.Classify(exitPublishHeld, &reportedError{err: fmt.Errorf("version %s is held for human review", final.Version)})
	case "rejected":
		r.Outcome = "rejected"
		r.Findings = reportFindings(ctx, client, ns, name, final.Version)
		if len(r.Findings) > 0 {
			_, _ = fmt.Fprintf(out, "findings (%d):\n", len(r.Findings))
			for _, f := range r.Findings {
				printPublishFinding(out, f)
			}
		}
		_, _ = fmt.Fprintf(out, "report: %s\n", r.ReportURL)
		if err == nil {
			err = fmt.Errorf("version %s rejected", final.Version)
		}
		err = engine.Classify(exitPublishRejected, err)
	default:
		r.Outcome = "ended"
	}
	if flags.jsonOut {
		if jerr := emitJSON(stdout, r); jerr != nil {
			return jerr
		}
	}
	return err
}

// printPublishFinding prints one finding on a line: severity, rule (or the
// scanner), where, and what.
func printPublishFinding(w io.Writer, f publishFinding) {
	id := f.Rule
	if id == "" {
		id = f.Scanner
	}
	loc := f.File
	if loc != "" && f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc != "" {
		loc = " " + loc
	}
	_, _ = fmt.Fprintf(w, "  %-8s %s%s — %s\n", strings.ToUpper(f.Severity), id, loc, f.Title)
}

// noRecord is the answer to a submit the registry answered 404: there is no
// QuickAgent of that name this credential can publish to.
func noRecord(stdout io.Writer, reg, ns, name, version string, problem error) error {
	create := createURL(reg)
	if flags.jsonOut {
		r := publishResult{Namespace: ns, Name: name, Version: version, Outcome: "no-record", Reason: problem.Error(), CreateURL: create, Findings: []publishFinding{}}
		if err := emitJSON(stdout, r); err != nil {
			return err
		}
	}
	how := "create it at " + create
	if strings.HasPrefix(create, "POST ") {
		how = "create it with " + create + " (its namespace and name)"
	}
	return fmt.Errorf("%w\nThe registry has no QuickAgent %s/%s this credential can publish to: %s, then publish again. A token publishing to a private QuickAgent also needs the agent:read scope", problem, ns, name, how)
}

// statusNotFound says the submit was answered 404.
func statusNotFound(code int) bool { return code == http.StatusNotFound }
