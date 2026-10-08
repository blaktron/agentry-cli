package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// TestChooseReportTheme (agentry-cli#731, plan D21, D22): the flag, then
// the account, then the config, then Plain; a name typed in the flag or the
// config that is not a theme's refuses as a usage error; the account's
// theme, unread or unknown here, is a warning and the next choice runs.
func TestChooseReportTheme(t *testing.T) {
	account := func(name string, err error) accountTheme {
		return func(context.Context) (string, error) { return name, err }
	}
	conf := func(name string) cliConfig { return cliConfig{Report: reportConfig{Theme: name}} }
	for _, c := range []struct {
		name    string
		flag    string
		cfg     cliConfig
		account accountTheme
		want    engine.ReportTheme
		warns   string
	}{
		{"nothing set", "", cliConfig{}, nil, engine.ReportTheme{Name: "plain", From: themeFromDefault}, ""},
		{"the flag wins", "letter", conf("brief"), account("formal", nil), engine.ReportTheme{Name: "letter", From: themeFromFlag}, ""},
		{"the account over the config", "", conf("brief"), account("formal", nil), engine.ReportTheme{Name: "formal", From: themeFromAccount}, ""},
		{"never chosen is not plain", "", conf("brief"), account("", nil), engine.ReportTheme{Name: "brief", From: themeFromConfig}, ""},
		{"no account", "", conf("technical"), nil, engine.ReportTheme{Name: "technical", From: themeFromConfig}, ""},
		{"an account that cannot be read", "", conf("brief"), account("", errors.New("offline")), engine.ReportTheme{Name: "brief", From: themeFromConfig}, "could not be read (offline); PDFs use brief"},
		{"a theme this CLI does not have", "", cliConfig{}, account("house-style", nil), engine.ReportTheme{Name: "plain", From: themeFromDefault}, `"house-style" is not one this CLI has`},
	} {
		var warn bytes.Buffer
		got, err := chooseReportTheme(context.Background(), c.flag, c.cfg, c.account, &warn)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if c.warns == "" && warn.Len() > 0 || !strings.Contains(warn.String(), c.warns) {
			t.Errorf("%s: warned %q, want %q", c.name, warn.String(), c.warns)
		}
	}
	asked := false
	spy := func(context.Context) (string, error) { asked = true; return "formal", nil }
	for _, c := range []struct {
		name string
		flag string
		cfg  cliConfig
	}{
		{"an unknown flag", "fancy", cliConfig{}},
		{"a capitalised flag", "Formal", cliConfig{}},
		{"an unknown config", "", conf("house")},
	} {
		_, err := chooseReportTheme(context.Background(), c.flag, c.cfg, spy, &bytes.Buffer{})
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "plain, formal, brief, technical, letter") {
			t.Errorf("%s: %v, want a usage error naming the themes", c.name, err)
		}
	}
	if asked {
		t.Error("the account was asked although a typed name refused the run")
	}
}

// TestReadAccountTheme: the signed-in account's report theme from the
// registry, with the sign-in's token; {} and a registry from before the
// setting are never chosen; any other answer is an error. No account is
// asked without a sign-in, nor in a driver's run, which holds a run grant.
func TestReadAccountTheme(t *testing.T) {
	isolateEnv(t)
	answer, status := `{"theme":"formal"}`, http.StatusOK
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/report-settings" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
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
	if name, err := read(context.Background()); err != nil || name != "formal" || auth != "Bearer agy_test" {
		t.Errorf("formal = (%q, %v), auth %q", name, err, auth)
	}
	answer = `{}`
	if name, err := read(context.Background()); err != nil || name != "" {
		t.Errorf("never chosen = (%q, %v)", name, err)
	}
	status = http.StatusForbidden
	if _, err := read(context.Background()); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("a refusal = %v", err)
	}
	old := readAccountTheme(srv.URL + "/older")
	if name, err := old(context.Background()); err != nil || name != "" {
		t.Errorf("a registry without the setting = (%q, %v)", name, err)
	}
	t.Setenv(envRunGrant, "grant")
	if readAccountTheme(srv.URL) != nil {
		t.Error("a driver's run asked the account")
	}
}

// An unknown report theme stops the run before anything starts: a usage
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
}

// report.theme survives the CLI rewriting its config, as a sign-in does.
func TestReportThemeSurvivesAConfigRewrite(t *testing.T) {
	isolateConfig(t)
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "letter"}}); err != nil {
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
	if cfg, err = loadCLIConfig(); err != nil || cfg.Report.Theme != "letter" {
		t.Errorf("report.theme after a rewrite = %q (%v)", cfg.Report.Theme, err)
	}
}
