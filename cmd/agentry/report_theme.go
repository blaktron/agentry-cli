package main

// Choosing a run's report design: how export_pdf lays a run's PDFs out is the
// person's choice, never the model's. The first that is set wins: the
// --report-design FILE or --report-theme NAME flag, for one run; the
// signed-in account's report design (the core's GET /user/report-settings);
// report.design (a file) or report.theme (a built-in theme)
// in this CLI's config; Plain. What the person typed here, in a flag or the
// config, refuses the run before anything starts when it is not a design: a
// name no theme has, a file that does not hold one. The account's design is
// read, never trusted to be one this CLI can draw: one it does not accept,
// or an account it cannot reach, is a warning, and the run takes the next
// choice, since how a PDF looks is no reason to stop a run.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// accountThemeTimeout bounds the read of the account's report design.
const accountThemeTimeout = 10 * time.Second

// maxDesignBytes is the most a design document is read as: the 16 KB of
// the design and, beside it, the base64 of the largest logo with room for
// its key. pkg/reportdesign's Parse holds the document to its own exact
// bounds.
var maxDesignBytes = reportdesign.MaxDocumentBytes + base64.StdEncoding.EncodedLen(reportdesign.MaxLogoBytes) + 1024

// accountTheme reads the signed-in account's report design: its document,
// or nil when the account never chose one.
type accountTheme func(ctx context.Context) ([]byte, error)

// reportFlags are the run's two report flags.
type reportFlags struct {
	// theme is --report-theme, a built-in theme's id.
	theme string
	// design is --report-design, a file holding a design document.
	design string
}

// chooseReportTheme is the run's report design and where it came from.
// account is nil when there is no account to ask.
func chooseReportTheme(ctx context.Context, flags reportFlags, cfg cliConfig, account accountTheme, warn io.Writer) (engine.ReportTheme, error) {
	themes := strings.Join(toolsrv.BuiltinThemes(), ", ")
	usage := func(format string, a ...any) (engine.ReportTheme, error) {
		return engine.ReportTheme{}, engine.Classify(engine.ExitUsage, fmt.Errorf(format, a...))
	}
	switch {
	case flags.theme != "" && flags.design != "":
		return usage("--report-theme and --report-design both choose the run's report design; give one")
	case flags.design != "":
		doc, err := readDesignFile(flags.design)
		if err != nil {
			return usage("--report-design %s: %v", flags.design, err)
		}
		return engine.ReportTheme{Design: doc, From: engine.ThemeFromDesignFlag}, nil
	case flags.theme != "":
		if !toolsrv.IsBuiltinTheme(flags.theme) {
			return usage("--report-theme %q is not a report theme (they are: %s)", flags.theme, themes)
		}
		return engine.ReportTheme{Name: flags.theme, From: engine.ThemeFromFlag}, nil
	}
	fallback := engine.ReportTheme{Name: toolsrv.DefaultTheme, From: engine.ThemeFromDefault}
	name, file := strings.TrimSpace(cfg.Report.Theme), strings.TrimSpace(cfg.Report.Design)
	path, pathErr := configPath()
	switch {
	case name != "" && file != "":
		return usage("report.theme and report.design in %s both choose the report design; keep one", path)
	case file != "":
		if !filepath.IsAbs(file) {
			// Relative to the config directory, never the working one.
			if pathErr != nil {
				return usage("report.design %q is relative, and the config directory is not known: %v", file, pathErr)
			}
			file = filepath.Join(filepath.Dir(path), file)
		}
		doc, err := readDesignFile(file)
		if err != nil {
			return usage("report.design in %s: %s: %v", path, file, err)
		}
		fallback = engine.ReportTheme{Design: doc, From: engine.ThemeFromDesignConfig}
	case name != "":
		if !toolsrv.IsBuiltinTheme(name) {
			return usage("report.theme %q in %s is not a report theme (they are: %s)", name, path, themes)
		}
		fallback = engine.ReportTheme{Name: name, From: engine.ThemeFromConfig}
	}
	if account == nil {
		return fallback, nil
	}
	ctx, cancel := context.WithTimeout(ctx, accountThemeTimeout)
	defer cancel()
	doc, err := account(ctx)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(warn, "agentry: warning: your account's report design could not be read (%v); PDFs use %s (%s)\n", err, describeTheme(fallback), fallback.From)
	case doc == nil:
	default:
		if _, err := reportdesign.Parse(doc); err != nil {
			_, _ = fmt.Fprintf(warn, "agentry: warning: your account's report design is not one this CLI can draw (%v; update agentry); PDFs use %s (%s)\n", err, describeTheme(fallback), fallback.From)
			break
		}
		return engine.ReportTheme{Design: doc, From: engine.ThemeFromAccount}, nil
	}
	return fallback, nil
}

