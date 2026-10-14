package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The browser in a run (agentry-notes plans/browser-testing.md §3
// invariants 2, 4, 6; §6, §7.1; agentry-cli#915): the engine starts the
// browse proxy with the run's scope on the relay egress network, gives the
// browser server's placeholders their values, writes the browser secrets
// into a file for it alone, copies what it saved into reports/browser/,
// and keeps the proxy's refusals as a receipt.

const plantedSitePassword = "planted-site-password-7302"

func declareBrowser(m *manifest.Manifest) {
	m.Intake = append(m.Intake,
		manifest.IntakeQuestion{ID: "site", Prompt: "Which site should I test?", Type: manifest.IntakeURL, Required: true})
	m.Capabilities.Secrets = append(m.Capabilities.Secrets, "SITE_PASSWORD")
	m.Capabilities.Browser = &manifest.Browser{Sites: []string{"site"}, Hosts: []string{"fonts.googleapis.com"}, Secrets: []string{"SITE_PASSWORD"}}
}

func browserFixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	t.Setenv("SITE_PASSWORD", plantedSitePassword)
	p, f := fixtureEdited(t, nil, declareBrowser)
	p.Intake = &intake.Answers{Values: map[string]string{"site": "https://Shop.Example/login"}}
	prev := ensureBrowserImage
	ensureBrowserImage = func(context.Context, dockerx.Client, string) error { return nil }
	t.Cleanup(func() { ensureBrowserImage = prev })
	return p, f
}

func TestABrowserRunStartsTheBrowseProxyWithItsScope(t *testing.T) {
	p, f := browserFixture(t)
	happyFake(f)
	p.KeepRunDir = true
	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")
	proxy := "agentry-cafe0000-browse-proxy"
	f.LogsByName[proxy] = "browse-proxy listening on :3128\nallow shop.example:443\n" +
		"deny tracker.example:443 (out of scope)\ndeny update.googleapis.com:443 (out of scope) [chrome service]\n" +
		"deny http shop.example:80 (it resolves to 10.0.0.9, which is not a public address)\n"
	var during struct {
		secrets  string
		mode     os.FileMode
		mountDir bool
	}
	f.Foreground["-agent"].Before = func(dockerx.RunSpec) {
		path := filepath.Join(runDir, "cfg", spec.BrowserDir, spec.BrowserSecretsFile)
		b, _ := os.ReadFile(path) //nolint:gosec // the test's own run folder
		during.secrets = string(b)
		if fi, err := os.Stat(path); err == nil {
			during.mode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Join(p.OutDir, spec.BrowserReports)); err == nil && fi.IsDir() {
			during.mountDir = true
		}
		// The browser saves a screenshot, and a link that would lead out.
		out := filepath.Join(runDir, "cfg", spec.BrowserOutDir)
		_ = os.WriteFile(filepath.Join(out, "home-phone.png"), []byte("PNG"), 0o600)
		// A snapshot the server saved with the value in it, as if its own
		// exact-match masking had missed it, and one too large to keep.
		_ = os.WriteFile(filepath.Join(out, "page-1.yml"), []byte("- paragraph: Your password is "+plantedSitePassword+"\n"), 0o600)
		_ = os.Symlink("/etc/passwd", filepath.Join(out, "leak"))
	}
	// The page echoes the value into a tool result the agent reads.
	f.Foreground["-agent"].Stdout = `{"type":"tool_call_response","tool":"browser_snapshot","response":"Your password is ` + plantedSitePassword + `"}` + "\n" + f.Foreground["-agent"].Stdout
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(p.OutDir, spec.BrowserReports, "page-1.yml")); err != nil || strings.Contains(string(b), plantedSitePassword) || !strings.Contains(string(b), "Your password is") {
		t.Errorf("reports/browser/page-1.yml: %q %v", b, err)
	}
	if during.secrets != "SITE_PASSWORD='"+plantedSitePassword+"'\n" || during.mode != 0o600 || !during.mountDir {
		t.Fatalf("during the run: secrets %q (%v), reports/browser made %v", during.secrets, during.mode, during.mountDir)
	}

	// The proxy: the run's scope, on the relay egress network, hardened.
	r, ok := f.FindRun("-browse-proxy")
	if !ok {
		t.Fatal("no browse proxy was started")
	}
	argv := strings.Join(r.Argv, " ")
	for _, want := range []string{"proxy --addr :3128", "--browse-sites shop.example", "--browse-private shop.example", "--browse-hosts fonts.googleapis.com"} {
		if !strings.Contains(argv, want) {
			t.Errorf("proxy argv %q lacks %q", argv, want)
		}
	}
	if r.Network != "agentry-cafe0000-relays-out" || r.Runtime != "" || !slices.Contains(r.CapDrop, "ALL") || !r.ReadOnly || strings.Contains(argv, "--browse-public") {
		t.Errorf("the browse proxy: %+v", r)
	}

	// The rendered catalog: the browser's placeholders have their values.
	cat, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml")) //nolint:gosec // the test's own run folder
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{proxy + ":3128/tcp", "http://" + proxy + ":3128", "user: '" + browserUser() + "'", filepath.Join(runDir, "cfg", spec.BrowserOutDir) + ":/out-browser:rw"} {
		if !strings.Contains(string(cat), want) {
			t.Errorf("the rendered catalog lacks %q", want)
		}
	}
	if strings.Contains(string(cat), "@@RUN_") {
		t.Errorf("a placeholder is left in the rendered catalog:\n%s", cat)
	}

	// Invariant 4: the secret is in no container's argv or environment the
	// runner makes (the gateway binds the file into the browser alone), and
	// in no file the run leaves.
	for _, c := range f.Calls {
		for _, a := range c.Run.Argv {
			if strings.Contains(a, plantedSitePassword) {
				t.Errorf("%s: the browser secret is in its argv", c.Run.Name)
			}
		}
		for _, m := range c.Run.Mounts {
			if strings.Contains(m.Source, filepath.Join("cfg", spec.BrowserDir)) {
				t.Errorf("%s mounts the browser's secrets folder", c.Run.Name)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "cfg", spec.BrowserDir, spec.BrowserSecretsFile)); !os.IsNotExist(err) {
		t.Errorf("the browser's secrets file outlived the run: %v", err)
	}
	_ = filepath.WalkDir(p.OutDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), plantedSitePassword) { //nolint:gosec // the test's own output
				t.Errorf("%s holds the browser secret", path)
			}
		}
		return nil
	})

	// Invariant 6: what the browser saved reaches reports/browser/, and a
	// link it made does not lead out of it.
	if b, err := os.ReadFile(filepath.Join(p.OutDir, spec.BrowserReports, "home-phone.png")); err != nil || string(b) != "PNG" {
		t.Errorf("reports/browser/home-phone.png: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(p.OutDir, spec.BrowserReports, "leak")); !os.IsNotExist(err) {
		t.Errorf("a link the browser made was copied: %v", err)
	}

	log, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "browser-proxy.log"))
	if err != nil || !strings.Contains(string(log), "deny tracker.example:443 (out of scope)") {
		t.Errorf("receipts/browser-proxy.log: %s %v", log, err)
	}
	for key, want := range map[string]string{
		"browser.proxy":   "2 connections refused (1 more of Chrome's own services)",
		"browser.scope":   "shop.example",
		"browser.image":   policy.DefaultBrowserImage,
		"browser.secrets": "SITE_PASSWORD, in a file bound read-only into the browser server alone",
		"browser":         "browse proxy",
		"containers":      "the browser server (",
	} {
		if !strings.Contains(env.CapabilitiesEnforced[key], want) {
			t.Errorf("%s = %q, want %q", key, env.CapabilitiesEnforced[key], want)
		}
	}
	doc, _ := os.ReadFile(filepath.Join(runDir, "inputs", "intake.json")) //nolint:gosec // the test's own run folder
	if !strings.Contains(string(doc), "https://Shop.Example/login") {
		t.Errorf("intake.json does not give the site: %s", doc)
	}
}

