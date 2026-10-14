package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
)

// The browser (agentry-notes plans/browser-testing.md §6, §7, §7.1, D1,
// D2, D8, D9; agentry-cli#915). The compiler put the browser server in the
// gateway's catalog with placeholders; the engine gives them their values:
// the browse proxy it starts on the run's relay egress network, under a
// run-unique name the browser's one relay is pointed at, holding the scope
// (the sites the person named, the declared hosts, or the public web);
// the user the server runs as; its output folder, copied into
// reports/browser/ as the run ends; and, for a line that declares browser
// secrets, the --secrets file bound into it alone.

// browserRun is a run's browser, once the engine has readied it.
type browserRun struct {
	// proxy is the browse proxy's container name (spec.PHBrowseProxy), and
	// proxyIP its address on the relay egress network once it started,
	// which the docker filter pins for its relay.
	proxy, proxyIP string
	// sites are the run's sites as the proxy takes them, and private the
	// ones the person typed whose address may be a LAN's (D2): every site
	// of a local run.
	sites, private []string
	// copied is set once the output folder reached reports/browser/.
	copied bool
}

// The tmpfs the docker filter gives the browser server (the owner added
// by the filter): a home for the profile Chromium keeps in --isolated, and
// a /tmp for its shared memory, which Playwright's --disable-dev-shm-usage
// puts there (M1: 255-570 MiB for two tabs, a full-page screenshot and a
// PDF).
const (
	browserHomeTmpfs = "rw,nosuid,nodev,size=268435456,mode=0700"
	browserTmpTmpfs  = "rw,nosuid,nodev,size=1073741824,mode=1777"
)

func browserTmpfs() map[string]string {
	return map[string]string{browser.Home: browserHomeTmpfs, "/tmp": browserTmpTmpfs}
}

// HostedBrowserRefusal is a hosted run's refusal of a release that
// declares a browser, until hosted runs carry one (plan M7,
// agentry-enterprise#565): exit 11.
func HostedBrowserRefusal() error {
	return Classify(ExitPolicy, errors.New("a hosted run has no browser yet: run this QuickAgent on your own computer"))
}

// checkBrowser refuses, before anything is made, a run whose plan declares
// a browser the run cannot give as declared: a hosted run, a plan without
// the browser server, a site answer that is not one, and a browser with
// nowhere to go.
func checkBrowser(p Params) error {
	c := p.Plan.Capabilities.Browser
	if c == nil {
		return nil
	}
	if p.InputReview != nil {
		return HostedBrowserRefusal()
	}
	if p.Plan.Browser == nil {
		return Classify(ExitCompile, errors.New("the plan declares a browser but no browser server; recompile it"))
	}
	sites, err := browserSites(p)
	if err != nil {
		return err
	}
	if len(sites) == 0 && len(c.Hosts) == 0 && !c.Public {
		return Classify(ExitCLI, fmt.Errorf("the browser has nowhere to go: answer its site question (--intake %s=https://…)", strings.Join(c.Sites, "=…, --intake ")))
	}
	return nil
}

// browserSites are the run's sites, from the answers to the site
// questions, in the plan's order; an unanswered optional one adds none.
func browserSites(p Params) ([]browser.Site, error) {
	var out []browser.Site
	for _, id := range p.Plan.Capabilities.Browser.Sites {
		if p.Intake == nil || p.Intake.Values[id] == "" {
			continue
		}
		s, err := browser.ParseSite(p.Intake.Values[id])
		if err != nil {
			return nil, Classify(ExitCLI, fmt.Errorf("intake %s: %w", id, err))
		}
		out = append(out, s)
	}
	return out, nil
}

// browseProxyName is the run's browse proxy's container name, the
// spec.PHBrowseProxy of its plan files: known from the run's id alone, so
// the gateway's files are rendered with it before the proxy starts.
func (rd *runDirs) browseProxyName() string {
	return containerName(filepath.Base(rd.root), "browse-proxy")
}

func (rd *runDirs) browserOutDir() string { return filepath.Join(rd.cfg, spec.BrowserOutDir) }
func (rd *runDirs) browserDir() string    { return filepath.Join(rd.cfg, spec.BrowserDir) }

// ensureBrowserImage pulls the browser server's image by its digest when
// the machine lacks it: the gateway starts servers with --pull never. A
// variable so a test can stand in for the pull.
var ensureBrowserImage = func(ctx context.Context, d dockerx.Client, ref string) error {
	ok, err := d.ImageExists(ctx, ref)
	if err != nil || ok {
		return err
	}
	return d.Pull(ctx, ref)
}

