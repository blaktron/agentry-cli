package engine

import (
	"context"
	"fmt"
	"net"
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

// The browser in a run: the engine starts the browse proxy with the
// run's scope on the relay egress network, gives the browser server's
// placeholders their values, writes the browser secrets into a file for
// it alone, copies what it saved into reports/browser/, and keeps the
// proxy's refusals as a receipt.

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
	for _, want := range []string{proxy + ":3128/tcp", "http://" + proxy + ":3128", "user: '" + dockerx.LocalDockerEngine().PersonUser() + "'", filepath.Join(runDir, "cfg", spec.BrowserOutDir) + ":/out-browser:rw"} {
		if !strings.Contains(string(cat), want) {
			t.Errorf("the rendered catalog lacks %q", want)
		}
	}
	if strings.Contains(string(cat), "@@RUN_") {
		t.Errorf("a placeholder is left in the rendered catalog:\n%s", cat)
	}

	// The secret is in no container's argv or environment the
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

	// What the browser saved reaches reports/browser/, and a
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

// stubSiteDNS answers lookupSiteHost from a table for the test.
func stubSiteDNS(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := lookupSiteHost
	lookupSiteHost = func(_ context.Context, host string) ([]net.IPAddr, error) {
		ips, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []net.IPAddr
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
	t.Cleanup(func() { lookupSiteHost = prev })
}

// TestAHostedRunGivesItsBrowserPublicSitesOnly: a hosted run's
// browser starts as a local run's does, but the browse proxy is given no
// private site, and the receipt says the sites are public.
func TestAHostedRunGivesItsBrowserPublicSitesOnly(t *testing.T) {
	stubSiteDNS(t, map[string][]string{"shop.example": {"93.184.215.14"}})
	p, f := browserFixture(t)
	happyFake(f)
	p.Hosted = true
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, ok := f.FindRun("-browse-proxy")
	if !ok {
		t.Fatal("no browse proxy was started")
	}
	argv := strings.Join(r.Argv, " ")
	if !strings.Contains(argv, "--browse-sites shop.example") || strings.Contains(argv, "--browse-private") {
		t.Errorf("a hosted run's proxy argv: %q", argv)
	}
	if got := env.CapabilitiesEnforced["browser.scope"]; !strings.Contains(got, "each a public address, on ports 80 and 443, in this hosted run") || strings.Contains(got, "LAN") {
		t.Errorf("browser.scope = %q", got)
	}
}

// TestAHostedRunRefusesASiteItCouldNotOpen: before any container starts:
// a private or loopback address, a name that resolves to one, a name that
// does not resolve, and a port a sandbox host does not let out. A run that
// reviews its inputs through the registry is hosted too.
func TestAHostedRunRefusesASiteItCouldNotOpen(t *testing.T) {
	for _, tc := range []struct{ name, site, want string }{
		{"private address", "http://10.0.0.5/", "10.0.0.5 is not a public address"},
		{"loopback v6", "http://[::1]:80/", "::1 is not a public address"},
		{"name to a LAN", "https://intranet.corp.example/", "resolves to 192.168.1.4, which is not a public address"},
		{"one address of two private", "https://split.example/", "resolves to 127.0.0.1"},
		{"unresolvable", "https://nowhere.invalid/", "could not be found on the public internet"},
		{"own port", "https://shop.example:8443/", "names port 8443"},
	} {
		for _, review := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s review=%v", tc.name, review), func(t *testing.T) {
				stubSiteDNS(t, map[string][]string{
					"shop.example":          {"93.184.215.14"},
					"intranet.corp.example": {"192.168.1.4"},
					"split.example":         {"93.184.215.14", "127.0.0.1"},
				})
				p, f := browserFixture(t)
				happyFake(f)
				p.Intake.Values["site"] = tc.site
				if review {
					p.InputReview = &fakeReview{}
				} else {
					p.Hosted = true
				}
				env, err := Run(context.Background(), f, p)
				if env == nil || env.ExitCode != ExitPolicy || err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "intake site:") {
					t.Fatalf("hosted run of %s: %v (%+v)", tc.site, err, env)
				}
				for _, op := range f.Ops() {
					if strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "NetworkCreate") || strings.HasPrefix(op, "Pull") {
						t.Errorf("a hosted run made %s before it refused", op)
					}
				}
			})
		}
	}
}

