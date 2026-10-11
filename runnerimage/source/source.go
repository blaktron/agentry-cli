// Package source is the runner image's `source` role (plan input-sources.md
// §5, D6; agentry-cli#833): the fetch step that brings a run's input source
// in before the run, as plain files under one directory. The repository
// kind is a depth-1 fetch of one commit over git's smart HTTP protocol,
// spoken by this package itself (git.go, pack.go) rather than by a git
// binary or a third-party library: the image builds with no module download
// (DR-2), and no shell or git runs in the fetch container. The checkout
// writes regular files only (tree.go): no link, no submodule, no .git, LFS
// pointer files as they are.
//
// The caller runs this in the files container on a network whose only
// route is a CONNECT proxy to the source's host on port 443, with the
// access token, when there is one, on stdin: never in the environment or an
// argument. The token is sent to that host alone, as HTTP basic auth.
//
// This package is part of the embedded runner-image build context and so
// imports nothing outside the standard library.
package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// Schema is the report's format; ReportFile is where Fetch writes it, in
// the work directory, beside nothing the agent reads.
const (
	Schema     = "source/v1"
	ReportFile = "source.json"
	// KindRepository is the one kind this role fetches today.
	KindRepository = "repository"
)

// The default caps (D7): a local repository.
const (
	DefaultMaxFiles = 20000
	DefaultMaxBytes = 1 << 30
)

// maxSkippedListed bounds how many paths the report names per reason; the
// count is always whole.
const maxSkippedListed = 50

// Options is one fetch.
type Options struct {
	// Kind is the source kind; only KindRepository is fetched.
	Kind string
	// URL is the repository's https address, with no credentials.
	URL string
	// Ref is the branch, tag or full commit name; empty is the default
	// branch.
	Ref string
	// Token is the access token, empty for anonymous. A token holding a
	// colon is user:token; otherwise the user is x-access-token.
	Token string
	// Proxy is the CONNECT proxy every request goes through; nil is a
	// direct connection (tests only).
	Proxy *url.URL
	// Out is the empty directory the files are written to; Work the
	// directory the pack and the report are written to.
	Out, Work string
	// MaxFiles and MaxBytes are the caps; over either the fetch is
	// refused and nothing is written to Out.
	MaxFiles int
	MaxBytes int64
	// Transport replaces the HTTP transport (tests only).
	Transport http.RoundTripper
	// anyPort lets a test's server listen where it can.
	anyPort bool
}

