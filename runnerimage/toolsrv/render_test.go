package toolsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// fakeRenderer answers the render folder's requests as the engine does:
// with answer(url) as the page (final address, status, HTML), or with a
// failure when answer returns "".
func fakeRenderer(t *testing.T, dir string, answer func(url string) string) *atomic.Int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n := &atomic.Int32{}
	go func() {
		for ctx.Err() == nil {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				id, ok := strings.CutSuffix(e.Name(), RenderRequestExt)
				if !ok {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, id+RenderPageExt)); err == nil {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, id+RenderFailExt)); err == nil {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				var req RenderRequest
				_ = json.Unmarshal(raw, &req)
				n.Add(1)
				if out := answer(req.URL); out != "" {
					_ = writeWhole(filepath.Join(dir, id+RenderPageExt), []byte(out))
				} else {
					_ = writeWhole(filepath.Join(dir, id+RenderFailExt), []byte("the browser could not open the page\n"))
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return n
}

// shellPage is a page whose text is drawn by a script: its plain read is
// thin.
const shellPage = `<html><head><title>App</title><script src="/app.js"></script></head><body><div id="root"></div></body></html>`

// TestAThinPageIsReadAsTheBrowserDrawsIt: a fetched HTML page whose text is
// thin is rendered through the engine, and the rendered text is what AI
// Review reads and the model gets; the ledger marks it rendered. A page with
// text of its own is not rendered unless the model asks.
func TestAThinPageIsReadAsTheBrowserDrawsIt(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/full" {
			_, _ = w.Write([]byte("<html><body><p>" + strings.Repeat("Replication keeps a standby in step. ", 60) + "</p></body></html>"))
			return
		}
		_, _ = w.Write([]byte(shellPage))
	}))
	defer site.Close()
	sc := &fakeScanner{verdict: passAll}
	s, spec := webServer(t, sc, map[string]*httptest.Server{"site.test": site})
	s.spec.Web.Render = t.TempDir()
	renders := fakeRenderer(t, s.spec.Web.Render, func(u string) string {
		return u + "\n200\n<html><body><h1>Streaming replication</h1><p>A standby replays the primary's WAL.</p></body></html>"
	})
	text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/app"}))
	if isErr || !strings.Contains(text, "A standby replays the primary's WAL.") || !strings.Contains(text, "rendered by a headless browser") {
		t.Fatalf("a thin page: %q", text)
	}
	calls := sc.calls()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "A standby replays") {
		t.Fatalf("AI Review read %+v, want the rendered text once", calls)
	}
	if text, isErr = s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/full"})); isErr || strings.Contains(text, "rendered") || renders.Load() != 1 {
		t.Fatalf("a page with text of its own was rendered (%d renders): %q", renders.Load(), text)
	}
	if _, isErr = s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/full", "render": true})); isErr || renders.Load() != 2 {
		t.Fatalf("render: true did not render (%d renders)", renders.Load())
	}
	var rendered int
	for _, e := range ledgerOf(t, spec) {
		if e.Rendered {
			rendered++
		}
	}
	if rendered != 2 {
		t.Fatalf("the ledger marks %d reads rendered, want 2", rendered)
	}
	entries, _ := os.ReadDir(s.spec.Web.Render)
	if len(entries) != 0 {
		t.Fatalf("the render folder keeps %d files after the reads", len(entries))
	}
}

// TestARenderThatFailsKeepsThePlainRead: when the engine cannot render, the
// plain read stands, and the ledger says why no render was used.
func TestARenderThatFailsKeepsThePlainRead(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shellPage))
	}))
	defer site.Close()
	s, spec := webServer(t, &fakeScanner{verdict: passAll}, map[string]*httptest.Server{"site.test": site})
	s.spec.Web.Render = t.TempDir()
	fakeRenderer(t, s.spec.Web.Render, func(string) string { return "" })
	if text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/app"})); isErr || strings.Contains(text, "rendered by") || !strings.Contains(text, "[not rendered: the browser could not render it") {
		t.Fatalf("a failed render: %q", text)
	}
	led := ledgerOf(t, spec)
	if len(led) != 1 || led[0].Rendered || !strings.Contains(led[0].Render, "could not open the page") {
		t.Fatalf("ledger %+v", led)
	}
}