// TestAHostedRunTakesEitherWebPortWhateverTheScheme: a sandbox host lets
// 80 and 443 out, so http on 443 and https on 80 are not refused.
func TestAHostedRunTakesEitherWebPortWhateverTheScheme(t *testing.T) {
	stubSiteDNS(t, map[string][]string{"shop.example": {"93.184.215.14"}})
	for _, site := range []string{"http://shop.example:443/", "https://shop.example:80/"} {
		p, f := browserFixture(t)
		happyFake(f)
		p.Hosted = true
		p.Intake.Values["site"] = site
		if _, err := Run(context.Background(), f, p); err != nil {
			t.Errorf("hosted run of %s: %v", site, err)
		}
	}
}

// TestALocalRunKeepsItsPrivateSite: the person's own LAN site is still a
// local run's to test, and no DNS is asked first.
func TestALocalRunKeepsItsPrivateSite(t *testing.T) {
	stubSiteDNS(t, nil)
	p, f := browserFixture(t)
	happyFake(f)
	p.Intake.Values["site"] = "http://192.168.1.20:8080/"
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, _ := f.FindRun("-browse-proxy")
	if argv := strings.Join(r.Argv, " "); !strings.Contains(argv, "--browse-private 192.168.1.20") {
		t.Errorf("a local run's proxy argv: %q", argv)
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

// TestABrowserRunsRuntimeLineNamesTheBrowseProxy: the browse proxy is on
// the daemon's default runtime by design, so even a run whose every other
// class is sandboxed does not say none is.
func TestABrowserRunsRuntimeLineNamesTheBrowseProxy(t *testing.T) {
	withFilter(t, true)
	t.Setenv("SITE_PASSWORD", plantedSitePassword)
	p, _ := fixtureEdited(t, func(pol *policy.Policy) {
		pol.Docker.Runtime = "runsc"
		pol.Docker.Resolvers = []string{"192.0.2.53"}
		pol.Docker.GatewayRuntime = "runsc-uds"
	}, declareBrowser)
	line := runtimeLine(p)
	if strings.Contains(line, "no container class of the run is on the daemon's default runtime") ||
		!strings.Contains(line, "on the daemon's default runtime, as measured under runsc: the browse proxy (it resolves every name the browser asks for through docker's embedded DNS") {
		t.Errorf("runtime line: %q", line)
	}
}

// TestTheSecretsLineNamesOnlyWhatTheRunWasGiven: the line names the
// declared secrets the run was given and says which it was not given.
func TestTheSecretsLineNamesOnlyWhatTheRunWasGiven(t *testing.T) {
	declared := []string{"SITE_USER", "SITE_PASSWORD"}
	if got := browserSecretsLine(declared, nil); got != "none of SITE_USER, SITE_PASSWORD was given to this run, so the browser has no secret to type (its secrets file is empty)" {
		t.Errorf("none given: %q", got)
	}
	got := browserSecretsLine(declared, []string{"SITE_PASSWORD"})
	if !strings.HasPrefix(got, "SITE_PASSWORD, in a file bound read-only into the browser server alone") || !strings.HasSuffix(got, "; not given to this run: SITE_USER") {
		t.Errorf("one given: %q", got)
	}
	if got := browserSecretsLine(declared, declared); strings.Contains(got, "not given") || !strings.HasPrefix(got, "SITE_USER, SITE_PASSWORD, in a file") {
		t.Errorf("both given: %q", got)
	}
}
