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

// fakeAccount is an account's library as the chooser reads it: a default
// (or none, or an error) and designs by name.
type fakeAccount struct {
	doc   []byte
	err   error
	named map[string][]byte
	asked int
}

func (f *fakeAccount) Default(context.Context) ([]byte, error) {
	f.asked++
	return f.doc, f.err
}

func (f *fakeAccount) Named(_ context.Context, ref string) ([]byte, error) {
	f.asked++
	if doc, ok := f.named[ref]; ok {
		return doc, nil
	}
	return nil, errors.New("your account's library has no report design named " + ref)
}

// TestChooseReportTheme: a flag, then the account's default, then the
// config, then Plain; a name or a file typed in a flag or the config that is
// not a design refuses as a usage error; the account's default, unread or
// not one this CLI accepts, is a warning and the next choice runs.
func TestChooseReportTheme(t *testing.T) {
	dir := t.TempDir()
	house := designOf(t, "editorial", "House", "#7a1f1f")
	houseFile := filepath.Join(dir, "house.json")
	mustWriteFile(t, houseFile, house)
	mine := designOf(t, "modern", "Mine", "#0b5394")
	account := func(doc []byte, err error) *fakeAccount {
		return &fakeAccount{doc: doc, err: err, named: map[string][]byte{"House": house, "Mine": mine}}
	}
	conf := func(name string) cliConfig { return cliConfig{Report: reportConfig{Theme: name}} }
	version2 := bytes.Replace(mine, []byte(`"agentryTheme": 3`), []byte(`"agentryTheme": 2`), 1)
	for _, c := range []struct {
		name    string
		flags   reportFlags
		cfg     cliConfig
		account *fakeAccount
		want    engine.ReportTheme
		warns   string
	}{
		{"nothing set", reportFlags{}, cliConfig{}, nil, engine.ReportTheme{Name: "plain", From: engine.ThemeFromDefault}, ""},
		{"the theme flag wins", reportFlags{theme: "classic"}, conf("modern"), account(mine, nil), engine.ReportTheme{Name: "classic", From: engine.ThemeFromFlag}, ""},
		{"a design file wins", reportFlags{design: houseFile}, conf("modern"), account(mine, nil), engine.ReportTheme{Design: house, From: engine.ThemeFromDesignFlag}, ""},
		{"a design by name", reportFlags{design: "House"}, conf("modern"), account(mine, nil), engine.ReportTheme{Design: house, From: engine.ThemeFromDesignFlag}, ""},
		{"the account's default over the config", reportFlags{}, conf("modern"), account(mine, nil), engine.ReportTheme{Design: mine, From: engine.ThemeFromAccount}, ""},
		{"no default is not plain", reportFlags{}, conf("technical"), account(nil, nil), engine.ReportTheme{Name: "technical", From: engine.ThemeFromConfig}, ""},
		{"a design file in the config", reportFlags{}, cliConfig{Report: reportConfig{Design: houseFile}}, nil, engine.ReportTheme{Design: house, From: engine.ThemeFromDesignConfig}, ""},
		{"an account that cannot be read", reportFlags{}, conf("classic"), account(nil, errors.New("offline")), engine.ReportTheme{Name: "classic", From: engine.ThemeFromConfig}, "could not be read (offline); PDFs use classic"},
		{"a default this CLI does not accept", reportFlags{}, cliConfig{Report: reportConfig{Design: houseFile}}, account(version2, nil),
			engine.ReportTheme{Design: house, From: engine.ThemeFromDesignConfig}, `is not one this CLI can draw (report design: agentryTheme: must be 3`},
	} {
		var warn bytes.Buffer
		var acct accountDesigns
		if c.account != nil {
			acct = c.account
		}
		got, err := chooseReportTheme(context.Background(), c.flags, c.cfg, acct, &warn)
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
	old := filepath.Join(dir, "old.json")
	mustWriteFile(t, old, version2)
	themes := "plain, classic, modern, editorial, technical, boardroom, studio, field, civic, lumen"
	for _, c := range []struct {
		name      string
		flags     reportFlags
		cfg       cliConfig
		account   *fakeAccount
		signedOut bool
		says      string
	}{
		{"an unknown theme flag", reportFlags{theme: "fancy"}, cliConfig{}, nil, false, themes},
		{"a capitalised theme flag", reportFlags{theme: "Classic"}, cliConfig{}, nil, false, themes},
		{"an old theme's name", reportFlags{theme: "formal"}, cliConfig{}, nil, false, themes},
		{"both flags", reportFlags{theme: "plain", design: houseFile}, cliConfig{}, nil, false, "give one"},
		{"a missing design file", reportFlags{design: filepath.Join(dir, "none.json")}, cliConfig{}, nil, false, "no such file"},
		{"a design that fails the contrast rule", reportFlags{design: pale}, cliConfig{}, nil, false, "accent"},
		{"a version-2 design file", reportFlags{design: old}, cliConfig{}, nil, false, "a theme document version 2; this CLI draws version 3"},
		{"a name with no sign-in", reportFlags{design: "House"}, cliConfig{}, nil, true, "not signed in"},
		{"a name the library lacks", reportFlags{design: "Nope"}, cliConfig{}, account(mine, nil), false, "no report design named Nope"},
		{"a named design this CLI does not accept", reportFlags{design: "Old"}, cliConfig{}, &fakeAccount{named: map[string][]byte{"Old": version2}}, false, "not one this CLI can draw"},
		{"an unknown config theme", reportFlags{}, conf("house"), nil, false, themes},
		{"both in the config", reportFlags{}, cliConfig{Report: reportConfig{Theme: "plain", Design: houseFile}}, nil, false, "keep one"},
		{"a config design that is not one", reportFlags{}, cliConfig{Report: reportConfig{Design: pale}}, nil, false, "accent"},
		{"a version-2 design in the config", reportFlags{}, cliConfig{Report: reportConfig{Design: old}}, nil, false, "a theme document version 2; this CLI draws version 3"},
	} {
		spy := &fakeAccount{doc: mine}
		var acct accountDesigns = spy
		switch {
		case c.signedOut:
			acct = nil
		case c.account != nil:
			acct = c.account
		}
		_, err := chooseReportTheme(context.Background(), c.flags, c.cfg, acct, &bytes.Buffer{})
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a usage error saying %q", c.name, err, c.says)
		}
		if spy.asked > 0 {
			t.Errorf("%s: the account's default was asked although a typed choice refused the run", c.name)
		}
	}
}

