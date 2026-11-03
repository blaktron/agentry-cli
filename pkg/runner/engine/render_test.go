package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestARenderRequestIsHeldAsWebReadHoldsOne: only an http or https address
// on port 80 or 443, with no credentials, is rendered.
func TestARenderRequestIsHeldAsWebReadHoldsOne(t *testing.T) {
	for _, ok := range []string{"https://example.com/docs", "http://example.com:80/", "https://example.com:443/a?b=c"} {
		if _, err := renderTarget(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for id, want := range map[string]bool{"r1": true, "a-B-9": true, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "": false, "a_b": false, "a.b": false, "a b": false} {
		if got := renderIDPattern.MatchString(id); got != want {
			t.Errorf("render id %q: %v, want %v", id, got, want)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "https://user:pw@example.com/", "https://example.com:8443/", "javascript:alert(1)", "", "chrome://settings", "https:///path", "http://example.com:8080/", "https://example.com:81/", "ftp://example.com/"} {
		if _, err := renderTarget(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestTheEngineNeverWritesThroughALink: the hand-off folder is the tools
// server's to write too, so a link it leaves where the engine writes an
// answer is never followed: the answer is written to a file of its own and
// renamed over the link, and the link's target is untouched.
func TestTheEngineNeverWritesThroughALink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"r1.page.engine-tmp", "r1.page"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := writeRootWhole(root, "r1.page", []byte("answer")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "unchanged" {
		t.Fatalf("the link's target was written: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "r1.page")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the answer is not a regular file: %v %v", fi, err)
	}
	if _, err := readRootFile(root, "missing", 10); err == nil {
		t.Fatal("a missing file was read")
	}
	if err := os.Symlink(target, filepath.Join(dir, "r2.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRootFile(root, "r2.json", 10); err == nil {
		t.Fatal("a request that is a link was read")
	}
}

// TestTheRendererAnswersARequestFromItsOwnContainer: a request is answered
// by a one-shot container of the browser image held as the browser server
// is (no capability, no new privileges, a read-only root, the browser's
// seccomp profile, the person's user) on the render network alone, given
// the address and the proxy and nothing of the run's; its output is the
// answer. A container that fails leaves a failure.
func TestTheRendererAnswersARequestFromItsOwnContainer(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var seen dockerx.RunSpec
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-render": {Before: func(r dockerx.RunSpec) { seen = r }, Print: func(r dockerx.RunSpec) string {
			return r.Argv[3] + "\n200\n<html><body>drawn</body></html>"
		}},
	}}
	r := &renderRun{image: "agentry-browser:test", network: "agentry-run1-render", proxy: "agentry-run1-render-proxy", seccomp: "/cfg/render-seccomp.json", user: "1000:1000"}
	if err := os.WriteFile(filepath.Join(dir, "r1"+toolsrv.RenderRequestExt), []byte(`{"url":"https://example.com/app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.answer(context.Background(), f, Params{}, root, "r1", "run1")
	b, err := os.ReadFile(filepath.Join(dir, "r1"+toolsrv.RenderPageExt))
	if err != nil || !strings.HasPrefix(string(b), "https://example.com/app\n200\n") {
		t.Fatalf("answer %q %v", b, err)
	}
	switch {
	case seen.Image != "agentry-browser:test" || seen.Network != "agentry-run1-render" || len(seen.ExtraNetworks) != 0:
		t.Fatalf("image %s network %s %v", seen.Image, seen.Network, seen.ExtraNetworks)
	case !slices.Equal(seen.CapDrop, []string{"ALL"}) || !seen.NoNewPrivileges || !seen.ReadOnly || seen.SeccompProfile != "/cfg/render-seccomp.json" || seen.User != "1000:1000":
		t.Fatalf("posture %+v", seen)
	case len(seen.Mounts) != 0 || len(seen.Volumes) != 0 || len(seen.Env) != 1:
		t.Fatalf("the renderer is given the run's mounts %v %v or env %v", seen.Mounts, seen.Volumes, seen.Env)
	case !strings.Contains(seen.Argv[1], "executablePath:process.env.PLAYWRIGHT_MCP_EXECUTABLE_PATH||undefined,"):
		t.Fatalf("the renderer does not launch the image's own Chromium: %s", seen.Argv[1])
	case seen.Argv[0] != "-e" || seen.Argv[2] != "--" || seen.Argv[3] != "https://example.com/app" || seen.Argv[4] != "http://"+renderProxyAlias+":3128":
		t.Fatalf("argv %q", seen.Argv[2:])
	}
	f.Foreground["-render"] = &dockerx.FakeForeground{Stdout: "http://169.254.169.254/\n403\n<html>refused by the proxy</html>"}
	if err := os.WriteFile(filepath.Join(dir, "r4"+toolsrv.RenderRequestExt), []byte(`{"url":"http://169.254.169.254/"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.answer(context.Background(), f, Params{}, root, "r4", "run1")
	if b, err := os.ReadFile(filepath.Join(dir, "r4"+toolsrv.RenderFailExt)); err != nil || !strings.Contains(string(b), "status 403") {
		t.Fatalf("an error page was handed back: %q %v", b, err)
	}
	f.Foreground["-render"] = &dockerx.FakeForeground{ExitCode: 2, Stderr: "net::ERR_TUNNEL_CONNECTION_FAILED"}
	if err := os.WriteFile(filepath.Join(dir, "r2"+toolsrv.RenderRequestExt), []byte(`{"url":"https://example.com/b"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.answer(context.Background(), f, Params{}, root, "r2", "run1")
	if b, err := os.ReadFile(filepath.Join(dir, "r2"+toolsrv.RenderFailExt)); err != nil || !strings.Contains(string(b), "could not open the page") {
		t.Fatalf("failure %q %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r3"+toolsrv.RenderRequestExt), []byte(`{"url":"file:///etc/passwd"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.answer(context.Background(), f, Params{}, root, "r3", "run1")
	if _, err := os.Stat(filepath.Join(dir, "r3"+toolsrv.RenderFailExt)); err != nil {
		t.Fatal("a request for a file was not refused")
	}
	if !strings.Contains(r.line(), "1 page(s) rendered, 3 render(s) that failed") {
		t.Fatalf("receipt line %q", r.line())
	}

	// A page answered with a status of 200 to 399 is handed back, any other
	// is not; a browser that draws nothing, a request cut short and one too
	// long to read are each a failure of their own.
	fresh := func() *renderRun {
		return &renderRun{image: "agentry-browser:test", network: "n", proxy: "p", user: "1000:1000"}
	}
	answerWith := func(ctx context.Context, r *renderRun, id, request string, fg *dockerx.FakeForeground) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+toolsrv.RenderRequestExt), []byte(request), 0o644); err != nil {
			t.Fatal(err)
		}
		f.Foreground["-render"] = fg
		r.answer(ctx, f, Params{}, root, id, "run1")
		if b, err := os.ReadFile(filepath.Join(dir, id+toolsrv.RenderPageExt)); err == nil {
			return "page " + string(b)
		}
		b, _ := os.ReadFile(filepath.Join(dir, id+toolsrv.RenderFailExt))
		return "fail " + strings.TrimSpace(string(b))
	}
	for status, ok := range map[string]bool{"199": false, "200": true, "399": true, "400": false} {
		got := answerWith(context.Background(), fresh(), "s"+status, `{"url":"https://example.com/"}`, &dockerx.FakeForeground{Stdout: "https://example.com/\n" + status + "\n<html></html>"})
		if ok != strings.HasPrefix(got, "page ") || !ok && got != "fail the page answered with status "+status {
			t.Errorf("status %s: %q", status, got)
		}
	}
	if got := answerWith(context.Background(), fresh(), "blank", `{"url":"https://example.com/"}`, &dockerx.FakeForeground{}); got != "fail the browser drew nothing" {
		t.Errorf("a browser that drew nothing: %q", got)
	}
	if got := answerWith(context.Background(), fresh(), "long", `{"url":"https://example.com/`+strings.Repeat("a", 5000)+`"}`, &dockerx.FakeForeground{Stdout: "u\n200\n<html></html>"}); got != "fail the request is not a page web_read reaches" {
		t.Errorf("a request too long to read: %q", got)
	}
	spent := fresh()
	spent.pages = maxRenders
	if got := answerWith(context.Background(), spent, "spent", `{"url":"https://example.com/"}`, &dockerx.FakeForeground{Stdout: "u\n200\n<html></html>"}); !strings.HasPrefix(got, "fail this run's ") || !strings.HasSuffix(got, " renders are spent") {
		t.Errorf("a run past its renders: %q", got)
	}
	last := fresh()
	last.pages = maxRenders - 1
	if got := answerWith(context.Background(), last, "last", `{"url":"https://example.com/"}`, &dockerx.FakeForeground{Stdout: "u\n200\n<html></html>"}); !strings.HasPrefix(got, "page ") {
		t.Errorf("a run's last render: %q", got)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if got := answerWith(cancelled, fresh(), "late", `{"url":"https://example.com/"}`, &dockerx.FakeForeground{Stdout: "u\n200\n<html></html>"}); got != "fail the page did not render in time" {
		t.Errorf("a render that ran out of time: %q", got)
	}
	// Under a runtime the renderer reaches its proxy by the address it was
	// given, and without one by name alone.
	pinned := fresh()
	pinned.proxyIP = "172.20.0.2"
	var saw dockerx.RunSpec
	answerWith(context.Background(), pinned, "pin", `{"url":"https://example.com/"}`, &dockerx.FakeForeground{Before: func(rs dockerx.RunSpec) { saw = rs }, Stdout: "u\n200\n<html></html>"})
	if !slices.Equal(saw.ExtraHosts, []string{renderProxyAlias + ":172.20.0.2"}) {
		t.Errorf("the renderer's extra hosts = %v", saw.ExtraHosts)
	}
	if len(seen.ExtraHosts) != 0 {
		t.Errorf("a renderer with no pinned address was given extra hosts %v", seen.ExtraHosts)
	}
}

// TestTheRendererClosesOnceBeforeTheReceipt: closing the renderer stops its
// watcher, puts its count in the receipt and removes its proxy and network;
// the run closes it before the receipt is written and teardown again, which
// changes nothing.
func TestTheRendererClosesOnceBeforeTheReceipt(t *testing.T) {
	stopped := 0
	r := &renderRun{proxy: "agentry-run1-render-proxy", network: "agentry-run1-render", done: make(chan struct{}), pages: 2, failed: 1}
	r.stop = func() {
		stopped++
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	}
	rd := &runDirs{receiptsDir: t.TempDir()}
	env := &receipt.Envelope{CapabilitiesEnforced: map[string]string{}}
	f := &dockerx.Fake{LogsByName: map[string]string{"agentry-run1-render-proxy": "CONNECT example.com:443 allowed\n"}}
	r.close(context.Background(), f, rd, env)
	if b, err := os.ReadFile(filepath.Join(rd.receiptsDir, "render-proxy.log")); err != nil || string(b) != "CONNECT example.com:443 allowed\n" {
		t.Fatalf("the proxy's log = %q, %v", b, err)
	}
	if got := env.CapabilitiesEnforced["web.render.pages"]; !strings.HasPrefix(got, "2 page(s) rendered, 1 render(s) that failed") {
		t.Fatalf("receipt line %q", got)
	}
	// The proxy and its network go as the renderer closes, not only in
	// teardown.
	var removed []string
	for _, c := range f.Calls {
		if c.Op == "RemoveForce" || c.Op == "NetworkRemove" {
			removed = append(removed, c.Op+" "+c.Arg)
		}
	}
	if strings.Join(removed, ",") != "RemoveForce agentry-run1-render-proxy,NetworkRemove agentry-run1-render" {
		t.Fatalf("on close: %v", removed)
	}
	r.pages = 5
	r.close(context.Background(), f, rd, env)
	if stopped != 1 || !strings.HasPrefix(env.CapabilitiesEnforced["web.render.pages"], "2 page(s)") {
		t.Fatalf("a second close stopped %d times and wrote %q", stopped, env.CapabilitiesEnforced["web.render.pages"])
	}
}

// TestTheRenderProxyReadsTheMachinesNameServersOnPodman: on podman serving
// this machine the render proxy, which may open any public host, reads this
// machine's upstream name servers (or the policy's docker.resolvers) in place
// of the internal network's, which answers no outside name; on Docker, or
// on podman in another machine, it keeps the engine's.
func TestTheRenderProxyReadsTheMachinesNameServersOnPodman(t *testing.T) {
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	was := lookupUpstreams
	lookupUpstreams = func() []string { return []string{"192.168.1.1"} }
	t.Cleanup(func() { lookupUpstreams = was })
	podman := func(host string) *dockerx.Engine {
		return &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Rootless: true, Endpoint: "unix:///run/user/1000/podman/podman.sock", DaemonHost: host, DaemonKernel: "7.1.13"}
	}
	plan := &spec.Plan{}
	named := &spec.Plan{Docker: &spec.DockerSpec{Resolvers: []string{"9.9.9.9"}}}
	for _, c := range []struct {
		name   string
		engine *dockerx.Engine
		plan   *spec.Plan
		want   []string
	}{
		{"docker", nil, plan, nil},
		{"podman here", podman("box"), plan, []string{"192.168.1.1"}},
		{"podman here, the policy's servers", podman("box"), named, []string{"9.9.9.9"}},
		{"podman in a VM", podman("podman-machine-default"), plan, nil},
	} {
		got := renderProxyResolvers(context.Background(), &dockerx.Fake{EngineInfo: c.engine}, Params{Plan: c.plan})
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if got := string(renderResolvConfBody([]string{"192.168.1.1", "1.1.1.1"})); !strings.HasSuffix(got, "\nnameserver 192.168.1.1\nnameserver 1.1.1.1\n") {
		t.Errorf("resolv.conf %q", got)
	}

	// The proxy reads the file in place of the engine's, and the renderer
	// itself is given nothing of it.
	prev := ensureBrowserImage
	ensureBrowserImage = func(context.Context, dockerx.Client, string) error { return nil }
	t.Cleanup(func() { ensureBrowserImage = prev })
	cfg := t.TempDir()
	f := &dockerx.Fake{EngineInfo: podman("box")}
	p := Params{Plan: &spec.Plan{
		Capabilities: spec.CapSummary{WebTools: true, WebRender: true},
		Render:       &spec.RenderSpec{Image: "agentry-browser:test"},
		Proxy:        spec.ProxySpec{Image: "agentry-runner:test"},
	}, Stderr: io.Discard}
	rd := &runDirs{cfg: cfg, receiptsDir: t.TempDir()}
	td := &teardown{d: f, stderr: io.Discard, runID: "run1"}
	env := &receipt.Envelope{CapabilitiesEnforced: map[string]string{}}
	if err := prepareRender(context.Background(), f, p, rd, td, "run1", env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(td.run)
	proxy, ok := f.FindRun("-render-proxy")
	if !ok || !strings.Contains(strings.Join(proxy.Argv, " "), "--browse-public") {
		t.Errorf("the render proxy's argv: %v (found %v)", proxy.Argv, ok)
	}
	var internal, bridged bool
	for _, c := range f.Calls {
		internal = internal || c.Op == "NetworkCreate" && c.Arg == "agentry-run1-render(internal=true)"
		bridged = bridged || c.Op == "NetworkConnect" && c.Arg == "bridge<-"+proxy.Name
	}
	if !internal || !bridged {
		t.Errorf("the render network is internal %v and its proxy on the bridge %v: %v", internal, bridged, f.Ops())
	}
	// A run on a sandbox host renders only when its host dispatched it as a
	// browser job.
	hosted := p
	hosted.Hosted = true
	err := prepareRender(context.Background(), &dockerx.Fake{EngineInfo: podman("box")}, hosted, &runDirs{cfg: t.TempDir(), receiptsDir: t.TempDir()}, &teardown{d: f, stderr: io.Discard, runID: "run2"}, "run2", &receipt.Envelope{CapabilitiesEnforced: map[string]string{}})
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "only when its host dispatched it as a browser job") {
		t.Errorf("a hosted run that was not dispatched as a browser job: %v", err)
	}
	conf := filepath.Join(cfg, renderResolvConf)
	if !ok || !slices.Contains(proxy.Mounts, dockerx.Mount{Source: conf, Target: "/etc/resolv.conf", ReadOnly: true, Relabel: true}) {
		t.Fatalf("the render proxy's mounts: %+v (found %v)", proxy.Mounts, ok)
	}
	if b, err := os.ReadFile(conf); err != nil || !strings.Contains(string(b), "nameserver 192.168.1.1\n") {
		t.Errorf("the proxy's resolv.conf: %q %v", b, err)
	}
}
