package toolsrv

// Fetch into the scratch folder (plan input-sources.md §4 and §6, D10;
// agentry-cli#831, agentry-cli#832). A fetch whose body is larger than one
// result may be is saved there and answered with its first page, so nothing
// the fetch read is lost and the harness never cuts the answer. A fetch
// that names saveTo is saved there whole, whatever its type, binary
// included, bounded by the folder's free space and the fetch's time rather
// than the reply cap, and answered with what was saved, never the body.
// Either way the host is one of the run's declared egress, every redirect
// hop is checked as toolFetch checks it, a body holding the value of one of
// the run's secrets is not kept, the file stays beneath the folder (os.Root)
// and is never executable, and the save is recorded in the fetch ledger
// the receipt counts.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// maxFetchHeadField bounds the status and the content type an answer
// repeats: a host's header must not crowd the page out of the budget.
const maxFetchHeadField = 256

// clip is s cut to at most n bytes on a rune boundary, with an ellipsis
// where it was cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return string(trimToRuneEnd([]byte(s[:n]))) + "…"
}

// fetchedOverBudget answers a fetch whose body is larger than one result may
// be: the body is saved in the scratch folder, and the answer is the status,
// the type, where it was saved and its first page, with how to read on.
// Nothing the fetch read is lost. A body that holds the value of one of this
// run's secrets is not saved, as a file the agent writes never holds one. A
// run without a scratch folder gets the first page and is told the rest was
// not kept.
func (s *server) fetchedOverBudget(u *url.URL, head string, data []byte, truncated bool, length int64) (string, bool) {
	size := fmt.Sprintf("%d bytes", len(data))
	if truncated {
		of := "more"
		if length >= 0 {
			of = strconv.FormatInt(length, 10)
		}
		size = fmt.Sprintf("the first %d bytes of %s; fetch reads no more of a body", len(data), of)
	}
	// The page's room is the budget's less the header's.
	room := s.spec.Limits.budget().less(head).room()
	if s.spec.ScratchDir == "" {
		// Scrubbed whole before the cut, so a secret the cut would split is
		// still replaced.
		page := room.cut([]byte(s.scrubSecrets(string(data))), false)
		text := string(page)
		if !utf8.Valid(page) {
			text = "(the body is not text, so no page of it is shown)"
		}
		return head + text + fmt.Sprintf("\n[shown: bytes 0–%d; the body: %s; this run has no scratch folder, so nothing past what is shown is kept]", len(page), size), false
	}
	if env := s.holdsRunSecret(string(data)); env != "" {
		return notSavedForSecret(env), true
	}
	rel, err := s.saveFetched(u, data)
	if err == nil {
		sum := sha256.Sum256(data)
		err = s.recordSave(SaveEntry{Path: rel, Host: u.Hostname(), Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return didNotFit(err), true
		}
		return s.failed(ToolFetch, err)
	}
	saved := fmt.Sprintf("saved: %s/%s (%s); the body is larger than one tool result, so read it on with scratch_read\n\n", s.spec.ScratchDir, rel, size)
	page := room.cut(data, false)
	if !utf8.Valid(page) {
		return s.scrubSecrets(head + saved + "(the body is not text, so no page of it is shown)"), false
	}
	text, _ := textPage(page, 0, int64(len(data)), fmt.Sprintf("scratch_read path %q, offset %%d", rel))
	return s.scrubSecrets(head + saved + text), false
}

// fetchDir is the folder in the scratch folder over-budget fetch bodies are
// saved in.
const fetchDir = "fetch"

// saveFetched writes a fetched body to fetch/<n>-<name> in the scratch
// folder, through the folder's os.Root, never over a file already there (a
// declared command may have put one in the way), and never executable. It
// returns the file's scratch-relative path.
func (s *server) saveFetched(u *url.URL, data []byte) (string, error) {
	root, err := s.scratchRoot()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := mkdirAllIn(root, fetchDir, scratchDirMode); err != nil {
		return "", err
	}
	name := fetchedName(u)
	for range 1000 {
		s.fetchSeq++
		rel := fmt.Sprintf("%s/%d-%s", fetchDir, s.fetchSeq, name)
		f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, scratchFileMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if werr == nil {
			werr = f.Chmod(scratchFileMode) // past the umask, and never executable
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = root.Remove(rel)
			return "", werr
		}
		return rel, nil
	}
	return "", errors.New("no free name in the scratch folder's fetch folder")
}

// fetchedName is a saved body's file name: the URL's last path segment,
// made safe (letters, digits, '.', '-' and '_', at most 64 bytes, never
// starting with a dot or a dash), or "body".
func fetchedName(u *url.URL) string {
	var b strings.Builder
	for _, r := range path.Base(u.Path) {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if name := strings.TrimLeft(b.String(), ".-"); name != "" {
		return name
	}
	return "body"
}

// SaveEntry is one line of the fetch ledger: a body fetch saved in the
// scratch folder. Path is relative to the folder and Host is the declared
// host the request was made to; Asked says the agent named the path
// (saveTo), where a body larger than one result is saved unasked.
type SaveEntry struct {
	Path   string `json:"path"`
	Host   string `json:"host"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Asked  bool   `json:"asked,omitempty"`
}

// validateFetchLedger holds the fetch ledger to its grant and to the
// output folder, as validatePDF holds the PDF ledger.
func validateFetchLedger(spec *Spec) error {
	l := spec.Fetch.Ledger
	if l == "" {
		return nil
	}
	if !spec.Fetch.Enabled || spec.ScratchDir == "" {
		return errors.New("fetch.ledger without both the fetch grant and the scratch folder")
	}
	if rel, err := filepath.Rel(spec.OutDir, l); !filepath.IsAbs(l) || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("fetch.ledger %q must be an absolute path under outDir %q", l, spec.OutDir)
	}
	return nil
}

// appendSave records a saved body in the fetch ledger, as appendPDF does.
func (s *server) appendSave(e SaveEntry) error {
	l := s.spec.Fetch.Ledger
	if l == "" {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
	if err != nil {
		return err
	}
	s.own(l)
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// recordSave records a saved body in the fetch ledger, or removes it: a
// file the ledger does not name is not kept, so the receipt never counts
// fewer saves than the folder held.
func (s *server) recordSave(e SaveEntry) error {
	err := s.appendSave(e)
	if err != nil {
		if root, rerr := s.scratchRoot(); rerr == nil {
			_ = root.Remove(e.Path)
			_ = root.Close()
		}
	}
	return err
}

// saveToPath checks a saveTo the model gave fetch before any request is
// made: the run has a scratch folder, and the path is a file beneath it.
func (s *server) saveToPath(saveTo string) (string, error) {
	rel, err := s.scratchPath(saveTo)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", errors.New("saveTo names the scratch folder itself; name a file in it")
	}
	return rel, nil
}

// fetchedTo answers a fetch that named saveTo: the body is streamed into
// the scratch folder at rel and the answer is what was saved (the status,
// the headers that say what it is, the bytes written and their SHA-256),
// never the body. Nothing is left under rel when the save fails.
func (s *server) fetchedTo(u *url.URL, resp *http.Response, rel string) (string, bool) {
	n, sum, err := s.saveStream(rel, resp.Body)
	var held errHoldsSecret
	switch {
	case errors.As(err, &held):
		return notSavedForSecret(string(held)), true
	case errors.Is(err, syscall.ENOSPC):
		return didNotFit(err), true
	case errors.Is(err, errSaveRead):
		return s.scrubSecrets("fetch: read response: " + err.Error()), true
	case err != nil:
		return s.failed(ToolFetch, err)
	}
	if err := s.recordSave(SaveEntry{Path: rel, Host: u.Hostname(), Bytes: n, SHA256: sum, Asked: true}); err != nil {
		return s.failed(ToolFetch, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %s\n", clip(resp.Status, maxFetchHeadField))
	header := func(name, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, clip(v, maxFetchHeadField))
		}
	}
	header("content-type", resp.Header.Get("Content-Type"))
	if resp.ContentLength >= 0 {
		header("content-length", strconv.FormatInt(resp.ContentLength, 10))
	}
	header("etag", resp.Header.Get("ETag"))
	header("last-modified", resp.Header.Get("Last-Modified"))
	fmt.Fprintf(&b, "\nsaved: %s/%s, %d bytes, sha256 %s; read it with scratch_read (view \"original\" for a binary file) or search it with scratch_search", s.spec.ScratchDir, rel, n, sum)
	return s.scrubSecrets(b.String()), false
}

// notSavedForSecret and didNotFit are the answers of a save refused for a
// run secret's value and of one the scratch folder had no room for: the
// agent's own doing, so never a fault of the run's machinery.
func notSavedForSecret(env string) string {
	return ToolFetch + ": the response body holds the value of this run's secret " + env + ", so it was not saved; fetch what you need without it"
}

func didNotFit(err error) string {
	return ToolFetch + ": the response body did not fit in the scratch folder (" + err.Error() + "); remove what you no longer need from it, or fetch less"
}

// errHoldsSecret names the secret a body held, which saveStream refused.
type errHoldsSecret string

func (e errHoldsSecret) Error() string { return "the body holds the value of the secret " + string(e) }

// errSaveRead marks a failure reading the response, not writing the file.
var errSaveRead = errors.New("reading the body failed")

// saveStream writes body to rel in the scratch folder, through the
// folder's os.Root: first to a new file of its own beside rel, never
// executable, then renamed over rel, so a file a declared command left at
// rel (a link planted after saveToPath's check included) is replaced, never
// written through, and a save that fails or holds a run secret leaves
// nothing behind. It returns the bytes written and their SHA-256.
func (s *server) saveStream(rel string, body io.Reader) (int64, string, error) {
	root, err := s.scratchRoot()
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = root.Close() }()
	dir := path.Dir(rel)
	if err := mkdirAllIn(root, dir, scratchDirMode); err != nil {
		return 0, "", err
	}
	var tmp string
	var f *os.File
	for range 1000 {
		s.partSeq++
		tmp = path.Join(dir, fmt.Sprintf(".fetch-%d.part", s.partSeq))
		f, err = root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, scratchFileMode)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	w := s.newSecretWatch()
	n, err := io.Copy(io.MultiWriter(f, h, w), readErrs{body})
	if err == nil {
		err = f.Chmod(scratchFileMode) // past the umask, and never executable
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// readErrs marks its reader's errors as errSaveRead's, so a failed read of
// the response is told from a failed write of the file.
type readErrs struct{ r io.Reader }

func (r readErrs) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("%w: %w", errSaveRead, err)
	}
	return n, err
}

// secretWatch finds the value of one of the server's own secrets in a
// stream written to it in parts, and fails the write that completes one
// (errHoldsSecret), which ends the copy: each part is searched with the end
// of the part before, so a value the parts split is found too.
type secretWatch struct {
	vals map[string]string
	keep int
	tail []byte
}

// newSecretWatch watches for the values of the server's own secrets, those
// holdsRunSecret looks for.
func (s *server) newSecretWatch() *secretWatch {
	w := &secretWatch{vals: map[string]string{}}
	for _, env := range s.grantedSecretEnv() {
		if v := os.Getenv(env); v != "" {
			w.vals[env] = v
			w.keep = max(w.keep, len(v)-1)
		}
	}
	return w
}

func (w *secretWatch) Write(p []byte) (int, error) {
	if len(w.vals) == 0 {
		return len(p), nil
	}
	buf := append(w.tail, p...)
	for env, v := range w.vals {
		if bytes.Contains(buf, []byte(v)) {
			return 0, errHoldsSecret(env)
		}
	}
	w.tail = append(w.tail[:0], buf[max(0, len(buf)-w.keep):]...)
	return len(p), nil
}