// TestDesignIsFile: --report-design reads a file when the value has a path
// separator or the .json extension, or names a file that exists; anything
// else names a design of the library.
func TestDesignIsFile(t *testing.T) {
	dir := t.TempDir()
	exists := filepath.Join(dir, "House")
	mustWriteFile(t, exists, []byte("{}"))
	for value, want := range map[string]bool{
		"House": false, "Acme Assurance": false, "b1b1f0f2-2f3e-4c2b-9d8a-5a5a5a5a5a5a": false,
		"house.json": true, "HOUSE.JSON": true, "designs/house": true, `designs\house`: true, "./House": true, exists: true,
	} {
		if got := designIsFile(value); got != want {
			t.Errorf("designIsFile(%q) = %v, want %v", value, got, want)
		}
	}
}

func mustWriteFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadAccountTheme: the signed-in account's library at the registry,
// read with the sign-in's token: the default design with its images, or
// none when no design is the default; a design by name or id; a registry
// from before the library is none; any other refusal is an error. No
// account is asked without a sign-in, nor in a driver's run, which holds a
// run grant.
func TestReadAccountTheme(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	mine := designOf(t, "modern", "Mine", "#0b5394")
	house := designOf(t, "editorial", "House", "#7a1f1f")

	if readAccountTheme(reg.URL) != nil {
		t.Fatal("an account was asked with no sign-in")
	}
	t.Setenv(envToken, "agy_test")
	read := readAccountTheme(reg.URL)
	if read == nil {
		t.Fatal("a signed-in run asks no account")
	}
	if doc, err := read.Default(context.Background()); err != nil || doc != nil {
		t.Errorf("an empty library = (%s, %v)", doc, err)
	}
	mineID := reg.add(t, mine, false)
	houseID := reg.add(t, house, false)
	if doc, err := read.Default(context.Background()); err != nil || doc != nil {
		t.Errorf("no default = (%s, %v)", doc, err)
	}
	reg.setDefault(houseID)
	doc, err := read.Default(context.Background())
	if err != nil || !sameDesign(t, doc, house) {
		t.Errorf("the default = (%s, %v)", doc, err)
	}
	if reg.lastAuth != "Bearer agy_test" || reg.lastInclude != "assets" {
		t.Errorf("the default was read with auth %q and include=%q", reg.lastAuth, reg.lastInclude)
	}
	for _, ref := range []string{"Mine", "mine", mineID, strings.ToUpper(mineID)} {
		if doc, err := read.Named(context.Background(), ref); err != nil || !sameDesign(t, doc, mine) {
			t.Errorf("Named(%q) = (%.40s, %v)", ref, doc, err)
		}
	}
	if _, err := read.Named(context.Background(), "Nope"); err == nil || !strings.Contains(err.Error(), `no report design named "Nope"`) || !strings.Contains(err.Error(), "Mine, House") {
		t.Errorf("Named(Nope) = %v", err)
	}
	reg.add(t, designOf(t, "plain", "mine", "#2b2f36"), false)
	if doc, err := read.Named(context.Background(), "Mine"); err != nil || !sameDesign(t, doc, mine) {
		t.Errorf("an exact name beside one in another case = (%.40s, %v)", doc, err)
	}
	if _, err := read.Named(context.Background(), "MINE"); err == nil || !strings.Contains(err.Error(), "2 report designs named") {
		t.Errorf("an ambiguous name = %v", err)
	}
	reg.broken = true
	if _, err := read.Default(context.Background()); err == nil || !strings.Contains(err.Error(), "Internal error") {
		t.Errorf("a refusal = %v", err)
	}
	reg.broken = false
	old := readAccountTheme(reg.URL + "/older")
	if doc, err := old.Default(context.Background()); err != nil || doc != nil {
		t.Errorf("a registry without the library = (%s, %v)", doc, err)
	}
	t.Setenv(envRunGrant, "grant")
	if readAccountTheme(reg.URL) != nil {
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
	// A name with no sign-in is a usage error too.
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--allow-unsigned", "--yes", "--policy", policyPath, "--report-design", "House")
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
// read whole by --report-design: the read is bounded by the images'
// sizes, not the 24 KB of the design alone.
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

var _ = http.StatusOK

// sameDesign says two documents are the same design, whatever their
// spacing: the registry answers a design compacted.
func sameDesign(t *testing.T, a, b []byte) bool {
	t.Helper()
	da, err := reportdesign.Parse(a)
	if err != nil {
		return false
	}
	db, err := reportdesign.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return da.SHA256() == db.SHA256()
}