// TestAHostedRunRefusesABrowser, until hosted runs carry one (M7): before
// any container starts.
func TestAHostedRunRefusesABrowser(t *testing.T) {
	p, f := browserFixture(t)
	happyFake(f)
	p.InputReview = &fakeReview{}
	env, err := Run(context.Background(), f, p)
	if env == nil || env.ExitCode != ExitPolicy || err == nil || !strings.Contains(err.Error(), "hosted run has no browser") {
		t.Fatalf("hosted run: %v (%+v)", err, env)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "NetworkCreate") || strings.HasPrefix(op, "Pull") {
			t.Errorf("a hosted run made %s before it refused", op)
		}
	}
}

func TestABrowserWithNowhereToGoIsRefused(t *testing.T) {
	p, f := browserFixture(t)
	happyFake(f)
	p.Plan.Capabilities.Browser.Hosts = nil
	p.Intake = &intake.Answers{Values: map[string]string{}}
	if env, err := Run(context.Background(), f, p); env == nil || env.ExitCode != ExitCLI || err == nil || !strings.Contains(err.Error(), "nowhere to go") {
		t.Fatalf("no site: %v (%+v)", err, env)
	}
	p, f = browserFixture(t)
	happyFake(f)
	p.Intake = &intake.Answers{Values: map[string]string{"site": "ftp://shop.example/"}}
	if env, err := Run(context.Background(), f, p); env == nil || err == nil || !strings.Contains(err.Error(), "intake site") {
		t.Fatalf("a bad site: %v (%+v)", err, env)
	}
}

func TestBrowserSecretsFile(t *testing.T) {
	f, err := browserSecretsFile([]string{"A", "B", "C", "D"}, map[string]string{"A": "plain", "B": "it's", "D": ""})
	if err != nil || string(f) != "A='plain'\nB=\"it's\"\n" {
		t.Fatalf("%q %v", f, err)
	}
	for _, bad := range []string{"two\nlines", `both ' and "`, `it's $HOME`} {
		if _, err := browserSecretsFile([]string{"X"}, map[string]string{"X": bad}); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestTheBrowserImageIsPulledWhenMissing(t *testing.T) {
	f := &dockerx.Fake{}
	if err := ensureBrowserImage(context.Background(), f, "ghcr.io/x/b@sha256:aa"); err != nil {
		t.Fatal(err)
	}
	if ops := f.Ops(); !slices.Contains(ops, "Pull:ghcr.io/x/b@sha256:aa") {
		t.Fatalf("ops %v", ops)
	}
	f = &dockerx.Fake{ExistingImages: map[string]bool{"ghcr.io/x/b@sha256:aa": true}}
	_ = ensureBrowserImage(context.Background(), f, "ghcr.io/x/b@sha256:aa")
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Pull") {
			t.Fatalf("pulled an image the machine has: %v", f.Ops())
		}
	}
}
