// The built-in Drafter's web tools: web_search, web_read and registry_search.
// The compiler grants them to the Drafter alone and refuses them to any
// release (compile.Inputs.Web); their contract is not the manifest's, which
// declares no egress.
//
// Every text they bring back is read by AI Review before the model sees it
// (runnerimage/webscan): a page, each search result and each registry
// answer. One that is flagged, or that could not be scanned, is withheld:
// the model gets its address and the reason, never the text. Each
// result's verdict goes on stderr, which the gateway keeps as gateway.log,
// and into the run's web ledger, which the engine counts into the receipt.
//
// The tools container these run in has no network allowlist of its own
// (the gateway gives it the daemon's default network), so web_read holds
// its own bounds, on a laptop as on a sandbox host: GET only; http and https
// on ports 80 and 443; public addresses only, checked on the address each
// connection is actually made to, so a name that resolves somewhere private
// and every redirect hop are refused alike; no cookie and no credential; at
// most 32 KiB of text a page.

package toolsrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry-cli/runnerimage/websearch"
)

// Web configures the web tools. Enabled is compiled (the Drafter's grant);
// everything below it is the run's, stamped by the engine into the run's own
// copy of this file, since one plan serves a local and a hosted draft alike.
type Web struct {
	Enabled bool `json:"enabled"`
	// RegistrySearch is compiled too: registry_search, the Drafter's alone,
	// granted only when Enabled and with a Registry to
	// read.
	RegistrySearch bool `json:"registrySearch,omitempty"`
	// SearchedOnly is compiled: web_read opens only an address a web_search
	// of this run returned and AI Review passed (the Knowledge Creator's).
	// Its model reads documents no one scanned, so an
	// instruction hidden in one must not be able to send another's text out
	// in an address of its choosing; a search result's address is the
	// search provider's, never the model's.
	SearchedOnly bool `json:"searchedOnly,omitempty"`
	// MaxPages, MaxSearches and MaxRegistryCalls bound one run's calls;
	// zero takes the defaults.
	MaxPages         int `json:"maxPages,omitempty"`
	MaxSearches      int `json:"maxSearches,omitempty"`
	MaxRegistryCalls int `json:"maxRegistryCalls,omitempty"`
	// Scan is where texts are scanned: WebScanRegistry (the registry's
	// route, with the job's credential in EnvWebJobToken) or WebScanJev
	// (the person's own key in EnvWebJevKey). Anything else scans nothing,
	// so every result is withheld.
	Scan string `json:"scan,omitempty"`
	// Search is where web_search goes: WebScanRegistry, a provider name
	// the person holds a key for (EnvWebSearchKey), or empty, when
	// web_search is not granted.
	Search string `json:"search,omitempty"`
	// Registry is the registry's root, for registry_search and, hosted,
	// the scan and search routes.
	Registry string `json:"registry,omitempty"`
	// Ledger is the container path the web ledger is appended to, one JSON
	// line a result.
	Ledger string `json:"ledger,omitempty"`
}

// Where a scan or a search goes.
const (
	WebScanRegistry = "registry"
	WebScanJev      = "jev"
)

// The environment variables the gateway injects into this container alone,
// from the run's secrets file. None of them reaches the agent container.
const (
	EnvWebJobToken  = "AGENTRY_WEB_JOB_TOKEN"
	EnvWebJevKey    = "AGENTRY_WEB_JEV_KEY"
	EnvWebSearchKey = "AGENTRY_WEB_SEARCH_KEY"
)

// WebSecretEnv is every key the web tools may be given, sorted.
func WebSecretEnv() []string { return []string{EnvWebJevKey, EnvWebJobToken, EnvWebSearchKey} }

// The web tools' bounds.
const (
	// PageTextBytes is the most text a page gives the model: four of the
	// judge's 8 KiB chunks.
	PageTextBytes = 32 << 10
	// DefaultMaxPages, DefaultMaxSearches and DefaultMaxRegistryCalls are
	// a run's calls (60 pages, 30 searches to start with).
	DefaultMaxPages         = 60
	DefaultMaxSearches      = 30
	DefaultMaxRegistryCalls = 60
	// maxPageRaw is the most of a response read before its text is
	// extracted.
	maxPageRaw = 2 << 20
	// maxRedirects bounds a page's redirect chain.
	maxRedirects = 5
	// webReadTimeout bounds one page, redirects included.
	webReadTimeout = 30 * time.Second
	// scanTimeout bounds one text's scan.
	scanTimeout = 90 * time.Second
	// registryTimeout bounds one registry request.
	registryTimeout = 20 * time.Second
	// userAgent names what is reading.
	userAgent = "AgentryDrafter/1 (+https://secureagentry.net)"
)

