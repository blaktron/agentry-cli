package main

// Choosing a run's report design: how export_pdf lays a run's PDFs out is the
// person's choice, never the model's. The first that is set wins: the
// --report-design NAME|ID|FILE or --report-theme NAME flag, for one run; the
// signed-in account's default design, from its library at the registry
// (the core's /user/report-designs); report.design (a file) or report.theme
// (a built-in theme) in this CLI's config; Plain. What the person typed
// here, in a flag or the config, refuses the run before anything starts
// when it is not a design: a name no theme has, a file that does not hold
// one, a name or id their library does not have. The account's default is
// read, never trusted to be one this CLI can draw: one it does not accept,
// or an account it cannot reach, is a warning, and the run takes the next
// choice, since how a PDF looks is no reason to stop a run.

import (
	"bytes"
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

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// accountThemeTimeout bounds the read of the account's report design.
const accountThemeTimeout = 10 * time.Second

// maxDesignBytes is the most a design document is read as: the document
// with every image and the custom cover at their largest, with room for
// its key. pkg/reportdesign's Parse holds the document to its own exact
// bounds.
var maxDesignBytes = reportdesign.MaxDocumentWithAssets + 1024

// accountDesigns is the signed-in account's report design library, as far
// as a run reads it.
type accountDesigns interface {
	// Default is the account's default design, with its images, or nil
	// when the account has none.
	Default(ctx context.Context) ([]byte, error)
	// Named is the design of the library a name or an id names, with its
	// images; an error when the library has no such design, or two of
	// that name.
	Named(ctx context.Context, ref string) ([]byte, error)
}

// reportFlags are the run's two report flags.
type reportFlags struct {
	// theme is --report-theme, a built-in theme's id.
	theme string
	// design is --report-design: a file holding a design document, or
	// the name or id of a design of the account's library.
	design string
}

// designIsFile says whether --report-design names a file rather than a
// design of the library: it has a path separator or the .json extension,
// or a file of that name exists.
func designIsFile(value string) bool {
	if strings.ContainsAny(value, `/\`) || strings.EqualFold(filepath.Ext(value), ".json") {
		return true
	}
	fi, err := os.Stat(value)
	return err == nil && !fi.IsDir()
}

// chooseReportTheme is the run's report design and where it came from.
// account is nil when there is no account to ask.
func chooseReportTheme(ctx context.Context, flags reportFlags, cfg cliConfig, account accountDesigns, warn io.Writer) (engine.ReportTheme, error) {
	themes := strings.Join(toolsrv.BuiltinThemes(), ", ")
	usage := func(format string, a ...any) (engine.ReportTheme, error) {
		return engine.ReportTheme{}, engine.Classify(engine.ExitUsage, fmt.Errorf(format, a...))
	}
	switch {
	case flags.theme != "" && flags.design != "":
		return usage("--report-theme and --report-design both choose the run's report design; give one")
	case flags.design != "" && designIsFile(flags.design):
		doc, err := readDesignFile(flags.design)
		if err != nil {
			return usage("--report-design %s: %v", flags.design, err)
		}
		return engine.ReportTheme{Design: doc, From: engine.ThemeFromDesignFlag}, nil
	case flags.design != "":
		if account == nil {
			return usage("--report-design %q names a design of your account's library, and you are not signed in (a file is read when it exists; a built-in theme is --report-theme)", flags.design)
		}
		ctx, cancel := context.WithTimeout(ctx, accountThemeTimeout)
		defer cancel()
		doc, err := account.Named(ctx, flags.design)
		if err != nil {
			return usage("--report-design %q: %v", flags.design, err)
		}
		if _, err := reportdesign.Parse(doc); err != nil {
			return usage("--report-design %q: your account's design is not one this CLI can draw (%v); update agentry", flags.design, err)
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
	doc, err := account.Default(ctx)
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
// file of any size is refused by Parse's own limit. A document of an
// earlier version is refused by its version first, in so many words.
func readDesign(r io.Reader) (reportdesign.Design, []byte, error) {
	doc, err := io.ReadAll(io.LimitReader(r, int64(maxDesignBytes)+1))
	if err != nil {
		return reportdesign.Design{}, nil, err
	}
	if err := checkDesignVersion(doc); err != nil {
		return reportdesign.Design{}, nil, err
	}
	d, err := reportdesign.Parse(doc)
	return d, doc, err
}

// checkDesignVersion refuses a theme document of another version than the
// one this CLI draws, naming both.
func checkDesignVersion(doc []byte) error {
	var head struct {
		AgentryTheme *int `json:"agentryTheme"`
	}
	if err := json.Unmarshal(doc, &head); err != nil || head.AgentryTheme == nil || *head.AgentryTheme == reportdesign.DocumentVersion {
		return nil
	}
	return fmt.Errorf("a theme document version %d; this CLI draws version %d (export the design again from your account, whose library holds every design in version %d)", *head.AgentryTheme, reportdesign.DocumentVersion, reportdesign.DocumentVersion)
}

// readAccountTheme is the account's report design library at the registry
// the CLI speaks to, or nil when no account is signed in there, or when the
// run is a driver's (a hosted run's host holds a run grant, not the
// person's sign-in, and names the design with a flag).
func readAccountTheme(regURL string) accountDesigns {
	if os.Getenv(envRunGrant) != "" {
		return nil
	}
	// A refused environment token reads as no account here; the command's
	// own registry call reports the refusal.
	token, err := credentialFor(regURL)
	if err != nil || token == "" {
		return nil
	}
	return &designLibrary{reg: regURL, token: token}
}

// designLibrary is the account's report design library at the registry:
// the core's /user/report-designs routes, spoken to with the sign-in's
// token. A plain client, not the generated one: a local run is not refused
// over a design, so for a run the minimum-version gate stays the run's own
// resolve's (the report-design commands gate before they ask).
type designLibrary struct {
	reg, token string
}

// librarySummary is one design of the library as the list names it.
type librarySummary struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	BasedOn        string    `json:"basedOn"`
	Version        int       `json:"version"`
	Default        bool      `json:"default"`
	HasLogo        bool      `json:"hasLogo"`
	HasCoverImage  bool      `json:"hasCoverImage"`
	HasCustomCover bool      `json:"hasCustomCover"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// libraryList is the library: its designs, oldest first, and the default's
// id, empty when none is.
type libraryList struct {
	Default string           `json:"default,omitempty"`
	Designs []librarySummary `json:"designs"`
}

// libraryRecord is one design with its document.
type libraryRecord struct {
	ID             string          `json:"id"`
	Default        bool            `json:"default"`
	HasLogo        bool            `json:"hasLogo"`
	HasCoverImage  bool            `json:"hasCoverImage"`
	HasCustomCover bool            `json:"hasCustomCover"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Design         json.RawMessage `json:"design"`
}

// errNoLibrary is a registry from before the library: nothing is chosen
// there.
var errNoLibrary = errors.New("this registry keeps no report design library")

// call sends one request and answers the body of a 2xx, or the problem.
func (l *designLibrary) call(ctx context.Context, method, path string, body any, op string) (int, []byte, error) {
	var in io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.reg, "/")+"/api/v1/user/report-designs"+path, in)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	// A design with its images and the answer's own wrapping, never much
	// more; a list of ten summaries is far smaller.
	out, err := io.ReadAll(io.LimitReader(res.Body, int64(maxDesignBytes)+(8<<10)))
	if err != nil {
		return 0, nil, err
	}
	if res.StatusCode/100 == 2 {
		return res.StatusCode, out, nil
	}
	return res.StatusCode, out, apiProblem(op, res.StatusCode, nil, out)
}

// list is the library.
func (l *designLibrary) list(ctx context.Context) (libraryList, error) {
	status, body, err := l.call(ctx, http.MethodGet, "", nil, "read your report designs")
	if status == http.StatusNotFound {
		return libraryList{}, errNoLibrary
	}
	if err != nil {
		return libraryList{}, err
	}
	var out libraryList
	if err := json.Unmarshal(body, &out); err != nil {
		return libraryList{}, fmt.Errorf("read your report designs: %w", err)
	}
	return out, nil
}

// get is one design by id, with its images when assets is set.
func (l *designLibrary) get(ctx context.Context, id string, assets bool) (libraryRecord, error) {
	path := "/" + id
	if assets {
		path += "?include=assets"
	}
	_, body, err := l.call(ctx, http.MethodGet, path, nil, "read your report design")
	if err != nil {
		return libraryRecord{}, err
	}
	var out libraryRecord
	if err := json.Unmarshal(body, &out); err != nil {
		return libraryRecord{}, fmt.Errorf("read your report design: %w", err)
	}
	return out, nil
}

// create adds a design, with its images, and makes it the default when
// asked; the answer carries the design as stored, images included.
func (l *designLibrary) create(ctx context.Context, doc []byte, dflt bool) (libraryRecord, error) {
	body := map[string]any{"design": json.RawMessage(doc)}
	if dflt {
		body["default"] = true
	}
	_, out, err := l.call(ctx, http.MethodPost, "?include=assets", body, "save your report design")
	if err != nil {
		return libraryRecord{}, err
	}
	var rec libraryRecord
	if err := json.Unmarshal(out, &rec); err != nil {
		return libraryRecord{}, fmt.Errorf("save your report design: %w", err)
	}
	return rec, nil
}

// update replaces a design's document; the images stay unless the
// document carries them.
func (l *designLibrary) update(ctx context.Context, id string, doc []byte) (libraryRecord, error) {
	_, out, err := l.call(ctx, http.MethodPut, "/"+id+"?include=assets", map[string]any{"design": json.RawMessage(doc)}, "save your report design")
	if err != nil {
		return libraryRecord{}, err
	}
	var rec libraryRecord
	if err := json.Unmarshal(out, &rec); err != nil {
		return libraryRecord{}, fmt.Errorf("save your report design: %w", err)
	}
	return rec, nil
}

// remove deletes a design, its images included.
func (l *designLibrary) remove(ctx context.Context, id string) error {
	_, _, err := l.call(ctx, http.MethodDelete, "/"+id, nil, "delete your report design")
	return err
}

// setDefault makes a design the default, or none when id is empty.
func (l *designLibrary) setDefault(ctx context.Context, id string) (libraryList, error) {
	body := map[string]any{}
	if id != "" {
		body["id"] = id
	}
	_, out, err := l.call(ctx, http.MethodPut, "/default", body, "set your default report design")
	if err != nil {
		return libraryList{}, err
	}
	var list libraryList
	if err := json.Unmarshal(out, &list); err != nil {
		return libraryList{}, fmt.Errorf("set your default report design: %w", err)
	}
	return list, nil
}

// find is the design a name or an id names: the id, exactly; else the one
// design of that name, letter for letter, else the one design of that
// name in any case. Two of a name is an error naming both ids.
func (l *designLibrary) find(ctx context.Context, ref string) (librarySummary, error) {
	list, err := l.list(ctx)
	if err != nil {
		return librarySummary{}, err
	}
	return findDesign(list, ref)
}

func findDesign(list libraryList, ref string) (librarySummary, error) {
	ref = strings.TrimSpace(ref)
	for _, d := range list.Designs {
		if strings.EqualFold(d.ID, ref) {
			return d, nil
		}
	}
	var exact, folded []librarySummary
	for _, d := range list.Designs {
		if d.Name == ref {
			exact = append(exact, d)
		} else if strings.EqualFold(d.Name, ref) {
			folded = append(folded, d)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = folded
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(list.Designs) == 0 {
			return librarySummary{}, errors.New("your account's library has no report design")
		}
		names := make([]string, 0, len(list.Designs))
		for _, d := range list.Designs {
			names = append(names, d.Name)
		}
		return librarySummary{}, fmt.Errorf("your account's library has no report design named %q (it holds: %s)", ref, strings.Join(names, ", "))
	default:
		ids := make([]string, 0, len(matches))
		for _, d := range matches {
			ids = append(ids, d.ID)
		}
		return librarySummary{}, fmt.Errorf("your account's library has %d report designs named %q; name one by its id (%s)", len(matches), ref, strings.Join(ids, ", "))
	}
}

// Default is the account's default design with its images, or nil when
// the account has none. A registry from before the library is none too.
func (l *designLibrary) Default(ctx context.Context) ([]byte, error) {
	list, err := l.list(ctx)
	if errors.Is(err, errNoLibrary) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if list.Default == "" {
		return nil, nil
	}
	rec, err := l.get(ctx, list.Default, true)
	if err != nil {
		return nil, err
	}
	return designOfRecord(rec)
}

// Named is the design a name or an id names, with its images.
func (l *designLibrary) Named(ctx context.Context, ref string) ([]byte, error) {
	d, err := l.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	rec, err := l.get(ctx, d.ID, true)
	if err != nil {
		return nil, err
	}
	return designOfRecord(rec)
}

// designOfRecord is the design document in a record, or an error when the
// record has none.
func designOfRecord(rec libraryRecord) ([]byte, error) {
	if len(rec.Design) == 0 || bytes.Equal(bytes.TrimSpace(rec.Design), []byte("null")) {
		return nil, errors.New("read your report design: the answer holds no design")
	}
	return rec.Design, nil
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
