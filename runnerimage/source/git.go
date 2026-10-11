package source

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The client half of git's smart HTTP transport (gitprotocol-http), as
// much of it as a depth-1 fetch of one commit takes: the ref listing, in
// protocol v2 (gitprotocol-v2: ls-refs, then fetch with deepen 1) or, from
// a host that does not speak v2, protocol v0 (the advertisement, then
// want, deepen 1, done). Nothing else of git is spoken: no push, no
// history, no submodules, no LFS.

const (
	userAgent = "git/2.0 (agentry-runner)"
	// maxRefsBytes bounds the ref listing a host may send.
	maxRefsBytes = 64 << 20
)

// remote is one repository on one host.
type remote struct {
	base   string // the repository's URL, without a trailing slash
	client *http.Client
	auth   string // the Authorization header, "" for anonymous
}

// listing is what the host advertised: its protocol, its capabilities, and
// its refs by full name (HEAD included), peeled tags under name+"^{}".
type listing struct {
	v2   bool
	caps map[string]string
	refs map[string]string
	// head is the branch HEAD points at, when the host says.
	head string
}

// discover asks for the ref advertisement, offering protocol v2. A v2 host
// answers with its capabilities and the refs are listed with ls-refs; a v0
// host answers with its refs.
func (r *remote) discover(ctx context.Context, ref string) (*listing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := r.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		return nil, fmt.Errorf("the host does not serve git's smart HTTP protocol at this address (it answered %q)", printable(ct, 80))
	}
	pr := &pktReader{r: io.LimitReader(resp.Body, maxRefsBytes)}
	first, kind, err := pr.line()
	if err != nil {
		return nil, err
	}
	if kind == pktData && strings.HasPrefix(first, "# service=") {
		// The smart HTTP preamble, then a flush.
		if k, _, err := pr.next(); err != nil || k != pktFlush {
			return nil, errors.New("the host's ref advertisement is malformed")
		}
		if first, kind, err = pr.line(); err != nil {
			return nil, err
		}
	}
	if kind != pktData {
		return nil, errors.New("the repository is empty: the host advertises no refs")
	}
	if first == "version 2" {
		l := &listing{v2: true, caps: map[string]string{}}
		for {
			s, k, err := pr.line()
			if err != nil {
				return nil, err
			}
			if k == pktFlush {
				break
			}
			if k != pktData {
				return nil, errors.New("the host's capability advertisement is malformed")
			}
			name, val, _ := strings.Cut(s, "=")
			l.caps[name] = val
		}
		if err := l.checkV2(); err != nil {
			return nil, err
		}
		if err := r.lsRefs(ctx, l, ref); err != nil {
			return nil, err
		}
		return l, nil
	}
	return parseV0(pr, first)
}

// checkV2 holds a v2 host to what this client needs: ls-refs, a fetch that
// can deepen, and SHA-1 object names.
func (l *listing) checkV2() error {
	if _, ok := l.caps["ls-refs"]; !ok {
		return errors.New("the host speaks git protocol v2 without ls-refs")
	}
	fetch, ok := l.caps["fetch"]
	if !ok {
		return errors.New("the host speaks git protocol v2 without fetch")
	}
	if !hasWord(fetch, "shallow") {
		return errors.New("the host does not serve shallow clones, so the repository cannot be fetched at depth 1")
	}
	if f, ok := l.caps["object-format"]; ok && f != "sha1" {
		return fmt.Errorf("the repository uses the %s object format, which is not supported", printable(f, 20))
	}
	return nil
}

// lsRefs lists HEAD, the branches and the tags (and, for a full ref name,
// that ref) with their peeled values and HEAD's target.
func (r *remote) lsRefs(ctx context.Context, l *listing, ref string) error {
	var w pktWriter
	w.line("command=ls-refs")
	r.v2Caps(&w, l)
	w.delim()
	w.line("peel")
	w.line("symrefs")
	for _, p := range []string{"HEAD", "refs/heads/", "refs/tags/"} {
		w.line("ref-prefix " + p)
	}
	if strings.HasPrefix(ref, "refs/") && !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		w.line("ref-prefix " + ref)
	}
	w.flush()
	resp, err := r.post(ctx, &w, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	pr := &pktReader{r: io.LimitReader(resp.Body, maxRefsBytes)}
	l.refs = map[string]string{}
	for {
		s, k, err := pr.line()
		if err != nil {
			return err
		}
		if k == pktFlush {
			return nil
		}
		if k != pktData {
			return errors.New("the host's ref listing is malformed")
		}
		fields := strings.Split(s, " ")
		if len(fields) < 2 {
			return errors.New("the host's ref listing is malformed")
		}
		oid, name := fields[0], fields[1]
		if oid == "unborn" {
			continue
		}
		if !validOID(oid) {
			return errors.New("the host's ref listing names an object that is not a SHA-1")
		}
		l.refs[name] = oid
		for _, attr := range fields[2:] {
			if v, ok := cutPrefix(attr, "peeled:"); ok && validOID(v) {
				l.refs[name+"^{}"] = v
			}
			if v, ok := cutPrefix(attr, "symref-target:"); ok && name == "HEAD" {
				l.head = v
			}
		}
	}
}

