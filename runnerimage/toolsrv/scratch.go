package toolsrv

// The scratch folder (with scratch_read's views, scratch_search and the
// bodies fetch saves here):
// every release's run and session may keep notes and working files in an
// empty folder of its own, a size-capped tmpfs the docker filter mounts at
// ScratchDir and that is gone with the container. Its tools are the
// runner's grant, never a line of the contract, so they have names of
// their own: fs_read, fs_list and fs_write keep meaning the workspace the
// contract grants, and the two roots never alias.
//
// A declared command may write the folder too, so what it
// holds is not the server's: a command can plant a link there, or swap a
// path for one while a call is under way. Every scratch tool therefore
// works through an os.Root opened on the folder, which resolves each name
// beneath it at the moment of use and never outside it, while the server,
// root and unconfined, would otherwise follow such a link into the output
// folder or anywhere else (resolveUnder's check alone is a check before
// use). The files are made world-writable (scratchFileMode,
// scratchDirMode) so that a command run as nobody, where there is no
// Landlock, can change what the agent wrote, as it can write the folder
// itself (mode 1777).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

const (
	scratchFileMode os.FileMode = 0o666
	scratchDirMode  os.FileMode = 0o777
)

func scratchReadSchema() map[string]any {
	return objSchema(map[string]any{
		"path":     strProp("scratch-relative slash path of the file"),
		"offset":   intProp("byte offset to start reading from (default 0)"),
		"maxBytes": intProp("cap on returned bytes; the server limit still applies"),
		"view": map[string]any{"type": "string", "enum": []string{ViewText, ViewOriginal},
			"description": "\"text\" reads a text file as text; \"original\" reads the file's bytes, a binary file as base64; default: text for a text file, original for a binary one"},
	}, "path")
}

func scratchSearchSchema() map[string]any {
	return objSchema(map[string]any{
		"query":      strProp("the word or phrase to find, matched without regard to case"),
		"path":       strProp("scratch-relative file or directory to search; empty or \".\" searches the whole folder"),
		"maxResults": intProp(fmt.Sprintf("most matching lines to return (default %d, at most %d)", defaultSearchHits, maxSearchHits)),
	}, "query")
}

func scratchListSchema() map[string]any {
	return objSchema(map[string]any{
		"path":   strProp("scratch-relative directory to list; empty or \".\" is the scratch folder itself"),
		"offset": offsetEntriesProp(),
	})
}

func scratchWriteSchema() map[string]any {
	return objSchema(map[string]any{
		"path":    strProp("scratch-relative slash path of the file to write"),
		"content": strProp("file content (text)"),
		"append":  boolProp("append to the file instead of replacing it"),
	}, "path", "content")
}

// scratchPath checks a name the model gave a scratch tool, as resolveUnder
// does for the other roots, and returns it cleaned and relative to the
// folder; "" is refused unless empty is allowed. The os.Root the tool then
// opens it through is what holds it beneath the folder.
func (s *server) scratchPath(rel string) (string, error) {
	if s.spec.ScratchDir == "" {
		return "", errors.New("this run has no scratch folder")
	}
	if _, err := resolveUnder(s.spec.ScratchDir, rel); err != nil {
		return "", err
	}
	return path.Clean(rel), nil
}

func (s *server) scratchRoot() (*os.Root, error) {
	return os.OpenRoot(s.spec.ScratchDir)
}

