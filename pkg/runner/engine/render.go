package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The page renderer: a rendered web_read (spec.CapSummary.WebRender). The
// tools server leaves a request in the run's render folder; the engine
// opens the page in a headless Chromium from the browser image, one
// container a page, and leaves the answer beside it (toolsrv/render.go
// names the files). Each container is held as the browser server is:
// Chromium's own sandbox on under the browser's seccomp profile (or the
// run's runtime), every capability dropped, no new privileges, a read-only
// root with a tmpfs home and /tmp, the person's user. Its one network is
// internal to it and the render proxy, which admits public addresses on
// ports 80 and 443 alone and is the only container on it with a way out.
// Nothing of the run's is mounted into it, no secret reaches it, and its
// profile dies with it, so no cookie outlives a page.

// renderScript drives the image's Playwright on the image's Chromium, the
// executable its PLAYWRIGHT_MCP_EXECUTABLE_PATH names (left unset,
// Playwright looks for its own download): open the page (60 seconds for its
// document), let its scripts settle (6 more for the network to go quiet),
// and print the address it ended on, its status and the HTML it drew. A
// page that moved on while its HTML was read is not answered, so the
// address always names the page the HTML came from.
const renderScript = `const {chromium}=require('/app/node_modules/playwright-core');
(async()=>{
  const [url,proxy]=process.argv.slice(1).filter(a=>a!=='--');
  const b=await chromium.launch({channel:'chromium',executablePath:process.env.PLAYWRIGHT_MCP_EXECUTABLE_PATH||undefined,headless:true,chromiumSandbox:true,proxy:{server:proxy}});
  try{
    const ctx=await b.newContext({viewport:{width:1280,height:720},acceptDownloads:false,serviceWorkers:'block'});
    const p=await ctx.newPage();
    const r=await p.goto(url,{waitUntil:'domcontentloaded',timeout:60000});
    try{await p.waitForLoadState('networkidle',{timeout:6000});}catch(e){}
    const before=p.url();
    const html=await p.content();
    if(p.url()!==before){throw new Error('the page moved on while it was read');}
    process.stdout.write(before+'\n'+(r?r.status():0)+'\n'+html);
  }finally{await b.close();}
})().catch(e=>{process.stderr.write(String((e&&e.message)||e).slice(0,300));process.exit(2);});`

// renderProxyAlias is the render proxy's name on the render network.
const renderProxyAlias = "agentry-render-proxy"

// renderIDPattern is a request's id as the tools server names it.
var renderIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// maxRenders bounds a run's renders, whatever its folder asks: twice the
// web tools' page budget, so a tools server that asks again is still held.
const maxRenders = 2 * toolsrv.DefaultMaxPages

// renderSlots is how many pages render at once.
const renderSlots = 2

// renderRun is a run's page renderer, once started.
type renderRun struct {
	image, network, proxy, proxyIP, seccomp, user, runtime string
	stop                                                   context.CancelFunc
	done                                                   chan struct{}
	mu                                                     sync.Mutex
	pages, failed                                          int
	// inflight are the requests being rendered.
	inflight map[string]bool
	// closed closes the renderer once: its watcher stopped, its proxy's
	// log kept and its count in the receipt.
	closed sync.Once
}

func (rd *runDirs) renderDir() string { return filepath.Join(rd.cfg, spec.RenderDir) }

func renderNetwork(runID string) string { return "agentry-" + runID + "-render" }