// parseV0 reads a v0 advertisement: the first ref line carries the
// capabilities after a NUL, then one ref a line to a flush.
func parseV0(pr *pktReader, first string) (*listing, error) {
	l := &listing{caps: map[string]string{}, refs: map[string]string{}}
	refLine, caps, _ := strings.Cut(first, "\x00")
	for _, c := range strings.Fields(caps) {
		name, val, _ := strings.Cut(c, "=")
		if name == "symref" {
			if from, to, ok := strings.Cut(val, ":"); ok && from == "HEAD" {
				l.head = to
			}
			continue
		}
		l.caps[name] = val
	}
	if f, ok := l.caps["object-format"]; ok && f != "sha1" {
		return nil, fmt.Errorf("the repository uses the %s object format, which is not supported", printable(f, 20))
	}
	if _, ok := l.caps["shallow"]; !ok {
		return nil, errors.New("the host does not serve shallow clones, so the repository cannot be fetched at depth 1")
	}
	add := func(s string) error {
		oid, name, ok := strings.Cut(s, " ")
		if !ok || !validOID(oid) {
			return errors.New("the host's ref advertisement is malformed")
		}
		if name != "capabilities^{}" {
			l.refs[name] = oid
		}
		return nil
	}
	if err := add(refLine); err != nil {
		return nil, err
	}
	for {
		s, k, err := pr.line()
		if err != nil {
			return nil, err
		}
		if k == pktFlush {
			break
		}
		if k != pktData {
			return nil, errors.New("the host's ref advertisement is malformed")
		}
		if err := add(s); err != nil {
			return nil, err
		}
	}
	if len(l.refs) == 0 {
		return nil, errors.New("the repository is empty: the host advertises no refs")
	}
	return l, nil
}

// resolve turns the ref asked for into the commit to fetch and the ref's
// full name: no ref is HEAD; a full commit name is itself; a full ref name
// is that ref; a short name is a branch, then a tag, peeled to its commit.
func (l *listing) resolve(ref string) (oid, name string, err error) {
	peeled := func(n string) string {
		if p, ok := l.refs[n+"^{}"]; ok {
			return p
		}
		return l.refs[n]
	}
	switch {
	case ref == "":
		if _, ok := l.refs["HEAD"]; !ok {
			return "", "", errors.New("the repository has no default branch (HEAD); name a branch, tag or commit after # in its address")
		}
		name = l.head
		if name == "" {
			name = "HEAD"
		}
		return l.refs["HEAD"], name, nil
	case validOID(ref):
		return ref, ref, nil
	case strings.HasPrefix(ref, "refs/"):
		if o := peeled(ref); o != "" {
			return o, ref, nil
		}
	default:
		for _, n := range []string{"refs/heads/" + ref, "refs/tags/" + ref} {
			if o := peeled(n); o != "" {
				return o, n, nil
			}
		}
	}
	return "", "", fmt.Errorf("the repository has no branch or tag named %q", printable(ref, 200))
}