// prepareBrowser readies the browser before the infrastructure starts:
// its image, its folders, its secrets file, and the run's sites, and names
// what the receipt says of it.
func prepareBrowser(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, env *receipt.Envelope, secrets map[string]string) error {
	c := p.Plan.Capabilities.Browser
	if c == nil {
		return nil
	}
	if err := ensureBrowserImage(ctx, d, p.Plan.Browser.Image); err != nil {
		return Classify(ExitGateway, fmt.Errorf("the browser server's image %s: %w", p.Plan.Browser.Image, err))
	}
	sites, err := browserSites(p)
	if err != nil {
		return err
	}
	b := &browserRun{proxy: rd.browseProxyName()}
	for _, s := range sites {
		b.sites = append(b.sites, s.ProxyEntry())
		// Every site of a local run is one the person typed (D2); the
		// proxy still dials a public or LAN address only.
		if !slices.Contains(b.private, s.Host) {
			b.private = append(b.private, s.Host)
		}
	}
	for _, dir := range []string{rd.browserOutDir(), rd.browserDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Classify(ExitCLI, fmt.Errorf("the browser's folders: %w", err))
		}
	}
	// The tools server reads the browser's files at /out/reports/browser, inside
	// its output folder: the mount point is made here, as the person's, so
	// the daemon does not make it as root.
	if err := os.MkdirAll(filepath.Join(rd.outDir, spec.BrowserReports), 0o755); err != nil { //nolint:gosec // the person's own output folder
		return Classify(ExitCLI, fmt.Errorf("reports/browser: %w", err))
	}
	if len(c.Secrets) > 0 {
		f, err := browserSecretsFile(c.Secrets, browserSecretValues(p.Plan, c.Secrets, secrets))
		if err != nil {
			return Classify(ExitPolicy, err)
		}
		// The folder dies with the run, kept run folder or not, through the
		// teardown's own sensitive removal (prepareRunDirs), which reports a
		// file it could not delete.
		if err := os.WriteFile(filepath.Join(rd.browserDir(), spec.BrowserSecretsFile), f, 0o600); err != nil {
			return Classify(ExitCLI, fmt.Errorf("the browser's secrets file: %w", err))
		}
	}
	for _, dir := range []string{rd.browserOutDir(), rd.browserDir()} {
		relabelShared(ctx, p.Stderr, dir)
	}
	rd.browser = b
	// Whatever happens next, what the browser saved reaches the person.
	td.add(func(context.Context) { copyBrowserOut(rd, p.Stderr) })
	env.CapabilitiesEnforced["browser.image"] = p.Plan.Browser.Image + ", pulled by digest"
	env.CapabilitiesEnforced["browser.scope"] = browserScopeLine(c, b)
	if len(c.Secrets) > 0 {
		env.CapabilitiesEnforced["browser.secrets"] = fmt.Sprintf("%s, in a file bound read-only into the browser server alone and removed with the run; the server types a value where the agent passes its name, and replaces the exact value with the name in what it returns (its own masking, which a transformed value would pass, so %s is not granted); the runner masks the values again in the transcript, the receipts and the browser's text files",
			strings.Join(c.Secrets, ", "), browser.ToolEvaluate)
	}
	return nil
}

// browserScopeLine says where this run's browser may go.
func browserScopeLine(c *spec.BrowserCap, b *browserRun) string {
	var parts []string
	if len(b.sites) > 0 {
		parts = append(parts, "the sites "+strings.Join(b.sites, ", ")+" and their subdomains, as the person named them (each may be a LAN's or a tailnet's address in this local run)")
	}
	if len(c.Hosts) > 0 {
		parts = append(parts, "the hosts "+strings.Join(c.Hosts, ", "))
	}
	if c.Public {
		parts = append(parts, "any public site on ports 80 and 443 (the policy's browser.allowPublicWeb)")
	}
	return "the browse proxy " + b.proxy + " admits " + strings.Join(parts, "; ") + ", resolving every name itself; nothing else, and no loopback, link-local or metadata address"
}

