package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// designOf is a built-in's document with its name and accent changed, as a
// person's own design would be.
func designOf(t *testing.T, base, name, accent string) []byte {
	t.Helper()
	d, ok := reportdesign.Builtin(base)
	if !ok {
		t.Fatalf("no built-in %s", base)
	}
	d.Name, d.Accent = name, accent
	return d.Canonical()
}

// TestChooseReportTheme (agentry-cli#731, plan D21, D22; agentry-cli#784,
// report-designs.md D12, D19, D20): a flag, then the account, then the
// config, then Plain; a name or a file typed in a flag or the config that is
// not a design refuses as a usage error; the account's design, unread or not
// one this CLI accepts, is a warning and the next choice runs.
func TestChooseReportTheme(t *testing.T) {
	dir := t.TempDir()
	house := designOf(t, "editorial", "House", "#7a1f1f")
	houseFile := filepath.Join(dir, "house.json")
	mustWriteFile(t, houseFile, house)
	mine := designOf(t, "modern", "Mine", "#0b5394")
	account := func(doc []byte, err error) accountTheme {
		return func(context.Context) ([]byte, error) { return doc, err }
	}
	conf := func(name string) cliConfig { return cliConfig{Report: reportConfig{Theme: name}} }
	for _, c := range []struct {
		name    string
		flags   reportFlags
		cfg     cliConfig
		account accountTheme
		want    engine.ReportTheme
		warns   string
	}{
		{"nothing set", reportFlags{}, cliConfig{}, nil, engine.ReportTheme{Name: "plain", From: engine.ThemeFromDefault}, ""},
		{"the theme flag wins", reportFlags{theme: "classic"}, conf("modern"), account(mine, nil), engine.ReportTheme{Name: "classic", From: engine.ThemeFromFlag}, ""},
		{"the design flag wins", reportFlags{design: houseFile}, conf("modern"), account(mine, nil), engine.ReportTheme{Design: house, From: engine.ThemeFromDesignFlag}, ""},
		{"the account over the config", reportFlags{}, conf("modern"), account(mine, nil), engine.ReportTheme{Design: mine, From: engine.ThemeFromAccount}, ""},
		{"never chosen is not plain", reportFlags{}, conf("technical"), account(nil, nil), engine.ReportTheme{Name: "technical", From: engine.ThemeFromConfig}, ""},
		{"a design file in the config", reportFlags{}, cliConfig{Report: reportConfig{Design: houseFile}}, nil, engine.ReportTheme{Design: house, From: engine.ThemeFromDesignConfig}, ""},
		{"an account that cannot be read", reportFlags{}, conf("classic"), account(nil, errors.New("offline")), engine.ReportTheme{Name: "classic", From: engine.ThemeFromConfig}, "could not be read (offline); PDFs use classic"},
		{"a design this CLI does not accept", reportFlags{}, cliConfig{Report: reportConfig{Design: houseFile}}, account(bytes.Replace(mine, []byte(`"agentryTheme": 2`), []byte(`"agentryTheme": 3`), 1), nil),
			engine.ReportTheme{Design: house, From: engine.ThemeFromDesignConfig}, `is not one this CLI can draw (report design: agentryTheme: must be 2`},
	} {
		var warn bytes.Buffer
		got, err := chooseReportTheme(context.Background(), c.flags, c.cfg, c.account, &warn)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Name != c.want.Name || !bytes.Equal(got.Design, c.want.Design) || got.From != c.want.From {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if c.warns == "" && warn.Len() > 0 || !strings.Contains(warn.String(), c.warns) {
			t.Errorf("%s: warned %q, want %q", c.name, warn.String(), c.warns)
		}
	}
	pale := filepath.Join(dir, "pale.json")
	mustWriteFile(t, pale, designOf(t, "modern", "Pale", "#7fb3d5"))
	asked := false
	spy := func(context.Context) ([]byte, error) { asked = true; return mine, nil }
	for _, c := range []struct {
		name  string
		flags reportFlags
		cfg   cliConfig
		says  string
	}{
		{"an unknown theme flag", reportFlags{theme: "fancy"}, cliConfig{}, "plain, classic, modern, editorial, technical"},
		{"a capitalised theme flag", reportFlags{theme: "Classic"}, cliConfig{}, "plain, classic, modern, editorial, technical"},
		{"an old theme's name", reportFlags{theme: "formal"}, cliConfig{}, "plain, classic, modern, editorial, technical"},
		{"both flags", reportFlags{theme: "plain", design: houseFile}, cliConfig{}, "give one"},
		{"a missing design file", reportFlags{design: filepath.Join(dir, "none.json")}, cliConfig{}, "no such file"},
		{"a design that fails the contrast rule", reportFlags{design: pale}, cliConfig{}, "accent"},
		{"an unknown config theme", reportFlags{}, conf("house"), "plain, classic, modern, editorial, technical"},
		{"both in the config", reportFlags{}, cliConfig{Report: reportConfig{Theme: "plain", Design: houseFile}}, "keep one"},
		{"a config design that is not one", reportFlags{}, cliConfig{Report: reportConfig{Design: pale}}, "accent"},
	} {
		_, err := chooseReportTheme(context.Background(), c.flags, c.cfg, spy, &bytes.Buffer{})
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a usage error saying %q", c.name, err, c.says)
		}
	}
	if asked {
		t.Error("the account was asked although a typed choice refused the run")
	}
}

func mustWriteFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadAccountTheme: the signed-in account's report design from the
// registry, with the sign-in's token; {}, a null design, a core's answer
// from before designs and a registry from before the setting are never
// chosen; any other answer is an error. No account is
// asked without a sign-in, nor in a driver's run, which holds a run grant.
func TestReadAccountTheme(t *testing.T) {
	isolateEnv(t)
	mine := designOf(t, "modern", "Mine", "#0b5394")
	answer, status := `{"design":`+string(mine)+`}`, http.StatusOK
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/report-settings" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		if r.URL.Query().Get("include") != "logo" {
			t.Errorf("a run read the account's design without include=logo: %s", r.URL)
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Access denied","status":403}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	if readAccountTheme(srv.URL) != nil {
		t.Fatal("an account was asked with no sign-in")
	}
	t.Setenv(envToken, "agy_test")
	read := readAccountTheme(srv.URL)
	if read == nil {
		t.Fatal("a signed-in run asks no account")
	}
	if doc, err := read(context.Background()); err != nil || !bytes.Equal(bytes.TrimSpace(doc), bytes.TrimSpace(mine)) || auth != "Bearer agy_test" {
		t.Errorf("a design = (%s, %v), auth %q", doc, err, auth)
	}
	for _, never := range []string{`{}`, `{"design":null}`, `{"theme":"formal"}`} {
		answer = never
		if doc, err := read(context.Background()); err != nil || doc != nil {
			t.Errorf("never chosen %s = (%s, %v)", never, doc, err)
		}
	}
	status = http.StatusForbidden
	if _, err := read(context.Background()); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("a refusal = %v", err)
	}
	old := readAccountTheme(srv.URL + "/older")
	if doc, err := old(context.Background()); err != nil || doc != nil {
		t.Errorf("a registry without the setting = (%s, %v)", doc, err)
	}
	t.Setenv(envRunGrant, "grant")
	if readAccountTheme(srv.URL) != nil {
		t.Error("a driver's run asked the account")
	}
}

// An unknown report theme, or a design file that is not one, stops the run
// before anything starts: a usage
// error, not the docker error a run on this machine would meet next.
func TestRunRefusesAnUnknownReportTheme(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--allow-unsigned", "--yes", "--policy", policyPath, "--report-theme", "fancy")
	wantClassified(t, err, engine.ExitUsage)
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "house"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--allow-unsigned", "--yes", "--policy", policyPath)
	wantClassified(t, err, engine.ExitUsage)
	if err := saveCLIConfig(cliConfig{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--allow-unsigned", "--yes", "--policy", policyPath, "--report-design", filepath.Join(t.TempDir(), "none.json"))
	wantClassified(t, err, engine.ExitUsage)
}

// report.theme and report.design survive the CLI rewriting its config, as a
// sign-in does.
func TestReportThemeSurvivesAConfigRewrite(t *testing.T) {
	isolateConfig(t)
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "classic", Design: "house.json"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.upsertRegistry(registryEntry{URL: "https://registry.example", TokenRef: tokenRefFile, Token: "agy_x"})
	if err := saveCLIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadCLIConfig(); err != nil || cfg.Report.Theme != "classic" || cfg.Report.Design != "house.json" {
		t.Errorf("report after a rewrite = %+v (%v)", cfg.Report, err)
	}
}

// A design file with a logo near the largest one a design may carry is
// read whole by --report-design (agentry-cli#816): the read is bounded by
// the logo's size, not the 16 KB of the design alone.
func TestReportDesignFileWithALargeLogo(t *testing.T) {
	isolateEnv(t)
	img := image.NewNRGBA(image.Rect(0, 0, 330, 330))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 400<<10 || buf.Len() > reportdesign.MaxLogoBytes {
		t.Fatalf("the test logo is %d bytes, want a large one within the bound", buf.Len())
	}
	doc := withLogo(t, designOf(t, "modern", "Large", "#0b5394"), buf.Bytes())
	f := filepath.Join(t.TempDir(), "large.json")
	mustWriteFile(t, f, doc)
	choice, err := chooseReportTheme(t.Context(), reportFlags{design: f}, cliConfig{}, nil, io.Discard)
	if err != nil || !bytes.Equal(choice.Design, doc) {
		t.Fatalf("--report-design with a %d-byte logo: %v", buf.Len(), err)
	}
}