// prepareRender readies the renderer for a plan that grants it: its
// folder, its image, its network and proxy, and the watcher that answers
// the tools server's requests while the run lasts.
func prepareRender(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string, env *receipt.Envelope) error {
	if p.Plan.Render == nil || !p.Plan.Capabilities.WebRender {
		return nil
	}
	if p.hosted() {
		// A sandbox host's slots count no browser beside a build.
		return Classify(ExitPolicy, errors.New("a run on a sandbox host does not render pages: its plan grants a rendered web_read"))
	}
	if err := os.MkdirAll(rd.renderDir(), 0o700); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the page renderer's folder: %w", err))
	}
	relabelShared(ctx, p.Stderr, rd.renderDir())
	if err := ensureBrowserImage(ctx, d, p.Plan.Render.Image); err != nil {
		return Classify(ExitGateway, fmt.Errorf("the page renderer's image %s: %w", p.Plan.Render.Image, err))
	}
	r := &renderRun{image: p.Plan.Render.Image, network: renderNetwork(runID), proxy: containerName(runID, "render-proxy"),
		user: rd.personUser(), runtime: p.Plan.Runtime()}
	if r.user == "" {
		r.user = browser.ImageUser
	}
	if r.runtime == "" {
		profile, err := sockfilter.BrowserSeccomp()
		if err != nil {
			return Classify(ExitCLI, err)
		}
		r.seccomp = filepath.Join(rd.cfg, "render-seccomp.json")
		if err := os.WriteFile(r.seccomp, []byte(profile), 0o644); err != nil { //nolint:gosec // a seccomp profile, no secret
			return Classify(ExitCLI, fmt.Errorf("the page renderer's seccomp profile: %w", err))
		}
	}
	labels := runLabels(runID)
	if err := d.NetworkCreate(ctx, r.network, true, labels); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create the page renderer's network: %w", err))
	}
	td.networks = append(td.networks, r.network)
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, r.network) })
	proxySpec := dockerx.RunSpec{
		Name:    r.proxy,
		Image:   p.Plan.Proxy.Image,
		Argv:    []string{"proxy", "--addr", fmt.Sprintf(":%d", browser.ProxyPort), "--browse-public"},
		Network: r.network,
		Alias:   renderProxyAlias,
		Labels:  labels,
	}
	proxySpec = readOnlyRoot(hardened(proxySpec, p.limits()), "/tmp")
	td.add(func(c context.Context) { _ = d.RemoveForce(c, r.proxy) })
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start the page renderer's proxy: %w", err))
	}
	// Its route out, as an input source's proxy's: the daemon's default
	// bridge. The renderer itself never joins it.
	if err := d.NetworkConnect(ctx, "bridge", r.proxy); err != nil {
		return Classify(ExitGateway, fmt.Errorf("connect the page renderer's proxy to the bridge: %w", err))
	}
	if r.runtime != "" {
		// A sandboxed runtime's network stack does not reach Docker's DNS:
		// the renderer is given the proxy's address.
		ip, err := d.ContainerIP(ctx, r.proxy, r.network)
		if err != nil {
			return Classify(ExitGateway, fmt.Errorf("the page renderer's proxy address: %w", err))
		}
		r.proxyIP = ip
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.stop, r.done = cancel, make(chan struct{})
	go r.watch(wctx, d, p, rd, runID)
	rd.render = r
	td.add(func(c context.Context) { r.close(c, d, rd, env) })
	env.CapabilitiesEnforced["web.render.image"] = r.image + ", pulled when this machine lacked it"
	return nil
}

// close stops the watcher, waiting no longer than a render's timeout
// (a render in flight ends at its own), keeps the proxy's decisions as a
// receipt and puts the count in the receipt. Once: the run calls it before
// the receipt is written, and teardown again in case the run ended early.
func (r *renderRun) close(ctx context.Context, d dockerx.Client, rd *runDirs, env *receipt.Envelope) {
	r.closed.Do(func() {
		r.stop()
		select {
		case <-r.done:
		case <-ctx.Done():
		case <-time.After(toolsrv.RenderTimeout + 10*time.Second):
		}
		collectRenderProxyLog(ctx, d, r, rd)
		env.CapabilitiesEnforced["web.render.pages"] = r.line()
	})
}

// line is the receipt's count of the run's renders.
func (r *renderRun) line() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := fmt.Sprintf("%d page(s) rendered, %d render(s) that failed, at most %d a run", r.pages, r.failed, maxRenders)
	if r.pages+r.failed > 0 {
		line += "; the proxy's decisions are kept in receipts/render-proxy.log"
	}
	return line
}

// watch answers the tools server's requests, renderSlots at a time, until
// the run ends.
func (r *renderRun) watch(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, runID string) {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(r.done)
	}()
	root, err := os.OpenRoot(rd.renderDir())
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	slots := make(chan struct{}, renderSlots)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		entries, _ := fs.ReadDir(root.FS(), ".")
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), toolsrv.RenderRequestExt)
			if !ok || !e.Type().IsRegular() || !renderIDPattern.MatchString(id) || r.busy(id) ||
				exists(root, id+toolsrv.RenderPageExt) || exists(root, id+toolsrv.RenderFailExt) {
				continue
			}
			select {
			case slots <- struct{}{}:
			default:
				continue // every slot is busy: next tick
			}
			r.mark(id, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots; r.mark(id, false) }()
				r.answer(ctx, d, p, root, id, runID)
			}()
		}
	}
}

func (r *renderRun) busy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[id]
}

func (r *renderRun) mark(id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight == nil {
		r.inflight = map[string]bool{}
	}
	if on {
		r.inflight[id] = true
	} else {
		delete(r.inflight, id)
	}
}

// spent says whether the run has used its renders.
func (r *renderRun) spent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pages+r.failed >= maxRenders
}