// browserSecretValues are the browser secrets' values by name, from the
// run's resolved secrets, keyed as the gateway's file keys them: the
// release's own secret, else a declared server's of that name (V15 admits
// either, through optionalSecrets).
func browserSecretValues(plan *spec.Plan, names []string, resolved map[string]string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if v := resolved[compile.SecretKey(n)]; v != "" {
			out[n] = v
			continue
		}
		for _, srv := range plan.MCPServers {
			if v := resolved[compile.ServerSecretKey(srv.Name, n)]; slices.Contains(srv.Secrets, n) && v != "" {
				out[n] = v
				break
			}
		}
	}
	return out
}

// browserSecretsFile is the server's --secrets file: one NAME='value' line
// a secret, in the dotenv form the server reads. A value it could not
// carry intact (a newline, or both quote kinds) is refused rather than
// mangled.
func browserSecretsFile(names []string, values map[string]string) ([]byte, error) {
	var b strings.Builder
	for _, n := range names {
		v, ok := values[n]
		if !ok || v == "" {
			// An optional secret the run was not given: the browser has no
			// value to type, and the System Prompt names it as absent.
			continue
		}
		switch {
		case strings.ContainsAny(v, "\r\n"):
			return nil, fmt.Errorf("the secret %s holds a line break, which the browser's secrets file cannot carry", n)
		case !strings.Contains(v, "'"):
			fmt.Fprintf(&b, "%s='%s'\n", n, v)
		case !strings.ContainsAny(v, `"\$`):
			fmt.Fprintf(&b, "%s=\"%s\"\n", n, v)
		default:
			return nil, fmt.Errorf("the secret %s holds both kinds of quote, which the browser's secrets file cannot carry", n)
		}
	}
	return []byte(b.String()), nil
}

// browseProxyArgs are the proxy role's arguments for the run's scope.
func browseProxyArgs(c *spec.BrowserCap, b *browserRun) []string {
	a := []string{"proxy", "--addr", fmt.Sprintf(":%d", browser.ProxyPort)}
	if len(b.sites) > 0 {
		a = append(a, "--browse-sites", strings.Join(b.sites, ","), "--browse-private", strings.Join(b.private, ","))
	}
	if len(c.Hosts) > 0 {
		a = append(a, "--browse-hosts", strings.Join(c.Hosts, ","))
	}
	if c.Public {
		a = append(a, "--browse-public")
	}
	return a
}

// browserNetwork is the run's network for the browser server and its one
// relay, alone (sockfilter.Config.BrowserNetwork; invariant 2).
func browserNetwork(runID string) string { return "agentry-" + runID + "-browser" }

// makeBrowserNetwork makes the browser's network, internal, and queues its
// removal, which runs after the gateway and its relays are gone.
func makeBrowserNetwork(ctx context.Context, d dockerx.Client, p Params, runID string, td *teardown) error {
	if p.Plan.Browser == nil {
		return nil
	}
	name := browserNetwork(runID)
	if err := d.NetworkCreate(ctx, name, true, runLabels(runID)); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create the browser's network %s: %w", name, err))
	}
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, name) })
	td.networks = append(td.networks, name)
	return nil
}

// startBrowseProxy starts the browse proxy on the run's relay egress
// network, where the browser's relay reaches it, on the daemon's default
// runtime (it resolves names through Docker's DNS, which a sandboxed
// runtime does not reach, DR-23), hardened as the source proxy is. Its
// address is kept for the docker filter's relay pin.
func startBrowseProxy(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) error {
	b := rd.browser
	if b == nil {
		return nil
	}
	_, out := relayNetworks(runID)
	s := dockerx.RunSpec{
		Name:    b.proxy,
		Image:   p.Plan.Proxy.Image,
		Argv:    browseProxyArgs(p.Plan.Capabilities.Browser, b),
		Network: out,
		Labels:  runLabels(runID),
	}
	s = readOnlyRoot(hardened(s, p.limits()), "/tmp")
	td.add(func(c context.Context) { _ = d.RemoveForce(c, b.proxy) })
	if err := d.RunDetached(ctx, s); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start the browse proxy: %w", err))
	}
	ip, err := d.ContainerIP(ctx, b.proxy, out)
	if err != nil {
		return Classify(ExitGateway, fmt.Errorf("the browse proxy's address: %w", err))
	}
	b.proxyIP = ip
	return nil
}

// browseProxyRelay is the relay target the docker filter admits for the
// browse proxy, and browseProxyPin its address for the relay's hosts file.
func (rd *runDirs) browseProxyRelay() []string {
	if rd.browser == nil {
		return nil
	}
	return []string{fmt.Sprintf("%s:%d", rd.browser.proxy, browser.ProxyPort)}
}