// fetch asks for one commit at depth 1 and writes the pack the host sends
// to w, at most maxPack bytes of it.
func (r *remote) fetch(ctx context.Context, l *listing, want string, w io.Writer, maxPack int64) error {
	var body pktWriter
	sideband := true
	if l.v2 {
		body.line("command=fetch")
		r.v2Caps(&body, l)
		body.delim()
		body.line("no-progress")
		body.line("ofs-delta")
		body.line("deepen 1")
		body.line("want " + want)
		body.line("done")
		body.flush()
	} else {
		caps := []string{}
		for _, c := range []string{"side-band-64k", "ofs-delta", "shallow", "no-progress"} {
			if _, ok := l.caps[c]; ok {
				caps = append(caps, c)
			}
		}
		_, sb64 := l.caps["side-band-64k"]
		if _, sb := l.caps["side-band"]; !sb64 && sb {
			caps = append(caps, "side-band")
			sb64 = true
		}
		sideband = sb64
		if _, ok := l.caps["agent"]; ok {
			caps = append(caps, "agent="+agentName)
		}
		body.line("want " + want + " " + strings.Join(caps, " "))
		body.line("deepen 1")
		body.flush()
		body.line("done")
	}
	resp, err := r.post(ctx, &body, l.v2)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	pr := &pktReader{r: resp.Body}
	if l.v2 {
		if err := skipToPackfile(pr); err != nil {
			return err
		}
	} else if err := skipV0Preamble(pr); err != nil {
		return err
	}
	lw := &limitWriter{w: w, n: maxPack}
	if !sideband {
		if _, err := io.Copy(lw, resp.Body); err != nil {
			return err
		}
		return nil
	}
	for {
		kind, data, err := pr.next()
		if err != nil {
			return fmt.Errorf("the pack stream ended early: %w", err)
		}
		if kind == pktFlush || kind == pktEnd {
			return nil
		}
		if kind != pktData || len(data) == 0 {
			return errors.New("the host's pack stream is malformed")
		}
		switch data[0] {
		case 1:
			if _, err := lw.Write(data[1:]); err != nil {
				return err
			}
		case 2:
			// Progress, which no-progress asked the host not to send.
		case 3:
			return hostError(string(data[1:]))
		default:
			return errors.New("the host's pack stream is malformed")
		}
	}
}

// skipToPackfile reads a v2 fetch answer's sections up to the packfile's.
func skipToPackfile(pr *pktReader) error {
	for {
		s, k, err := pr.line()
		if err != nil {
			return err
		}
		switch {
		case k == pktFlush || k == pktEnd:
			return errors.New("the host sent no pack")
		case k == pktDelim:
		case s == "packfile":
			return nil
		}
	}
}

// skipV0Preamble reads a v0 fetch answer's shallow lines, their flush and
// the NAK, up to the pack.
func skipV0Preamble(pr *pktReader) error {
	for {
		s, k, err := pr.line()
		if err != nil {
			return err
		}
		switch {
		case k == pktFlush:
		case k != pktData:
			return errors.New("the host's fetch answer is malformed")
		case strings.HasPrefix(s, "shallow ") || strings.HasPrefix(s, "unshallow "):
		case s == "NAK" || strings.HasPrefix(s, "ACK "):
			return nil
		default:
			return errors.New("the host's fetch answer is malformed")
		}
	}
}

const agentName = "agentry-runner"

// v2Caps sends the capabilities a v2 command may carry, those the host
// advertised.
func (r *remote) v2Caps(w *pktWriter, l *listing) {
	if _, ok := l.caps["agent"]; ok {
		w.line("agent=" + agentName)
	}
	if _, ok := l.caps["object-format"]; ok {
		w.line("object-format=sha1")
	}
}

func (r *remote) post(ctx context.Context, body *pktWriter, v2 bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/git-upload-pack", bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	if v2 {
		req.Header.Set("Git-Protocol", "version=2")
	}
	resp, err := r.do(req)
	if err != nil {
		return nil, err
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-result" {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the host answered git-upload-pack with %q", printable(ct, 80))
	}
	return resp, nil
}

// do sends one request with the credential, and says plainly what a
// refusal means.
func (r *remote) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", req.URL.Host, scrubURLError(err))
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	_ = resp.Body.Close()
	return nil, statusError(resp.StatusCode, r.auth != "", "repository")
}

// statusError says what the host's refusal of a source (noun: repository,
// file) means for the person.
func statusError(code int, token bool, noun string) error {
	switch {
	case (code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound) && !token:
		return fmt.Errorf("the host answered %d %s: the %s does not exist, or it is private and needs an access token (no token was given)", code, http.StatusText(code), noun)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("the host answered %d %s: it refused the access token given for this %s", code, http.StatusText(code), noun)
	case code == http.StatusNotFound:
		return fmt.Errorf("the host answered 404 Not Found: the %s does not exist, or the access token given cannot read it", noun)
	default:
		return fmt.Errorf("the host answered %d %s", code, http.StatusText(code))
	}
}

// scrubURLError keeps a transport error's cause and drops the request's
// URL, which a *url.Error repeats in full.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// limitWriter fails a write past n bytes.
type limitWriter struct {
	w io.Writer
	n int64
}

var errPackTooLarge = errors.New("the pack is too large")

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errPackTooLarge
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

func validOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func hasWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}
