package toolsrv

// A fetch whose body is larger than one result may be (plan input-sources.md
// §4, agentry-cli#831): the body is saved in the scratch folder and the
// answer is its first page, so nothing the fetch read is lost and the
// harness never cuts the answer.

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
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
		return "fetch: the response body holds the value of this run's secret " + env + ", so it was not saved; fetch what you need without it", true
	}
	rel, err := s.saveFetched(u, data)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return ToolFetch + ": the response body did not fit in the scratch folder (" + err.Error() + "); remove what you no longer need from it, or fetch less", true
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