// TestARenderedPageThatLeftTheSiteIsNotRead: in a run that reads only what
// its searches returned, a page that took the browser to another site, one
// no search returned, is refused and its text never scanned; a redirect
// within the site (to www.) is read.
func TestARenderedPageThatLeftTheSiteIsNotRead(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shellPage))
	}))
	defer site.Close()
	sc := &fakeScanner{verdict: passAll}
	s, _ := webServer(t, sc, map[string]*httptest.Server{"site.test": site})
	s.spec.Web.SearchedOnly, s.spec.Web.Search = true, "fixed"
	s.webState.searcher = fixedSearch{hits: []webscan.Hit{
		{URL: "http://site.test/away", Title: "Away", Result: passAll},
		{URL: "http://site.test/home", Title: "Home", Result: passAll},
		{URL: "http://site.test/tenant", Title: "Tenant", Result: passAll},
	}}
	s.spec.Web.Render = t.TempDir()
	fakeRenderer(t, s.spec.Web.Render, func(u string) string {
		switch {
		case strings.HasSuffix(u, "/away"):
			return "https://elsewhere.test/collect?d=x\n200\n<html><body><p>elsewhere</p></body></html>"
		case strings.HasSuffix(u, "/tenant"):
			return "https://attacker.site.test/\n200\n<html><body><p>elsewhere</p></body></html>"
		}
		return "http://www.site.test/home\n200\n<html><body><p>home text</p></body></html>"
	})
	if _, isErr := s.toolWebSearch(rawArgs(t, map[string]any{"query": "site"})); isErr {
		t.Fatal("search failed")
	}
	for _, target := range []string{"http://site.test/away", "http://site.test/tenant"} {
		if text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": target})); !isErr || !strings.Contains(text, failRenderAway) {
			t.Fatalf("%s left the site: %q", target, text)
		}
	}
	for _, c := range sc.calls() {
		if strings.Contains(c.text, "elsewhere") {
			t.Fatal("the other site's text was scanned")
		}
	}
	if text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/home"})); isErr || !strings.Contains(text, "home text") {
		t.Fatalf("a redirect within the site: %q", text)
	}
}

// TestWebReadOffersRenderOnlyWhereTheRunRenders: the render argument is in
// web_read's schema only for a run that renders.
func TestWebReadOffersRenderOnlyWhereTheRunRenders(t *testing.T) {
	if _, ok := webReadSchemaFor(false)["properties"].(map[string]any)["render"]; ok {
		t.Fatal("render offered without the renderer")
	}
	if _, ok := webReadSchemaFor(true)["properties"].(map[string]any)["render"]; !ok {
		t.Fatal("render not offered with the renderer")
	}
}

// TestAGivenPageThatRendersAwayIsNotRead: in a run held to the addresses
// it was given, a rendered page that took the browser to another site is
// refused and its text never scanned, and one that stays on its site is
// read.
func TestAGivenPageThatRendersAwayIsNotRead(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shellPage))
	}))
	defer site.Close()
	sc := &fakeScanner{verdict: passAll}
	s, _ := webServer(t, sc, map[string]*httptest.Server{"site.test": site})
	s.spec.Web.GivenOnly, s.spec.Web.Given = true, []string{"http://site.test/away", "http://site.test/post"}
	s.spec.Web.Render = t.TempDir()
	fakeRenderer(t, s.spec.Web.Render, func(u string) string {
		if strings.HasSuffix(u, "/away") {
			return "https://elsewhere.test/collect\n200\n<html><body><p>elsewhere</p></body></html>"
		}
		return "http://www.site.test/post\n200\n<html><body><p>post text</p></body></html>"
	})
	if text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/away"})); !isErr || !strings.Contains(text, failRenderAway) {
		t.Fatalf("a page that left its site: %q", text)
	}
	for _, c := range sc.calls() {
		if strings.Contains(c.text, "elsewhere") {
			t.Fatal("the other site's text was scanned")
		}
	}
	if text, isErr := s.toolWebRead(rawArgs(t, map[string]any{"url": "http://site.test/post"})); isErr || !strings.Contains(text, "post text") {
		t.Fatalf("a page that stayed on its site: %q", text)
	}
}
