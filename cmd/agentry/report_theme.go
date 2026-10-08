package main

// Choosing a run's report theme (plan scratch-and-pdf-reports.md D18, D21,
// D22; agentry-cli#731): how export_pdf lays a run's PDFs out is the
// person's choice, never the model's. The first that is set wins: the
// --report-theme flag, for one run; the signed-in account's report theme
// (the core's GET /user/report-settings, agentry#989); report.theme in this
// CLI's config; Plain. A name the person typed here, in the flag or the
// config, refuses the run before anything starts when it is not a theme's.
// The account's theme is read, never trusted to be one this CLI has: a name
// it does not know, or an account it cannot reach, is a warning, and the
// run takes the next choice, since how a PDF looks is no reason to stop a
// run.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// accountThemeTimeout bounds the read of the account's report theme.
const accountThemeTimeout = 10 * time.Second

// accountTheme reads the signed-in account's report theme: a name, or ""
// when the account never chose one.
type accountTheme func(ctx context.Context) (string, error)

// chooseReportTheme is the run's report theme and where it came from.
// account is nil when there is no account to ask.
func chooseReportTheme(ctx context.Context, flag string, cfg cliConfig, account accountTheme, warn io.Writer) (engine.ReportTheme, error) {
	themes := strings.Join(toolsrv.BuiltinThemes(), ", ")
	if flag != "" {
		if !toolsrv.IsBuiltinTheme(flag) {
			return engine.ReportTheme{}, engine.Classify(engine.ExitUsage, fmt.Errorf("--report-theme %q is not a report theme (they are: %s)", flag, themes))
		}
		return engine.ReportTheme{Name: flag, From: engine.ThemeFromFlag}, nil
	}
	fallback := engine.ReportTheme{Name: toolsrv.DefaultTheme, From: engine.ThemeFromDefault}
	if name := strings.TrimSpace(cfg.Report.Theme); name != "" {
		if !toolsrv.IsBuiltinTheme(name) {
			path, _ := configPath()
			return engine.ReportTheme{}, engine.Classify(engine.ExitUsage, fmt.Errorf("report.theme %q in %s is not a report theme (they are: %s)", name, path, themes))
		}
		fallback = engine.ReportTheme{Name: name, From: engine.ThemeFromConfig}
	}
	if account == nil {
		return fallback, nil
	}
	ctx, cancel := context.WithTimeout(ctx, accountThemeTimeout)
	defer cancel()
	switch name, err := account(ctx); {
	case err != nil:
		_, _ = fmt.Fprintf(warn, "agentry: warning: your account's report theme could not be read (%v); PDFs use %s (%s)\n", err, fallback.Name, fallback.From)
	case name == "":
	case !toolsrv.IsBuiltinTheme(name):
		_, _ = fmt.Fprintf(warn, "agentry: warning: your account's report theme %q is not one this CLI has (update agentry); PDFs use %s (%s)\n", name, fallback.Name, fallback.From)
	default:
		return engine.ReportTheme{Name: name, From: engine.ThemeFromAccount}, nil
	}
	return fallback, nil
}

// readAccountTheme is the account's report theme at the registry the CLI
// speaks to, or nil when no account is signed in there, or when the run is
// a driver's (a hosted run's host holds a run grant, not the person's
// sign-in, and names the theme with the flag).
func readAccountTheme(regURL string) accountTheme {
	if os.Getenv(envRunGrant) != "" {
		return nil
	}
	token := credentialFor(regURL)
	if token == "" {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		// A plain request, not the generated client: the route is the core's
		// since agentry#989, and moving the core dependency to get its client
		// would bring unrelated changes with it. The minimum-version gate is
		// the run's own resolve's; a local run is not refused over a theme.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(regURL, "/")+"/api/v1/user/report-settings", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		res, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if err != nil {
			return "", err
		}
		switch res.StatusCode {
		case http.StatusOK:
			var settings struct {
				Theme *string `json:"theme"`
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				return "", fmt.Errorf("read your report theme: %w", err)
			}
			if settings.Theme == nil {
				return "", nil
			}
			return *settings.Theme, nil
		case http.StatusNotFound:
			// A registry from before the setting: nothing was chosen there.
			return "", nil
		}
		return "", apiProblem("read your report theme", res.StatusCode, nil, body)
	}
}