// LedgerEntry is one line of the web ledger: one text a web tool read, or
// one call it refused before reading anything.
type LedgerEntry struct {
	Tool   string         `json:"tool"`
	Source webscan.Source `json:"source,omitempty"`
	// URL is the page's final address, a search result's address (its
	// origin alone when it did not pass), or the registry request.
	URL string `json:"url,omitempty"`
	// Bytes is the length of the text scanned.
	Bytes int `json:"bytes,omitempty"`
	// Refused says why a call read nothing (an address, a budget, a type),
	// in the words the model was given; Detail is the error behind it,
	// bounded, for the person.
	Refused string `json:"refused,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Search marks a web search that answered, with Results results; each
	// result has a line of its own after it.
	Search  bool `json:"search,omitempty"`
	Results int  `json:"results,omitempty"`
	webscan.Result
}

// webState is the web tools' per-run state.
type webState struct {
	mu       sync.Mutex
	pages    int
	searches int
	registry int
	scanner  webscan.Scanner
	searcher webscan.Searcher
	// allow decides whether a connection may be made to an address; tests
	// widen it to reach a loopback server.
	allow func(netip.AddrPort) error
	// mapAddr, when set, rewrites the address a page connection dials
	// before allow sees it (tests: a public name standing for a loopback
	// server).
	mapAddr    func(string) string
	pageClient *http.Client
	regClient  *http.Client
	ledger     *os.File
	// found is every address a web_search of this run returned that AI
	// Review passed, as web_read compares them (pageKey): what a
	// SearchedOnly run may read.
	found map[string]bool
}

// pageKey is an address as web_read and the searches' results are compared:
// parsed, its fragment dropped.
func pageKey(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

// remember records a passed search result's address for a SearchedOnly run.
func (w *webState) remember(raw string) {
	k, ok := pageKey(raw)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.found == nil {
		w.found = map[string]bool{}
	}
	w.found[k] = true
}

// wasFound reports whether a search of this run returned the address.
func (w *webState) wasFound(u *url.URL) bool {
	k, ok := pageKey(u.String())
	w.mu.Lock()
	defer w.mu.Unlock()
	return ok && w.found[k]
}

// failNotSearched is web_read's answer, in a SearchedOnly run, for an
// address no search of the run returned.
const failNotSearched = "this run reads only the addresses its web searches returned: search first, then read a result by its address as it was given"

func (w Web) maxPages() int {
	if w.MaxPages > 0 {
		return w.MaxPages
	}
	return DefaultMaxPages
}

func (w Web) maxSearches() int {
	if w.MaxSearches > 0 {
		return w.MaxSearches
	}
	return DefaultMaxSearches
}

func (w Web) maxRegistryCalls() int {
	if w.MaxRegistryCalls > 0 {
		return w.MaxRegistryCalls
	}
	return DefaultMaxRegistryCalls
}

// newWebState builds the scanner and the searcher the spec names from the
// keys in this container's environment. A scanner that cannot be built is
// none, and every text is then withheld as not scanned: the tools never
// return an unscanned text.
func (s *server) newWebState() *webState {
	w := &webState{allow: publicAddrPort}
	w.pageClient = w.newPageClient()
	w.regClient = &http.Client{Timeout: registryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	web := s.spec.Web
	switch web.Scan {
	case WebScanRegistry:
		if tok := os.Getenv(EnvWebJobToken); tok != "" && web.Registry != "" {
			w.scanner = &webscan.Registry{Base: web.Registry, JobToken: tok}
		}
	case WebScanJev:
		if key := os.Getenv(EnvWebJevKey); key != "" {
			if j, err := webscan.NewJev(key, "", nil); err == nil {
				w.scanner = j
			}
		}
	}
	if w.scanner == nil {
		_, _ = fmt.Fprintf(s.logw, "web: no AI Review scanner (scan %q): every web result will be withheld\n", web.Scan)
		return w
	}
	switch web.Search {
	case "":
	case WebScanRegistry:
		if reg, ok := w.scanner.(*webscan.Registry); ok {
			w.searcher = webscan.RegistrySearch{Registry: reg}
		}
	default:
		if key := os.Getenv(EnvWebSearchKey); key != "" {
			if p, err := websearch.New(web.Search, "", key); err == nil && p != nil {
				w.searcher = webscan.ProviderSearch{Provider: p, Scanner: w.scanner}
			}
		}
	}
	return w
}

// web returns the run's web state, built on first use.
func (s *server) web() *webState {
	s.webOnce.Do(func() {
		if s.webState == nil {
			s.webState = s.newWebState()
		}
	})
	return s.webState
}

// ---- the address rule ----

// errNotPublic is the refusal of an address that is not public.
var errNotPublic = errors.New("not a public address")

// publicAddrPort is the address rule: a public address, on port 80 or 443.
func publicAddrPort(ap netip.AddrPort) error {
	if p := ap.Port(); p != 80 && p != 443 {
		return fmt.Errorf("port %d is not 80 or 443", p)
	}
	return publicAddr(ap.Addr())
}

// publicAddr is the runner's one public-address rule (netaddr, shared
// with the input-source and browse proxies).
func publicAddr(a netip.Addr) error {
	if !netaddr.Public(a) {
		return fmt.Errorf("%s is %w", a.Unmap(), errNotPublic)
	}
	return nil
}

// newPageClient is web_read's client: no proxy from the environment, no
// cookie jar, every connection checked against allow at the address it is
// made to, every redirect held to the URL rule.
func (w *webState) newPageClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("refused %s: %w", address, err)
			}
			if err := w.allow(ap); err != nil {
				return &addressRefused{fmt.Errorf("refused %s: %w", address, err)}
			}
			return nil
		},
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if w.mapAddr != nil {
			address = w.mapAddr(address)
		}
		return dialer.DialContext(ctx, network, address)
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		// A server's headers are its own text; far more of them than a
		// page needs is refused, not read.
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           8,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   webReadTimeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return &redirectRefused{err: fmt.Errorf("stopped after %d redirects", maxRedirects), many: true}
			}
			if err := checkPageURL(r.URL); err != nil {
				return &redirectRefused{err: fmt.Errorf("redirect to %s refused: %w", r.URL.Redacted(), err)}
			}
			// No header the first request did not carry is added on a
			// hop; Go carries no Cookie or Authorization here because the
			// first request has none.
			return nil
		},
	}
}

// checkPageURL is the rule a URL is held to before any connection: http or
// https, a host, no user information, port 80 or 443 when one is named, and
// an address literal only when it is public. The dialer holds a resolved
// name to the same address rule.
func checkPageURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.User != nil {
		return errors.New("an address with user information is refused")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("no host")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return fmt.Errorf("port %s is not 80 or 443", p)
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%s is %w", host, errNotPublic)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if err := publicAddr(a); err != nil {
			return err
		}
	}
	return nil
}

// ---- the ledger and the log ----

// record writes one result to stderr (the gateway's log) and the ledger.
func (s *server) record(e LedgerEntry) {
	switch {
	case e.Refused != "":
		_, _ = fmt.Fprintf(s.logw, "web: %s refused %q: %s %q\n", e.Tool, e.URL, e.Refused, e.Detail)
	case e.Search:
		_, _ = fmt.Fprintf(s.logw, "web: %s answered %d result(s)\n", e.Tool, e.Results)
	default:
		line := fmt.Sprintf("web: %s %s %s score=%d", e.Tool, e.Source, e.Verdict, e.Score)
		if len(e.Flagged) > 0 {
			line += " flagged=" + strings.Join(e.Flagged, ",")
		}
		if e.Failure != "" {
			line += " failure=" + e.Failure
		}
		_, _ = fmt.Fprintf(s.logw, "%s bytes=%d %q\n", line, e.Bytes, e.URL)
	}
	w := s.web()
	if s.spec.Web.Ledger == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ledger == nil {
		if err := os.MkdirAll(filepath.Dir(s.spec.Web.Ledger), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(s.spec.Web.Ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
		if err != nil {
			_, _ = fmt.Fprintf(s.logw, "web: open ledger: %v\n", err)
			return
		}
		s.own(s.spec.Web.Ledger)
		w.ledger = f
	}
	_, _ = w.ledger.Write(append(b, '\n'))
}

// scan scans one text, bounded in time.
func (s *server) scan(source webscan.Source, text string) webscan.Result {
	w := s.web()
	if w.scanner == nil {
		return webscan.Result{Verdict: webscan.NotScanned, Failure: webscan.FailureUnavailable}
	}
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	return w.scanner.Scan(ctx, source, text)
}

// take spends one of a budget, or says it is spent.
func (w *webState) take(counter *int, limit int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if *counter >= limit {
		return false
	}
	*counter++
	return true
}

// withheldText is what the model gets of a text that did not pass.
func withheldText(what, address string, r webscan.Result) string {
	return fmt.Sprintf("withheld: %s was not given to you, because %s.\nURL: %s\nDo not use it, and do not guess at what it said; where your sources are listed, list it as withheld.",
		what, r.Reason(), address)
}

// ---- web_read ----

func webReadSchema() map[string]any {
	return objSchema(map[string]any{
		"url": strProp("The http or https address of a public page to read (GET only, ports 80 and 443)."),
	}, "url")
}

func (s *server) toolWebRead(raw json.RawMessage) (string, bool) {
	var a struct {
		URL string `json:"url"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "web_read: " + err.Error(), true
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || a.URL == "" {
		return "web_read: url is required and must be an http or https address", true
	}
	u.Fragment = ""
	if err := checkPageURL(u); err != nil {
		s.record(LedgerEntry{Tool: ToolWebRead, URL: u.Redacted(), Refused: err.Error()})
		return "web_read: refused: " + err.Error(), true
	}
	w := s.web()
	if s.spec.Web.SearchedOnly && !w.wasFound(u) {
		// The address is the model's own, and is not sent anywhere: the
		// ledger keeps its origin alone.
		s.record(LedgerEntry{Tool: ToolWebRead, URL: u.Scheme + "://" + u.Host, Refused: failNotSearched})
		return "web_read: refused: " + failNotSearched, true
	}
	if !w.take(&w.pages, s.spec.Web.maxPages()) {
		s.record(LedgerEntry{Tool: ToolWebRead, URL: u.String(), Refused: "the run's page budget is spent"})
		return fmt.Sprintf("web_read: this run's %d pages are spent; finish with what you have", s.spec.Web.maxPages()), true
	}
	text, final, err := s.fetchPage(u)
	if err != nil {
		// What failed is said to the model in the tools' own words only:
		// a server's error, header or redirect target is its own text,
		// and none of it has been scanned. The ledger keeps a bounded
		// copy of the detail for the person.
		word := pageFailure(err)
		s.record(LedgerEntry{Tool: ToolWebRead, URL: u.String(), Refused: word, Detail: detail(err)})
		return "web_read: " + word, true
	}
	res := s.scan(webscan.SourcePage, text)
	s.record(LedgerEntry{Tool: ToolWebRead, Source: webscan.SourcePage, URL: final, Bytes: len(text), Result: res})
	if !res.Passed() {
		// The address the model asked for: a redirect's own target was
		// part of the text that did not pass.
		return withheldText("this page", u.String(), res), false
	}
	return s.scrubSecrets(text), false
}

