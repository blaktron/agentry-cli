// Package browsersignin lets a person sign in to a web site in a browser
// window and hands back the cookies that browser would send to one
// address: how a calendar feed whose address answers only a signed-in
// browser (a Google Workspace calendar, which redirects to Google's sign-in
// page) is read.
//
// The browser is a Chromium-family one installed on this computer (Chrome,
// Edge, Chromium or Brave), started in a fresh profile of its own that is
// removed afterwards, so nothing of the person's everyday browser is read
// or changed. It is started twice.
//
// The window the person signs in is an ordinary browser that nothing
// controls. A browser driven over DevTools is marked as automated
// (navigator.webdriver is true in every page), and a sign-in page such as
// Google's refuses it. The window opens a page written in the profile that
// sends it on to the address, so the address is on no command line, and
// the profile's download folder is inside the profile, so an address that
// answers with a file leaves it nowhere else. The window ends when the
// person closes it, or, once a download lands in that folder (the address
// answered with its file), when it is asked to quit.
//
// The same profile is then opened again, headless, its last session
// restored so that cookies kept until the browser closes are read too. It
// is driven over a pipe the process inherits (Chrome's
// --remote-debugging-pipe, or --remote-debugging-io-pipes on Windows),
// never a debugging port another program could reach, with its downloads
// denied. The browser's cookies for the address are read and the browser
// closed, and the caller's Ready says whether the address answers as wanted
// with them.
package browsersignin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PollEvery is how often the window's download folder is looked at;
// QuitWait is how long a window asked to quit has before it is killed;
// ReadyTries is how many times Ready is asked, ReadyEvery apart, so a fetch
// that failed for a moment does not lose the sign-in.
var (
	PollEvery  = time.Second
	QuitWait   = 15 * time.Second
	ReadyTries = 3
	ReadyEvery = 2 * time.Second
)

// profilePrefix begins a sign-in profile's folder name; staleProfile is how
// old one is when a run that was killed left it behind.
const (
	profilePrefix = "agentry-sign-in-"
	staleProfile  = time.Hour
)

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
	// the Cookie header given. An error stops the sign-in at once; false
	// ends it with ErrClosed once Ready has been asked ReadyTries times.
	Ready func(ctx context.Context, header string) (bool, error)
	// Browser is the browser's program; empty finds one (Find).
	Browser string
	// Progress, when set, is told what the sign-in is waiting for.
	Progress func(string)
}

// ErrClosed says the browser window ended without a sign-in that Ready
// accepts.
var ErrClosed = errors.New("the browser window was closed before the sign-in was done")

// ErrNotQuit says the window, asked to quit once the address's download
// landed, had to be killed, and no sign-in was found in what it left.
var ErrNotQuit = errors.New("the browser did not quit when asked, so its sign-in could not be read: sign in again, and close the window yourself once signed in")

// notInTime is the error for a sign-in that ctx ended.
func notInTime(ctx context.Context) error {
	return fmt.Errorf("the sign-in was not done in time: %w", ctx.Err())
}

// Run opens a browser window at o.URL and waits, until ctx ends, for it to
// close; then it returns the cookies the browser sends to o.URL (Match),
// once o.Ready accepts them.
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
	removeStaleProfiles()
	profile, err := os.MkdirTemp("", profilePrefix)
	if err != nil {
		return nil, fmt.Errorf("make the browser's profile: %w", err)
	}
	defer removeProfile(profile)

	opener, err := prepareProfile(profile, o.URL)
	if err != nil {
		return nil, err
	}
	w, err := startWindow(program, append(flags(profile), "--new-window", opener))
	if err != nil {
		return nil, err
	}
	progress(o, signInHint)
	killed, err := w.wait(ctx, filepath.Join(profile, downloadsDir))
	if err != nil {
		return nil, err
	}

	progress(o, "Reading the sign-in from the browser.")
	cookies, err := readCookies(ctx, program, profile)
	if err != nil {
		return nil, err
	}
	match := Match(cookies, u)
	header := Header(match)
	if header == "" {
		if killed {
			return nil, ErrNotQuit
		}
		return nil, ErrClosed
	}
	for try := 0; try < ReadyTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, notInTime(ctx)
			case <-time.After(ReadyEvery):
			}
		}
		ok, err := o.Ready(ctx, header)
		if err != nil {
			return nil, err
		}
		if ok {
			return match, nil
		}
	}
	return nil, ErrClosed
}

// readCookies opens the profile again, headless and over the DevTools
// pipe, and returns every cookie it holds. The last session is restored, or
// the cookies kept only until the browser closed would be gone.
func readCookies(ctx context.Context, program, profile string) ([]Cookie, error) {
	b, err := start(program, append(flags(profile), "--headless=new", "--restore-last-session", "about:blank"))
	if err != nil {
		return nil, err
	}
	defer b.close()
	if _, err := b.call(ctx, "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}); err != nil {
		return nil, readErr(ctx, err)
	}
	cookies, err := b.cookies(ctx)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	return cookies, nil
}

// readErr is the error for a read the browser did not answer, or that ctx
// ended.
func readErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return notInTime(ctx)
	}
	return fmt.Errorf("the browser did not answer: %w", err)
}

func progress(o Options, s string) {
	if o.Progress != nil {
		o.Progress(s)
	}
}

// removeStaleProfiles removes sign-in profiles a run that was killed left
// in the temporary folder, each holding the cookies of a sign-in.
func removeStaleProfiles() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), profilePrefix+"*"))
	for _, d := range dirs {
		if st, err := os.Lstat(d); err == nil && st.IsDir() && time.Since(st.ModTime()) > staleProfile {
			_ = os.RemoveAll(d)
		}
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
