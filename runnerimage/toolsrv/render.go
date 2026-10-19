package toolsrv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// A rendered read. Where the run renders pages (Web.Render), web_read asks
// the engine to open a page in a headless browser when the page's own text
// is thin (most of it is drawn by scripts) or the model asks. The tools
// server and the engine share one folder, and nothing else: the tools
// server leaves <id>.json naming the address, the engine answers with
// <id>.page (the address the browser ended on, its status, then the HTML it
// drew) or <id>.fail (why it could not), each made whole before it appears.
// The tools server reads the answer as it reads a fetched page: HTMLText,
// the address check, AI Review, the ledger. The browser is the engine's, on
// a network whose one way out admits public addresses alone; the model
// never drives it.

// RenderTextMin is the text a fetched HTML page must hold, trimmed, for
// web_read to take it without rendering it.
const RenderTextMin = 1024

// RenderTimeout bounds one render, the browser's start included.
const RenderTimeout = 60 * time.Second

// The hand-off's file names, by request id.
const (
	RenderRequestExt = ".json"
	RenderPageExt    = ".page"
	RenderFailExt    = ".fail"
)

// RenderRequest is a request's file.
type RenderRequest struct {
	URL string `json:"url"`
}

// failRenderAway is web_read's answer when the rendered page ended on
// another site, one this run's searches did not return.
const failRenderAway = "the page sent the browser to another site, which this run's searches did not return"

// notRenderedNote ends a plain read whose render was asked for and not
// used, so the model knows the text may be the page's shell.
func notRenderedNote(why string) string {
	return "\n\n[not rendered: " + why + "; this is the page's own text]"
}

var renderSeq atomic.Int64

// renderPage asks the engine to render u and reads its answer as a page.
// why is set, and the page empty, when no rendered page came back.
func (s *server) renderPage(u *url.URL) (pg page, why string) {
	dir := s.spec.Web.Render
	id := fmt.Sprintf("r%d-%d", os.Getpid(), renderSeq.Add(1))
	req, err := json.Marshal(RenderRequest{URL: u.String()})
	if err != nil {
		return page{}, "the render request could not be written"
	}
	if err := writeWhole(filepath.Join(dir, id+RenderRequestExt), req); err != nil {
		return page{}, "the render request could not be written"
	}
	pagePath, failPath := filepath.Join(dir, id+RenderPageExt), filepath.Join(dir, id+RenderFailExt)
	defer func() {
		for _, p := range []string{filepath.Join(dir, id+RenderRequestExt), pagePath, failPath} {
			_ = os.Remove(p)
		}
	}()
	// The engine renders two pages at a time, so a request may wait for one
	// render before its own.
	deadline := time.Now().Add(2*RenderTimeout + 5*time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(failPath); err == nil { //nolint:gosec // the run's own hand-off folder
			return page{}, "the browser could not render it: " + firstLine(string(b), 200)
		}
		if f, err := os.Open(pagePath); err == nil { //nolint:gosec // the run's own hand-off folder
			raw, rerr := io.ReadAll(io.LimitReader(f, maxPageRaw+4096))
			_ = f.Close()
			if rerr != nil {
				return page{}, "the rendered page could not be read"
			}
			return s.renderedPage(raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return page{}, "the browser did not render it in time"
}

// renderedPage reads the engine's answer: the final address, the status,
// then the HTML.
func (s *server) renderedPage(raw []byte) (page, string) {
	finalLine, rest, ok1 := bytes.Cut(raw, []byte("\n"))
	statusLine, html, ok2 := bytes.Cut(rest, []byte("\n"))
	if !ok1 || !ok2 {
		return page{}, "the rendered page could not be read"
	}
	final, err := url.Parse(strings.TrimSpace(string(finalLine)))
	if err != nil || checkPageURL(final) != nil {
		return page{}, "the browser ended on an address web_read does not reach"
	}
	text := HTMLText(html, final)
	status := strings.TrimSpace(string(statusLine)) + " (rendered by a headless browser)"
	answer, err := s.pageAnswer(final.String(), status, "text/html", text)
	if err != nil {
		return page{}, "the rendered page could not be read"
	}
	return page{answer: answer, final: final.String(), html: true, textLen: len(strings.TrimSpace(text))}, ""
}

// sameSite says whether a rendered page may be read where the browser
// ended, in a run that reads only what its searches returned: on the asked
// page's own host, with or without "www." (example.com to www.example.com),
// or at an address a search returned. Any other host, a sibling or a parent
// on a shared platform included, is another site.
func (s *server) sameSite(asked *url.URL, final string) bool {
	if !s.spec.Web.SearchedOnly {
		return true
	}
	f, err := url.Parse(final)
	if err != nil {
		return false
	}
	bare := func(h string) string { return strings.TrimPrefix(strings.ToLower(h), "www.") }
	if bare(asked.Hostname()) == bare(f.Hostname()) {
		return true
	}
	return s.web().wasFound(f)
}

// hostOf is an address's origin alone, for the ledger.
func hostOf(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// writeWhole writes a file so that it appears whole: a temporary name,
// then a rename.
func writeWhole(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // a request the engine reads
		return err
	}
	return os.Rename(tmp, path)
}

// firstLine is s's first line, at most n bytes.
func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > n {
		s = s[:n]
	}
	return s
}
