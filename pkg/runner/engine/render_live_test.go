package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The live renderer test. Everything is real: the runner image built here
// (its proxy role), the pinned browser image, the render network and its
// proxy, Chromium under the browser's seccomp profile. It asks for a public
// page whose text a script writes, and for a non-public address, which the
// proxy refuses. It runs only when asked: AGENTRY_LIVE_RENDER=1 on a host
// with docker and network.
func TestLiveRenderDrawsAScriptWrittenPage(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_RENDER") != "1" {
		t.Skip("set AGENTRY_LIVE_RENDER=1 to run the live renderer test")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	work := t.TempDir()
	pol := policy.Default()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(ctx, d, pol, runnerTag, filepath.Join(work, "img"), os.Stderr, false); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(work, "cfg")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("live%d", time.Now().UnixNano()%1_000_000_000)
	p := Params{
		Plan: &spec.Plan{
			Capabilities: spec.CapSummary{WebTools: true, WebRender: true},
			Render:       &spec.RenderSpec{Image: pol.Images.BrowserImage()},
			Proxy:        spec.ProxySpec{Image: runnerTag},
		},
		Policy: pol,
		Stderr: os.Stderr,
	}
	rd := &runDirs{cfg: cfg, receiptsDir: work}
	td := &teardown{d: d, stderr: os.Stderr, runID: runID}
	env := &receipt.Envelope{CapabilitiesEnforced: map[string]string{}}
	if err := prepareRender(ctx, d, p, rd, td, runID, env); err != nil {
		td.run()
		t.Fatal(err)
	}
	defer td.run()

	ask := func(id, u string) (string, string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rd.renderDir(), id+toolsrv.RenderRequestExt), []byte(`{"url":"`+u+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(toolsrv.RenderTimeout + 10*time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(filepath.Join(rd.renderDir(), id+toolsrv.RenderPageExt)); err == nil {
				return string(b), ""
			}
			if b, err := os.ReadFile(filepath.Join(rd.renderDir(), id+toolsrv.RenderFailExt)); err == nil {
				return "", string(b)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("%s: no answer", u)
		return "", ""
	}
	page, fail := ask("r1", "https://quotes.toscrape.com/js/")
	if fail != "" || !strings.HasPrefix(page, "https://quotes.toscrape.com/js/\n200\n") || !strings.Contains(page, "Albert Einstein") {
		t.Fatalf("the script-written page: fail %q, page %.300q", fail, page)
	}
	if _, fail = ask("r2", "http://169.254.169.254/latest/meta-data/"); fail == "" {
		t.Fatal("the metadata address was rendered")
	}
	td.run()
	logs, _ := os.ReadFile(filepath.Join(work, "render-proxy.log"))
	if !strings.Contains(string(logs), "169.254.169.254") {
		t.Errorf("the proxy log does not show the refusal:\n%s", logs)
	}
	if line := env.CapabilitiesEnforced["web.render.pages"]; !strings.Contains(line, "1 page(s) rendered, 1 render(s) that failed") {
		t.Errorf("receipt line %q", line)
	}
	if names, _ := d.PSByLabel(context.WithoutCancel(ctx), runLabel+"="+runID); len(names) != 0 {
		t.Errorf("left behind: %v", names)
	}
}