// The ways a page read fails, as the model is told them.
const (
	failRefused   = "the page's address is not a public address on port 80 or 443"
	failRedirect  = "the page redirected to an address web_read does not reach"
	failRedirects = "the page redirected too many times"
	failTimeout   = "the page did not answer in time"
	failName      = "the page's host name did not resolve"
	failTLS       = "the page's TLS connection could not be verified"
	failNotText   = "the page is not text: web_read reads HTML, text, JSON, XML and YAML pages only"
	failRead      = "the page could not be read"
)

// addressRefused is the dialer's refusal of an address; redirectRefused the
// redirect check's; notText a page of another type.
type (
	addressRefused  struct{ err error }
	redirectRefused struct {
		err  error
		many bool
	}
	notText struct{ mediaType string }
)

func (e *addressRefused) Error() string  { return e.err.Error() }
func (e *redirectRefused) Error() string { return e.err.Error() }
func (e *notText) Error() string         { return "not text: " + e.mediaType }

// pageFailure is a failed read in closed words.
func pageFailure(err error) string {
	var (
		ar   *addressRefused
		rr   *redirectRefused
		nt   *notText
		dns  *net.DNSError
		ne   net.Error
		cert *tls.CertificateVerificationError
		ua   x509.UnknownAuthorityError
		he   x509.HostnameError
		ci   x509.CertificateInvalidError
		rh   tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &rr) && rr.many:
		return failRedirects
	case errors.As(err, &rr):
		return failRedirect
	case errors.As(err, &ar):
		return failRefused
	case errors.As(err, &nt):
		return failNotText
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return failTimeout
	case errors.As(err, &dns):
		return failName
	case errors.As(err, &cert), errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ci), errors.As(err, &rh):
		return failTLS
	}
	return failRead
}