// answer renders one request and leaves its answer, whole.
func (r *renderRun) answer(ctx context.Context, d dockerx.Client, p Params, root *os.Root, id, runID string) {
	raw, err := readRootFile(root, id+toolsrv.RenderRequestExt, 4096)
	var req toolsrv.RenderRequest
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	var u *url.URL
	if err == nil {
		u, err = renderTarget(req.URL)
	}
	if err != nil {
		r.fail(root, id, "the request is not a page web_read reaches")
		return
	}
	if r.spent() {
		r.fail(root, id, fmt.Sprintf("this run's %d renders are spent", maxRenders))
		return
	}
	out, why := r.render(ctx, d, p, u.String(), runID, id)
	if !exists(root, id+toolsrv.RenderRequestExt) {
		// The tools server gave up on it: no answer is left behind.
		return
	}
	if why != "" {
		r.fail(root, id, why)
		return
	}
	if err := writeRootWhole(root, id+toolsrv.RenderPageExt, out); err != nil {
		r.fail(root, id, "the rendered page could not be handed back")
		return
	}
	r.mu.Lock()
	r.pages++
	r.mu.Unlock()
}

func (r *renderRun) fail(root *os.Root, id, why string) {
	_ = writeRootWhole(root, id+toolsrv.RenderFailExt, []byte(why+"\n"))
	r.mu.Lock()
	r.failed++
	r.mu.Unlock()
}

// renderTarget is a request's address, held as web_read holds one: http or
// https, ports 80 and 443, no credentials. Where it resolves is the
// proxy's to hold.
func renderTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("not an http or https address")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return nil, errors.New("not on port 80 or 443")
	}
	return u, nil
}

// render runs the one-shot browser on u and returns its output: the final
// address, the status and the HTML.
func (r *renderRun) render(ctx context.Context, d dockerx.Client, p Params, u, runID, id string) ([]byte, string) {
	name := containerName(runID, "render-"+id)
	_ = d.RemoveForce(context.WithoutCancel(ctx), name)
	uid, gid, ok := strings.Cut(r.user, ":")
	if !ok {
		gid = uid // a rootless engine's "0"
	}
	proxy := fmt.Sprintf("http://%s:%d", renderProxyAlias, browser.ProxyPort)
	s := dockerx.RunSpec{
		Name:       name,
		Image:      r.image,
		Entrypoint: "/usr/local/bin/node",
		Argv:       []string{"-e", renderScript, "--", u, proxy},
		Network:    r.network,
		Env:        map[string]string{"HOME": browser.Home},
		User:       r.user,
		Runtime:    r.runtime,
		AutoRemove: true,
		Labels:     runLabels(runID),
	}
	if r.proxyIP != "" {
		s.ExtraHosts = []string{renderProxyAlias + ":" + r.proxyIP}
	}
	s = readOnlyRoot(hardened(s, p.limits()),
		browser.Home+":"+browserHomeTmpfs+",uid="+uid+",gid="+gid,
		"/tmp:"+browserTmpTmpfs)
	s.SeccompProfile = r.seccomp
	rctx, cancel := context.WithTimeout(ctx, toolsrv.RenderTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	code, err := d.RunForeground(rctx, s, nil, &limitedBuffer{b: &out, n: renderOutputCap}, &limitedBuffer{b: &errOut, n: 4096})
	_ = d.RemoveForce(context.WithoutCancel(ctx), name)
	switch {
	case rctx.Err() != nil:
		return nil, "the page did not render in time"
	case err != nil || code != 0:
		return nil, "the browser could not open the page"
	case out.Len() == 0:
		return nil, "the browser drew nothing"
	}
	// A page that answered with an error, the proxy's own refusal
	// included, is not read: the plain read stands.
	if code := renderStatus(out.Bytes()); code < 200 || code > 399 {
		return nil, fmt.Sprintf("the page answered with status %d", code)
	}
	return out.Bytes(), ""
}

// renderStatus is the status line of a render's output, 0 when it has none.
func renderStatus(out []byte) int {
	_, rest, _ := bytes.Cut(out, []byte("\n"))
	line, _, _ := bytes.Cut(rest, []byte("\n"))
	code, err := strconv.Atoi(strings.TrimSpace(string(line)))
	if err != nil {
		return 0
	}
	return code
}

// renderOutputCap bounds what one render hands back: the tools server reads
// a page's first 2 MiB, as it does a fetched one.
const renderOutputCap = 2<<20 + 4096

// collectRenderProxyLog keeps the render proxy's decisions as a receipt.
func collectRenderProxyLog(ctx context.Context, d dockerx.Client, r *renderRun, rd *runDirs) {
	logs, err := d.Logs(ctx, r.proxy, 2000)
	if err != nil || logs == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(rd.receiptsDir, "render-proxy.log"), []byte(logs), 0o644) //nolint:gosec // receipt
}

func exists(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil
}

// readRootFile reads a regular file beneath root, at most n bytes. It opens
// without blocking and checks what it opened, not the name: the folder is
// the tools server's to write too, and a name swapped for a pipe between a
// check and the open must never hold the engine.
func readRootFile(root *os.Root, name string, n int64) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, n))
}

// writeRootWhole writes name beneath root so that it appears whole: a new
// temporary file (never one the folder already holds, which the tools
// server's side could have made a link), then a rename.
func writeRootWhole(root *os.Root, name string, b []byte) error {
	tmp := name + ".engine-tmp"
	_ = root.Remove(tmp)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
