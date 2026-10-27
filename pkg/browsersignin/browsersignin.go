// Package browsersignin lets a person sign in to a web site in a browser
// window and hands back the cookies that browser would send to one
// address: how a calendar feed whose address answers only a signed-in
// browser (a Google Workspace calendar, which redirects to Google's sign-in
// page) is read.
//
// The browser is a Chromium-family one installed on this computer (Chrome,
// Edge, Chromium or Brave), started in a fresh profile of its own that is
// removed afterwards, so nothing of the person's everyday browser is read
// or changed. It is driven over a pipe the process inherits (Chrome's
// --remote-debugging-pipe, or --remote-debugging-io-pipes on Windows),
// never a debugging port another program could reach, and its downloads
// are denied, so an address that answers with a file does not leave the
// file on disk. While the window is open the browser's cookies for the
// address are read every PollEvery; each new set is handed to the caller's
// Ready, which says whether the address now answers as wanted. The first
// set Ready accepts is returned and the browser is closed.
package browsersignin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// PollEvery is how often the browser's cookies are read.
var PollEvery = 2 * time.Second

// Cookie is one cookie as the browser keeps it.
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
	Secure bool   `json:"secure"`
	// PartitionKey is set on a cookie kept for one top-level site alone
	// (CHIPS); such a cookie is never sent to a plain fetch of the address.
	PartitionKey any `json:"partitionKey,omitempty"`
}

// Options are what Run opens and how it knows the sign-in is done.
type Options struct {
	// URL is the https address the window opens, and the one whose cookies
	// are read.
	URL string
	// Ready says whether the address answers as wanted when fetched with
	// the Cookie header given. An error stops the sign-in; false keeps the
	// window open.
	Ready func(ctx context.Context, header string) (bool, error)
	// Browser is the browser's program; empty finds one (Find).
	Browser string
	// Progress, when set, is told what the sign-in is waiting for.
	Progress func(string)
}

// ErrClosed says the person closed the browser window before the sign-in
// was done.
var ErrClosed = errors.New("the browser window was closed before the sign-in was done")

// Run opens the browser at o.URL and waits, until ctx ends, for a set of
// cookies o.Ready accepts, which it returns: those the browser sends to
// o.URL (Match).
func Run(ctx context.Context, o Options) ([]Cookie, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, errors.New("a browser sign-in opens an https address alone")
	}
	if o.Ready == nil {
		return nil, errors.New("a browser sign-in needs a check of the address")
	}
	program := o.Browser
	if program == "" {
		if program, err = Find(); err != nil {
			return nil, err
		}
	}
	profile, err := os.MkdirTemp("", "agentry-sign-in-")
	if err != nil {
		return nil, fmt.Errorf("make the browser's profile: %w", err)
	}
	defer removeProfile(profile)

	b, err := start(program, flags(profile))
	if err != nil {
		return nil, err
	}
	defer b.close()

	if _, err := b.call(ctx, "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}); err != nil {
		return nil, fmt.Errorf("the browser did not answer: %w", err)
	}
	if err := b.open(ctx, o.URL); err != nil {
		return nil, fmt.Errorf("the browser did not open the address: %w", err)
	}
	progress(o, "Sign in in the browser window that opened. It closes by itself once the calendar can be read.")

	tick := time.NewTicker(PollEvery)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the sign-in was not done in time: %w", ctx.Err())
		case <-b.exited:
			return nil, ErrClosed
		case <-tick.C:
		}
		open, err := b.pageOpen(ctx)
		if err != nil {
			if b.hasExited() {
				return nil, ErrClosed
			}
			return nil, fmt.Errorf("the browser did not answer: %w", err)
		}
		if !open {
			return nil, ErrClosed
		}
		cookies, err := b.cookies(ctx)
		if err != nil {
			return nil, fmt.Errorf("the browser did not answer: %w", err)
		}
		match := Match(cookies, u)
		header := Header(match)
		if header == "" || header == last {
			continue
		}
		last = header
		ok, err := o.Ready(ctx, header)
		if err != nil {
			return nil, err
		}
		if ok {
			_, _ = b.call(ctx, "Browser.close", nil)
			return match, nil
		}
	}
}

func progress(o Options, s string) {
	if o.Progress != nil {
		o.Progress(s)
	}
}

// removeProfile removes the browser's profile, trying again for a few
// seconds while the closing browser still holds its files.
func removeProfile(dir string) {
	for i := 0; i < 20; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Match are the cookies a browser would send with a request for u: those
// whose domain and path match it, a secure one only over https, none
// partitioned and none a Cookie header could not carry, longer paths first.
func Match(cookies []Cookie, u *url.URL) []Cookie {
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	var match []Cookie
	for _, c := range cookies {
		if c.Name == "" || c.PartitionKey != nil || (c.Secure && u.Scheme != "https") {
			continue
		}
		if !domainMatch(host, c.Domain) || !pathMatch(path, c.Path) {
			continue
		}
		if (&http.Cookie{Name: c.Name, Value: c.Value}).Valid() != nil {
			continue
		}
		match = append(match, c)
	}
	sort.SliceStable(match, func(i, j int) bool { return len(match[i].Path) > len(match[j].Path) })
	return match
}

// Header is the Cookie header that sends cookies.
func Header(cookies []Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// domainMatch says a cookie kept for domain is sent to host: a domain with
// a leading dot covers itself and every host under it, one without covers
// that host alone.
func domainMatch(host, domain string) bool {
	domain = strings.ToLower(domain)
	if d, ok := strings.CutPrefix(domain, "."); ok {
		return host == d || strings.HasSuffix(host, "."+d)
	}
	return host == domain
}

// pathMatch is RFC 6265's path-match.
func pathMatch(reqPath, cookiePath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if reqPath == cookiePath {
		return true
	}
	if strings.HasPrefix(reqPath, cookiePath) {
		return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
	}
	return false
}