func (rd *runDirs) browseProxyPin() map[string]string {
	if rd.browser == nil || rd.browser.proxyIP == "" {
		return nil
	}
	return map[string]string{rd.browser.proxy: rd.browser.proxyIP}
}

// browserUser is the uid:gid the browser server runs as: the invoking
// user's, as the host link runs, or "" where there is none (Windows),
// which leaves the image's own.
func browserUser() string {
	u, _, _ := hostLinkUser()
	return u
}

// collectBrowseProxyLog keeps the browse proxy's decisions as
// receipts/browser-proxy.log, masked like every receipt, and counts its
// refusals into the receipt: the page's, and those of Chrome's own
// services (plan §6).
func collectBrowseProxyLog(ctx context.Context, d dockerx.Client, rd *runDirs, env *receipt.Envelope) {
	if rd.browser == nil {
		return
	}
	logs, err := d.Logs(ctx, rd.browser.proxy, 20000)
	if err != nil {
		return
	}
	logs = rd.redact.String(logs)
	_ = os.WriteFile(filepath.Join(rd.receiptsDir, "browser-proxy.log"), []byte(logs), 0o644) //nolint:gosec // a receipt, masked
	env.CapabilitiesEnforced["browser.proxy"] = browseProxyLine(logs)
}

// browseProxyLine counts a browse proxy log's refusals.
func browseProxyLine(logs string) string {
	page, chrome := 0, 0
	for _, l := range strings.Split(logs, "\n") {
		if !strings.HasPrefix(l, "deny ") {
			continue
		}
		if strings.HasSuffix(l, netproxy.ChromeServiceMark) {
			chrome++
		} else {
			page++
		}
	}
	return fmt.Sprintf("%d connections refused (%d more of Chrome's own services); each with its host and reason in receipts/browser-proxy.log", page, chrome)
}

// The browser's files that reach the person: each at most
// browserFileMax, all of them at most browserTotalMax.
const (
	browserFileMax  = 64 << 20
	browserTotalMax = 512 << 20
)

// copyBrowserOut copies the browser's output folder into reports/browser/,
// once: regular files only, through an os.Root at each end so a name cannot
// lead out, each held to browserFileMax as it is copied (the browser may
// still be writing) and all to browserTotalMax. A text file the server
// wrote (a snapshot, a console log) is then masked like every receipt,
// behind the server's own exact-match masking of a typed secret.
func copyBrowserOut(rd *runDirs, stderr io.Writer) {
	if rd.browser == nil || rd.browser.copied {
		return
	}
	rd.browser.copied = true
	dstDir := filepath.Join(rd.outDir, spec.BrowserReports)
	dst, err := os.OpenRoot(dstDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentry run: reports/browser: %v\n", err)
		return
	}
	defer func() { _ = dst.Close() }()
	src, err := os.OpenRoot(rd.browserOutDir())
	if err != nil {
		return
	}
	defer func() { _ = src.Close() }()
	var total int64
	_ = fs.WalkDir(src.FS(), ".", func(name string, de fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return nil
		}
		if de.IsDir() {
			_ = dst.Mkdir(name, 0o755)
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		n, err := copyBrowserFile(src, dst, name, browserTotalMax-total)
		total += n
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agentry run: reports/browser/%s: %v\n", name, err)
			return nil
		}
		if rd.redact != nil && browserTextFile(name) {
			rd.redact.File(filepath.Join(dstDir, filepath.FromSlash(name)))
		}
		return nil
	})
}

// copyBrowserFile copies one file, at most browserFileMax and room bytes;
// one that is larger is left out.
func copyBrowserFile(src, dst *os.Root, name string, room int64) (int64, error) {
	limit := min(int64(browserFileMax), room)
	in, err := src.Open(name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	w, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, cerr := io.Copy(w, io.LimitReader(in, limit+1))
	if err := w.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil && n > limit {
		cerr = fmt.Errorf("larger than %d bytes, left out", limit)
	}
	if cerr != nil {
		_ = dst.Remove(name)
		return 0, cerr
	}
	return n, nil
}

// browserTextFile says whether a file the server wrote is text the runner
// masks: its snapshots, console and network logs, saved pages.
func browserTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yml", ".yaml", ".log", ".md", ".txt", ".json", ".html", ".htm":
		return true
	}
	return false
}