// Report is what one fetch did.
type Report struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`
	// Ref is the ref resolved (a full ref name, or the commit asked for);
	// Commit the commit checked out.
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Files and Bytes are what was written.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// LFSPointers counts the files that are Git LFS pointers.
	LFSPointers int `json:"lfsPointers,omitempty"`
	// Skipped counts what was left out, by reason; SkippedPaths names the
	// first few of each.
	Skipped      map[string]int      `json:"skipped,omitempty"`
	SkippedPaths map[string][]string `json:"skippedPaths,omitempty"`
	// Refusal says why nothing was written, when a cap refused the source:
	// its count against the cap.
	Refusal string `json:"refusal,omitempty"`
	// Error says why the fetch failed, in words for the person.
	Error string `json:"error,omitempty"`
	// Protocol is the git protocol the host spoke.
	Protocol string `json:"protocol,omitempty"`
}

// Fetch fetches one source and writes its report to Work. The error is
// for a fetch that could not be made; a refusal is a report.
func Fetch(ctx context.Context, o Options) (Report, error) {
	rep := Report{Schema: Schema, Kind: o.Kind}
	err := fetch(ctx, o, &rep)
	if err != nil {
		rep.Error = printable(err.Error(), 600)
		if o.Token != "" {
			rep.Error = strings.ReplaceAll(rep.Error, o.Token, "[token]")
		}
	}
	if werr := writeReport(o.Work, rep); werr != nil && err == nil {
		err = werr
	}
	return rep, err
}

func fetch(ctx context.Context, o Options, rep *Report) error {
	if o.Kind != KindRepository {
		return fmt.Errorf("the source kind %q is not fetched by this runner", printable(o.Kind, 40))
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	u, err := checkURL(o.URL, o.anyPort)
	if err != nil {
		return err
	}
	tr := o.Transport
	if tr == nil {
		tr = &http.Transport{
			Proxy:                 http.ProxyURL(o.Proxy),
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			ForceAttemptHTTP2:     true,
		}
	}
	r := &remote{
		base: strings.TrimSuffix(u.String(), "/"),
		client: &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A redirect is followed only on the same host over https,
			// the one host the proxy lets the step reach: the token is
			// never sent anywhere else.
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, u.Host) {
				return fmt.Errorf("the host redirected to another address (%s), which is not followed", printable(req.URL.Host, 100))
			}
			return nil
		}},
		auth: basicAuth(o.Token),
	}
	l, err := r.discover(ctx, o.Ref)
	if err != nil {
		return err
	}
	rep.Protocol = "v0"
	if l.v2 {
		rep.Protocol = "v2"
	}
	ref := o.Ref
	if len(ref) == 40 {
		ref = strings.ToLower(ref)
	}
	want, name, err := l.resolve(ref)
	if err != nil {
		return err
	}
	rep.Ref = name
	packPath := filepath.Join(o.Work, "pack")
	pf, err := os.OpenFile(packPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the pack file: %w", err)
	}
	defer func() {
		_ = pf.Close()
		_ = os.Remove(packPath)
	}()
	// The pack of one commit's tree is no larger than its checkout, plus
	// the trees and the framing.
	maxPack := o.MaxBytes + 64<<20
	if err := r.fetch(ctx, l, want, pf, maxPack); err != nil {
		if errors.Is(err, errPackTooLarge) {
			rep.Refusal = fmt.Sprintf("the repository's pack at %s is larger than %s, more than a repository within the %s cap can be", want, sizeWords(maxPack), sizeWords(o.MaxBytes))
			return nil
		}
		return err
	}
	p, err := indexPack(pf, 4*o.MaxFiles+1000)
	var many tooManyObjects
	if errors.As(err, &many) {
		rep.Refusal = fmt.Sprintf("the repository's pack at %s holds %d objects, more than a repository within the %d-file cap can", want, many.count, o.MaxFiles)
		return nil
	}
	if err != nil {
		return err
	}
	tree, err := treeOf(p, want)
	if err != nil {
		return err
	}
	rep.Commit = want
	// Folders and what is left out count too, so the walk is bounded at
	// a multiple of the file cap.
	maxEntries := 2*o.MaxFiles + 10000
	pl, err := planTree(p, tree, maxEntries)
	if errors.Is(err, errTooManyEntries) {
		rep.Refusal = fmt.Sprintf("the repository holds more than %d files, folders and links at %s, past what the %d-file cap allows", maxEntries, want, o.MaxFiles)
		return nil
	}
	if err != nil {
		return err
	}
	rep.Skipped, rep.SkippedPaths = map[string]int{}, map[string][]string{}
	for why, paths := range pl.skipped {
		sort.Strings(paths)
		rep.Skipped[why] = len(paths)
		rep.SkippedPaths[why] = paths[:min(len(paths), maxSkippedListed)]
	}
	switch {
	case len(pl.files) > o.MaxFiles:
		rep.Refusal = fmt.Sprintf("the repository holds %d files at %s, past the %d-file cap", len(pl.files), want, o.MaxFiles)
		return nil
	case pl.bytes > o.MaxBytes:
		rep.Refusal = fmt.Sprintf("the repository holds %s of files at %s, past the %s cap", sizeWords(pl.bytes), want, sizeWords(o.MaxBytes))
		return nil
	}
	root, err := os.OpenRoot(o.Out)
	if err != nil {
		return fmt.Errorf("the output directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if entries, err := os.ReadDir(o.Out); err != nil || len(entries) != 0 {
		return errors.New("the output directory is not empty")
	}
	if err := pl.write(p, root); err != nil {
		return err
	}
	rep.Files, rep.Bytes, rep.LFSPointers = len(pl.files), pl.bytes, pl.lfsPointers(p)
	return nil
}

// CheckURL holds a repository address to what a source may be: https, a
// host, no credentials, no query or fragment (the ref travels apart), and
// no port but 443.
func CheckURL(raw string) (*url.URL, error) { return checkURL(raw, false) }

func checkURL(raw string, anyPort bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the repository address is not a URL")
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("the repository address must be https, not %q", printable(u.Scheme, 20))
	case u.User != nil:
		return nil, errors.New("the repository address carries credentials; give the token apart from the address")
	case u.Hostname() == "" || u.Opaque != "":
		return nil, errors.New("the repository address names no host")
	case u.Port() != "" && u.Port() != "443" && !anyPort:
		return nil, errors.New("the repository address names a port other than 443")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("the repository address carries a query or a fragment")
	case u.Path == "" || u.Path == "/":
		return nil, errors.New("the repository address names no repository")
	}
	return u, nil
}

// basicAuth is the Authorization header for a token.
func basicAuth(token string) string {
	if token == "" {
		return ""
	}
	user, pass, ok := strings.Cut(token, ":")
	if !ok {
		user, pass = "x-access-token", token
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func writeReport(dir string, rep Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ReportFile), append(b, '\n'), 0o644) //nolint:gosec // read by the caller
}

// sizeWords is a byte count as a person reads it.
func sizeWords(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