// readDesignFile reads a design document from a file and holds it to
// pkg/reportdesign's rules, the core's own.
func readDesignFile(file string) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // a file the person named
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	_, doc, err := readDesign(f)
	return doc, err
}

// readDesign reads at most one document past the cap and parses it, so a
// file of any size is refused by Parse's own limit.
func readDesign(r io.Reader) (reportdesign.Design, []byte, error) {
	doc, err := io.ReadAll(io.LimitReader(r, int64(maxDesignBytes)+1))
	if err != nil {
		return reportdesign.Design{}, nil, err
	}
	d, err := reportdesign.Parse(doc)
	return d, doc, err
}

// readAccountTheme is the account's report design at the registry the CLI
// speaks to, or nil when no account is signed in there, or when the run is
// a driver's (a hosted run's host holds a run grant, not the person's
// sign-in, and names the design with a flag).
func readAccountTheme(regURL string) accountTheme {
	if os.Getenv(envRunGrant) != "" {
		return nil
	}
	// A refused environment token reads as no account here; the command's
	// own registry call reports the refusal.
	token, err := credentialFor(regURL)
	if err != nil || token == "" {
		return nil
	}
	return func(ctx context.Context) ([]byte, error) {
		return getReportDesign(ctx, regURL, token)
	}
}

// getReportDesign is the account's report design document as the registry
// answers it, with the person's logo when there is one, or nil when the
// account never chose one.
func getReportDesign(ctx context.Context, regURL, token string) ([]byte, error) {
	// A plain request, not the generated client: the route is the core's,
	// and a local run is not refused over a design, so for a run the
	// minimum-version gate stays the run's own resolve's (report-design
	// export gates before it asks). include=logo asks for the logo, which
	// the core answers only when asked so that a client built before logos
	// still reads the design; an older core ignores the parameter.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(regURL, "/")+"/api/v1/user/report-settings?include=logo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	// The design and the answer's own wrapping, never much more.
	body, err := io.ReadAll(io.LimitReader(res.Body, int64(maxDesignBytes)+(4<<10)))
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		return designOfSettings(body)
	case http.StatusNotFound:
		// A registry from before the setting: nothing was chosen there.
		return nil, nil
	}
	return nil, apiProblem("read your report design", res.StatusCode, nil, body)
}

// designOfSettings is the design in a report-settings answer, or nil for
// never chosen.
func designOfSettings(body []byte) ([]byte, error) {
	var settings struct {
		Design json.RawMessage `json:"design"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return nil, fmt.Errorf("read your report design: %w", err)
	}
	// {} is never chosen; nor is an answer from an older core, which names
	// a theme and no design.
	if len(settings.Design) == 0 || bytes.Equal(bytes.TrimSpace(settings.Design), []byte("null")) {
		return nil, nil
	}
	return settings.Design, nil
}

// describeTheme names a choice in a warning: a built-in theme's id, or the
// name a design document gives itself.
func describeTheme(t engine.ReportTheme) string {
	if t.Design != nil {
		if d, err := reportdesign.Parse(t.Design); err == nil {
			return fmt.Sprintf("the design %q", d.Name)
		}
	}
	return t.Name
}