// detail is an error's text for the ledger: valid UTF-8, one line, at most
// 300 bytes. It never reaches the model.
func detail(err error) string {
	d := strings.ToValidUTF8(err.Error(), "?")
	d = strings.Join(strings.Fields(d), " ")
	d, _ = cutText(d, 300)
	return d
}

// fetchPage GETs u and returns what the model would read of it (the
// address, the status and the page's text, at most PageTextBytes of it) and
// the final address.
func (s *server) fetchPage(u *url.URL) (string, string, error) {
	w := s.web()
	ctx, cancel := context.WithTimeout(context.Background(), webReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html, application/xhtml+xml, text/plain, text/markdown, application/json, application/xml;q=0.9, text/*;q=0.8")
	resp, err := w.pageClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", u.Redacted(), unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	final := resp.Request.URL.String()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	kind := textKind(mt)
	if kind == "" {
		return "", "", &notText{mediaType: mt}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageRaw))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", final, unwrapURLError(err))
	}
	var text string
	if kind == "html" {
		text = HTMLText(body, resp.Request.URL)
	} else {
		text = strings.ToValidUTF8(string(body), "\uFFFD")
	}
	// The whole answer, its head and the cut note included, is at most
	// PageTextBytes: four of the judge's chunks, and within the run's
	// result budget (page.go), so that the harness never cuts what AI
	// Review read.
	head := fmt.Sprintf("URL: %s\nStatus: %s\nContent-Type: %s\n\n", final, resp.Status, mt)
	const noteRoom = 64
	room := PageTextBytes - len(head) - noteRoom
	if room < 0 {
		return "", "", errors.New("the page's address and status alone fill a page")
	}
	full := len(text)
	text, cut := cutText(text, room)
	if b := s.spec.Limits.budget().less(head).room(); !b.fits(text) {
		text, cut = string(b.cut([]byte(text), false)), true
	}
	out := head + text
	if cut {
		out += fmt.Sprintf("\n\n[cut at %d of %d bytes of text]", len(text), full)
	}
	return out, final, nil
}

// textKind is how a media type is read: "html", "text", or "" for a type
// that is not text.
func textKind(mt string) string {
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return "html"
	case strings.HasPrefix(mt, "text/"), mt == "application/json", mt == "application/xml", mt == "application/yaml",
		mt == "application/x-yaml", mt == "application/toml", strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return "text"
	}
	return ""
}

// cutText cuts s to at most n bytes at a character boundary.
func cutText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s[len(s)-min(len(s), utf8.UTFMax):]) {
		s = s[:len(s)-1]
	}
	return s, true
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ---- web_search ----

func webSearchSchema() map[string]any {
	return objSchema(map[string]any{
		"query": strProp("What to search the web for, 1 to 400 characters."),
		"count": intProp("How many results, 1 to 10 (default 5)."),
	}, "query")
}

func (s *server) toolWebSearch(raw json.RawMessage) (string, bool) {
	var a struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "web_search: " + err.Error(), true
	}
	q, err := websearch.CleanQuery(a.Query)
	if err != nil {
		return "web_search: " + err.Error(), true
	}
	w := s.web()
	if w.searcher == nil {
		return "web_search: no search is available to this run", true
	}
	if !w.take(&w.searches, s.spec.Web.maxSearches()) {
		s.record(LedgerEntry{Tool: ToolWebSearch, Refused: "the run's search budget is spent"})
		return fmt.Sprintf("web_search: this run's %d searches are spent; finish with what you have", s.spec.Web.maxSearches()), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	hits, err := w.searcher.Search(ctx, q, websearch.ClampCount(a.Count))
	if err != nil {
		s.record(LedgerEntry{Tool: ToolWebSearch, Refused: "the search did not answer"})
		return "web_search: the search did not answer; try again, or read a page you already know", true
	}
	s.record(LedgerEntry{Tool: ToolWebSearch, Search: true, Results: len(hits)})
	var b strings.Builder
	shown, held := 0, 0
	for _, h := range hits {
		s.record(LedgerEntry{Tool: ToolWebSearch, Source: webscan.SourceSearchResult, URL: h.URL, Result: h.Result})
		if !h.Passed() {
			held++
			fmt.Fprintf(&b, "- withheld: a result from %s, because %s\n", h.URL, h.Reason())
			continue
		}
		shown++
		w.remember(h.URL)
		fmt.Fprintf(&b, "- %s\n  %s\n  %s\n", h.Title, h.URL, h.Snippet)
	}
	head := fmt.Sprintf("%d result(s) for %q", shown, q)
	if held > 0 {
		head += fmt.Sprintf(", %d withheld by AI Review (only their origin is shown)", held)
	}
	return s.scrubSecrets(head + "\n" + b.String()), false
}

// ---- registry_search ----

func registrySearchSchema() map[string]any {
	return objSchema(map[string]any{
		"query":     strProp("Search the registry's published QuickAgents for this text."),
		"ref":       strProp("Resolve one QuickAgent (ns/name, ns/name@1.2.3 or ns/name@^1) and read its manifest."),
		"ecosystem": strProp("With package and version: an MCP server's ecosystem, npm or pypi."),
		"package":   strProp("With ecosystem and version: the MCP server's package name."),
		"version":   strProp("With ecosystem and package: the exact version, to read the registry's vulnerability check of it."),
	})
}

func (s *server) toolRegistrySearch(raw json.RawMessage) (string, bool) {
	var a struct {
		Query     string `json:"query"`
		Ref       string `json:"ref"`
		Ecosystem string `json:"ecosystem"`
		Package   string `json:"package"`
		Version   string `json:"version"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "registry_search: " + err.Error(), true
	}
	var path string
	q := url.Values{}
	switch {
	case a.Query != "" && a.Ref == "" && a.Package == "":
		path = "/api/v1/search"
		q.Set("q", a.Query)
		q.Set("limit", "10")
	case a.Ref != "" && a.Query == "" && a.Package == "":
		path = "/api/v1/resolve"
		q.Set("ref", a.Ref)
	case a.Package != "" && a.Query == "" && a.Ref == "":
		if a.Ecosystem != "npm" && a.Ecosystem != "pypi" {
			return "registry_search: ecosystem must be npm or pypi", true
		}
		if a.Version == "" {
			return "registry_search: an MCP server's check needs its exact version", true
		}
		path = "/api/v1/mcp-servers/scan"
		q.Set("ecosystem", a.Ecosystem)
		q.Set("package", a.Package)
		q.Set("version", a.Version)
	default:
		return "registry_search: give one of query, ref, or ecosystem with package and version", true
	}
	w := s.web()
	if s.spec.Web.Registry == "" {
		return "registry_search: no registry is configured for this run", true
	}
	target := strings.TrimRight(s.spec.Web.Registry, "/") + path + "?" + q.Encode()
	if !w.take(&w.registry, s.spec.Web.maxRegistryCalls()) {
		s.record(LedgerEntry{Tool: ToolRegistrySearch, URL: path, Refused: "the run's registry budget is spent"})
		return fmt.Sprintf("registry_search: this run's %d registry calls are spent", s.spec.Web.maxRegistryCalls()), true
	}
	status, body, err := s.registryGet(target)
	if err != nil {
		s.record(LedgerEntry{Tool: ToolRegistrySearch, URL: path, Refused: "the registry did not answer"})
		return "registry_search: the registry did not answer", true
	}
	text := renderRegistry(path, status, body, a.Ref, a.Ecosystem+":"+a.Package+"@"+a.Version)
	text, _ = cutText(text, PageTextBytes)
	res := s.scan(webscan.SourceRegistry, text)
	s.record(LedgerEntry{Tool: ToolRegistrySearch, Source: webscan.SourceRegistry, URL: path + "?" + q.Encode(), Bytes: len(text), Result: res})
	if !res.Passed() {
		return withheldText("the registry's answer", path+"?"+q.Encode(), res), false
	}
	return text, false
}

// registryGet reads one public registry route: no credential, no redirect.
func (s *server) registryGet(target string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.web().regClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// renderRegistry turns a registry answer into the text the model reads (and
// AI Review reads first). The house voice holds here as in the CLI: a server
// with no known advisories is said to have none known when last checked,
// never to be safe.
func renderRegistry(path string, status int, body []byte, ref, server string) string {
	if status == http.StatusNotFound {
		switch path {
		case "/api/v1/resolve":
			return fmt.Sprintf("No published QuickAgent resolves from %q.", ref)
		case "/api/v1/mcp-servers/scan":
			return fmt.Sprintf("The registry has no check of %s: no published manifest has declared that version yet.", server)
		}
	}
	if status != http.StatusOK {
		return fmt.Sprintf("The registry answered %d.", status)
	}
	var b strings.Builder
	switch path {
	case "/api/v1/search":
		var r struct {
			Results []struct {
				Namespace   string   `json:"namespace"`
				Name        string   `json:"name"`
				Modes       []string `json:"modes"`
				Summary     string   `json:"summary"`
				Installs30d int      `json:"installs30d"`
			} `json:"results"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return "The registry's answer was not the search's shape."
		}
		fmt.Fprintf(&b, "%d published QuickAgent(s) match (of %d):\n", len(r.Results), r.Total)
		for _, h := range r.Results {
			fmt.Fprintf(&b, "- %s/%s (%s; %d installs in 30 days): %s\n", h.Namespace, h.Name, strings.Join(h.Modes, ", "), h.Installs30d, h.Summary)
		}
	case "/api/v1/resolve":
		var r struct {
			Namespace  string          `json:"namespace"`
			Name       string          `json:"name"`
			Version    string          `json:"version"`
			Yanked     bool            `json:"yanked"`
			ScanStatus string          `json:"scanStatus"`
			Manifest   json.RawMessage `json:"manifest"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return "The registry's answer was not the resolve's shape."
		}
		fmt.Fprintf(&b, "%s/%s@%s", r.Namespace, r.Name, r.Version)
		if r.Yanked {
			b.WriteString(" (yanked)")
		}
		if r.ScanStatus == "skipped" {
			b.WriteString(" (published without a scan)")
		}
		b.WriteString("\nIts manifest:\n")
		b.Write(r.Manifest)
		b.WriteByte('\n')
	case "/api/v1/mcp-servers/scan":
		var r struct {
			Ecosystem   string `json:"ecosystem"`
			Package     string `json:"package"`
			Version     string `json:"version"`
			Status      string `json:"status"`
			MaxSeverity string `json:"maxSeverity"`
			Advisories  []struct {
				ID       string   `json:"id"`
				Severity string   `json:"severity"`
				Summary  string   `json:"summary"`
				FixedIn  []string `json:"fixedIn"`
			} `json:"advisories"`
			LastCheckedAt string `json:"lastCheckedAt"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return "The registry's answer was not the check's shape."
		}
		id := r.Ecosystem + ":" + r.Package + "@" + r.Version
		switch r.Status {
		case "safe":
			fmt.Fprintf(&b, "%s: no known advisories when the registry last checked (%s).\n", id, r.LastCheckedAt)
		case "vulnerable":
			fmt.Fprintf(&b, "%s: %d known advisory(ies), the worst %s, when the registry last checked (%s):\n", id, len(r.Advisories), r.MaxSeverity, r.LastCheckedAt)
		default:
			fmt.Fprintf(&b, "%s: the registry's last check failed (%s); the advisories last known:\n", id, r.LastCheckedAt)
		}
		for _, adv := range r.Advisories {
			fmt.Fprintf(&b, "- %s (%s): %s", adv.ID, adv.Severity, adv.Summary)
			if len(adv.FixedIn) > 0 {
				fmt.Fprintf(&b, "; fixed in %s", strings.Join(adv.FixedIn, ", "))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// webKeysForScrub are the key variables every web tool's answer is scrubbed
// of, as fetch's are of its granted secrets.
func webKeysForScrub(w Web) []string {
	if !w.Enabled {
		return nil
	}
	return WebSecretEnv()
}