func (s *server) toolScratchRead(raw json.RawMessage) (string, bool) {
	var a struct {
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"maxBytes"`
		View     string `json:"view"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "scratch_read: " + err.Error(), true
	}
	if a.Path == "" {
		return "scratch_read: path is required", true
	}
	if a.Offset < 0 {
		return "scratch_read: offset must be >= 0", true
	}
	if err := validView(a.View); err != nil {
		return "scratch_read: " + err.Error(), true
	}
	rel, err := s.scratchPath(a.Path)
	if err != nil {
		return "scratch_read: " + err.Error(), true
	}
	root, err := s.scratchRoot()
	if err != nil {
		return "scratch_read: " + err.Error(), true
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		return "scratch_read: " + err.Error(), true
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && !fi.IsDir() {
		head, err := binaryHead(f)
		if err != nil {
			return "scratch_read: " + err.Error(), true
		}
		// The file decides, unless the view says: a binary file is read as
		// base64, never as text, and a text file's original is its text.
		switch {
		case head.binary && a.View == ViewText:
			return fmt.Sprintf("scratch_read: %s is a binary file (%d bytes, detected as %s), which has no text view; read it with view %q", rel, fi.Size(), head.mime, ViewOriginal), true
		case head.binary:
			text, err := base64Page(f, a.Offset, a.MaxBytes, s.spec.Limits.readCap(), s.spec.Limits.budget(), head.mime, fmt.Sprintf("pass scratch_read path %q, view %q, offset %%d", rel, ViewOriginal))
			if err != nil {
				return "scratch_read: " + err.Error(), true
			}
			return text, false
		}
	}
	return readText(fileTools{ToolScratchRead, ToolScratchList}, f, rel, a.Offset, a.MaxBytes, s.spec.Limits.readCap(), s.spec.Limits.budget())
}

// toolScratchSearch is search_inputs over the scratch folder: the same
// matching, caps and answers, over every regular text file beneath the path
// asked, each reached through the folder's os.Root and none by a link.
func (s *server) toolScratchSearch(raw json.RawMessage) (string, bool) {
	var a struct {
		Query      string `json:"query"`
		Path       string `json:"path"`
		MaxResults int    `json:"maxResults"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "scratch_search: " + err.Error(), true
	}
	needle, limit, refused := searchQuery(ToolScratchSearch, a.Query, a.MaxResults)
	if refused != "" {
		return refused, true
	}
	if a.Path == "" {
		a.Path = "."
	}
	rel, err := s.scratchPath(a.Path)
	if err != nil {
		return "scratch_search: " + err.Error(), true
	}
	root, err := s.scratchRoot()
	if err != nil {
		return "scratch_search: " + err.Error(), true
	}
	defer func() { _ = root.Close() }()
	var m matches
	err = fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := readRootPrefix(root, p, maxSearchFileBytes)
		if err != nil {
			return nil // a file a command removed mid-walk is not a failure
		}
		if bytes.IndexByte(data[:min(len(data), 64<<10)], 0) >= 0 {
			return nil // binary, as scratch_read judges it: never searched
		}
		m.add(p, data, needle, limit)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("scratch_search: nothing in the scratch folder at %q", rel), true
	}
	if err != nil {
		return "scratch_search: " + err.Error(), true
	}
	if m.total == 0 {
		return fmt.Sprintf("no scratch text matches %q", strings.TrimSpace(a.Query)), false
	}
	return searchAnswer(m.hits, m.total, s.spec.Limits.budget()), false
}

// readRootPrefix is readPrefix of a file beneath root.
func readRootPrefix(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		data = trimToRuneEnd(data[:limit])
	}
	return data, nil
}

func (s *server) toolScratchList(raw json.RawMessage) (string, bool) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "scratch_list: " + err.Error(), true
	}
	if a.Offset < 0 {
		return "scratch_list: offset must be >= 0", true
	}
	if a.Path == "" {
		a.Path = "."
	}
	rel, err := s.scratchPath(a.Path)
	if err != nil {
		return "scratch_list: " + err.Error(), true
	}
	root, err := s.scratchRoot()
	if err != nil {
		return "scratch_list: " + err.Error(), true
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(rel)
	if err != nil {
		return "scratch_list: " + err.Error(), true
	}
	defer func() { _ = d.Close() }()
	return listDir(ToolScratchList, d, a.Offset, s.spec.Limits.budget())
}

func (s *server) toolScratchWrite(raw json.RawMessage) (string, bool) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "scratch_write: " + err.Error(), true
	}
	if a.Path == "" {
		return "scratch_write: path is required", true
	}
	if env := s.holdsRunSecret(a.Content); env != "" {
		return "scratch_write: the content holds the value of this run's secret " + env + "; write what it is and where it was found, not the value", true
	}
	if limit := s.spec.Limits.writeCap(); int64(len(a.Content)) > limit {
		return fmt.Sprintf("scratch_write: content is %d bytes, over the %d-byte limit — append in parts", len(a.Content), limit), true
	}
	rel, err := s.scratchPath(a.Path)
	if err != nil {
		return "scratch_write: " + err.Error(), true
	}
	if rel == "." {
		return "scratch_write: path names the scratch folder itself", true
	}
	root, err := s.scratchRoot()
	if err != nil {
		return s.scratchFailed(err)
	}
	defer func() { _ = root.Close() }()
	if err := mkdirAllIn(root, path.Dir(rel), scratchDirMode); err != nil {
		return s.scratchFailed(err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if a.Append {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := root.OpenFile(rel, flag, scratchFileMode)
	if err != nil {
		return s.scratchFailed(err)
	}
	_, werr := f.WriteString(a.Content)
	if werr == nil {
		werr = f.Chmod(scratchFileMode) // past the umask, on the file opened
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return s.scratchFailed(werr)
	}
	verb := "wrote"
	if a.Append {
		verb = "appended"
	}
	return fmt.Sprintf("%s %d bytes to scratch %s", verb, len(a.Content), rel), false
}

// scratchFailed is scratch_write's failure. A full scratch folder (ENOSPC
// past its cap) is the agent's own doing, so unlike failed it is no fault
// of the run's channel in the calls ledger, which an agent could otherwise
// bring about; any other I/O error is one, as for the
// other file tools.
func (s *server) scratchFailed(err error) (string, bool) {
	if errors.Is(err, syscall.ENOSPC) {
		return ToolScratchWrite + ": the scratch folder is full (" + err.Error() + "); write the result with write_report", true
	}
	return s.failed(ToolScratchWrite, err)
}

// mkdirAllIn makes dir and its missing parents beneath root with mode,
// past the umask, each through root so that none can be a link out.
func mkdirAllIn(root *os.Root, dir string, mode os.FileMode) error {
	if dir == "." || dir == "" {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		if err := root.Mkdir(cur, mode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := root.Chmod(cur, mode); err != nil {
			return err
		}
	}
	return nil
}
