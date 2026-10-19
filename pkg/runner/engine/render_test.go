package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
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
	for _, bad := range []string{"file:///etc/passwd", "https://user:pw@example.com/", "https://example.com:8443/", "javascript:alert(1)", "", "chrome://settings"} {
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
}

// TestTheRendererClosesOnceBeforeTheReceipt: closing the renderer stops its
// watcher and puts its count in the receipt; the run closes it before the
// receipt is written and teardown again, which changes nothing.
func TestTheRendererClosesOnceBeforeTheReceipt(t *testing.T) {
	stopped := 0
	r := &renderRun{proxy: "agentry-run1-render-proxy", done: make(chan struct{}), pages: 2, failed: 1}
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
	f := &dockerx.Fake{}
	r.close(context.Background(), f, rd, env)
	if got := env.CapabilitiesEnforced["web.render.pages"]; !strings.HasPrefix(got, "2 page(s) rendered, 1 render(s) that failed") {
		t.Fatalf("receipt line %q", got)
	}
	r.pages = 5
	r.close(context.Background(), f, rd, env)
	if stopped != 1 || !strings.HasPrefix(env.CapabilitiesEnforced["web.render.pages"], "2 page(s)") {
		t.Fatalf("a second close stopped %d times and wrote %q", stopped, env.CapabilitiesEnforced["web.render.pages"])
	}
}
