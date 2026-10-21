package toolsrv

// A persistent agent's share folder is the person's own, live: they put
// files there while the agent runs. When the spec says the share is checked
// (Spec.ShareChecks), a file there is read only once ClamAV has checked its
// bytes. The runner checks what is new or changed in the share before each
// turn and writes the verdicts into a folder this server reads; a file with
// no verdict for its bytes is refused as not yet checked, one ClamAV found
// is refused naming what it found, and a file this server wrote for the
// agent is read as written. fs_read holds the file's SHA-256 to its
// verdict; grep and glob, which walk many files, hold each file's size and
// modification time to the ones the runner checked.
//
// The check runs the decode step over each file, and the runner keeps the
// text it read out of a document (a PDF, a Word file, an image by OCR) at
// ShareTextDir/<SHA-256>.txt beside the verdicts. fs_read serves that text
// for a document, found by the digest of the bytes it read and checked, so
// the text it serves is always the text of those bytes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"unicode/utf8"
)

// ShareVerdicts is the verdicts file the runner writes (ShareChecks.File).
type ShareVerdicts struct {
	Version int `json:"version"`
	// Files are the share's checked files, by their slash path beneath the
	// share folder.
	Files map[string]ShareVerdict `json:"files"`
}

// ShareVerdictsVersion is the verdicts file's version.
const ShareVerdictsVersion = 2

// ShareTextDir is the folder beside the verdicts file that holds the text
// the decode step read out of each checked document, one file a digest:
// <SHA-256>.txt.
const ShareTextDir = "text"

// ShareTextName is the name, in ShareTextDir, of the text kept for the
// bytes whose SHA-256 is sum.
func ShareTextName(sum string) string { return sum + ".txt" }

// maxShareText bounds a kept text fs_read serves: the decode step's own
// cut, with room to spare.
const maxShareText = 16 << 20

// The verdicts a checked file can have.
const (
	// ShareClean: ClamAV checked the bytes and found nothing.
	ShareClean = "clean"
	// ShareFound: ClamAV found Signature in the bytes.
	ShareFound = "found"
	// ShareUnchecked: the runner did not check the bytes with ClamAV
	// (Reason says why: the malware check is off on this computer, no
	// signatures on it, a file larger than the check takes). Read as it
	// is, as a run's inputs are then.
	ShareUnchecked = "unchecked"
)

// ShareVerdict is one checked file's verdict, and the bytes it is for.
type ShareVerdict struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MTimeNano int64  `json:"mtimeNano"`
	Verdict   string `json:"verdict"`
	Signature string `json:"signature,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Text says the runner kept the text the decode step read out of
	// these bytes, at ShareTextDir/ShareTextName(SHA256); Tool names the
	// reader that read it (pdftotext, pandoc, tesseract, agentry-runner,
	// tika).
	Text bool   `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
	// Unread is why no reader could read text out of these bytes, in the
	// decode step's words; empty when one did, or when the file is text.
	Unread string `json:"unread,omitempty"`
}

// shareState is what the server remembers of the share between calls: the
// digests it wrote, and each written file's size and time.
type shareState struct {
	mu      sync.Mutex
	written map[string]bool      // digests of files fs_write wrote there
	wrote   map[string]fileStamp // path beneath the share → its stamp once written
}

type fileStamp struct {
	size  int64
	mtime int64
}

func stampOf(fi os.FileInfo) fileStamp {
	return fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
}

// shareRel says whether rel (a path beneath WorkspaceDir) is in the checked
// share folder, and its path beneath that folder.
func (s *server) shareRel(rel string) (string, bool) {
	sc := s.spec.ShareChecks
	if sc == nil {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == sc.Folder {
		return ".", true
	}
	if strings.HasPrefix(clean, sc.Folder+"/") {
		return strings.TrimPrefix(clean, sc.Folder+"/"), true
	}
	return "", false
}

// shareVerdicts reads the runner's verdicts as they are now. A missing file
// is no verdicts.
func (s *server) shareVerdicts() (ShareVerdicts, error) {
	var v ShareVerdicts
	b, err := os.ReadFile(s.spec.ShareChecks.File)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("the share's verdicts do not parse: %w", err)
	}
	if v.Version != ShareVerdictsVersion {
		return v, fmt.Errorf("the share's verdicts are version %d, this server reads %d", v.Version, ShareVerdictsVersion)
	}
	return v, nil
}

// maxShareRead is the largest share file fs_read reads, whole, to check it.
const maxShareRead = 64 << 20

// shareRead reads an open file of the share whole and answers its bytes and
// the verdict they have, or why they may not be read: they are read only
// when they are the bytes a verdict that is not a finding is for, or the
// ones this server wrote (whose verdict is the zero one).
func (s *server) shareRead(f *os.File, inShare string) ([]byte, ShareVerdict, string) {
	data, err := io.ReadAll(io.LimitReader(f, maxShareRead+1))
	if err != nil {
		return nil, ShareVerdict{}, "the file could not be read to check it: " + err.Error()
	}
	if len(data) > maxShareRead {
		return nil, ShareVerdict{}, fmt.Sprintf("the file is larger than the %d MiB a checked read takes", maxShareRead>>20)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	s.share.mu.Lock()
	mine := s.share.written[digest]
	s.share.mu.Unlock()
	if mine {
		return data, ShareVerdict{}, ""
	}
	verdicts, err := s.shareVerdicts()
	if err != nil {
		return nil, ShareVerdict{}, err.Error()
	}
	if v, ok := verdicts.Files[inShare]; ok && v.SHA256 == digest {
		return data, v, refusalOf(v)
	}
	// The same bytes under another name: moved or copied since the check.
	for _, v := range verdicts.Files {
		if v.SHA256 == digest {
			return data, v, refusalOf(v)
		}
	}
	return nil, ShareVerdict{}, "this file has not been checked yet: a file added to the share or changed there is checked before your next turn, so ask again then"
}

// shareText opens the text kept for the bytes whose SHA-256 is digest, the
// digest fs_read computed of the bytes it read: a regular file in the
// verdicts' text folder, within maxShareText. The folder is the runner's,
// mounted read-only, and nothing in it is followed through a link.
func (s *server) shareText(digest string) (*os.File, int64, error) {
	root, err := os.OpenRoot(path.Join(path.Dir(s.spec.ShareChecks.File), ShareTextDir))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = root.Close() }()
	name := ShareTextName(digest)
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxShareText {
		return nil, 0, fmt.Errorf("the text kept for it is not a regular file within %d MiB", maxShareText>>20)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// readerWords names a decode step reader as fs_read's header says it.
var readerWords = map[string]string{
	"pdftotext":      "pdftotext",
	"pandoc":         "pandoc",
	"tesseract":      "tesseract (OCR)",
	"agentry-runner": "Agentry's own reader",
	"tika":           "Apache Tika",
}

// shareFileRead is fs_read's answer for the checked bytes data of rel, a file
// of the share whose verdict is v. By default a document is read by the text
// the runner kept for those bytes, headed with what read it; a file that is
// text is read as it is; view "original" reads the file itself, a binary one
// as base64 pages.
func (s *server) shareFileRead(rel string, data []byte, v ShareVerdict, view string, offset, maxBytes int64) (string, bool) {
	readCap, bud := s.spec.Limits.readCap(), s.spec.Limits.budget()
	binary := bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
	if view == ViewOriginal || (!v.Text && !binary) {
		if !binary {
			return readTextFrom(ToolFSRead, bytes.NewReader(data), int64(len(data)), offset, maxBytes, readCap, bud)
		}
		head, err := binaryHead(bytes.NewReader(data))
		if err != nil {
			return "fs_read: " + err.Error(), true
		}
		text, err := base64PageAt(bytes.NewReader(data), int64(len(data)), offset, maxBytes, readCap, bud, head.mime, fmt.Sprintf("pass fs_read path %q, view %q, offset %%d", rel, ViewOriginal))
		if err != nil {
			return "fs_read: " + err.Error(), true
		}
		return text, false
	}
	if !v.Text {
		if v.Unread != "" {
			return fmt.Sprintf("fs_read: %s: no text could be read out of this file (%s); view %q reads its bytes as base64", rel, v.Unread, ViewOriginal), true
		}
		return fmt.Sprintf("fs_read: %s is a binary file (%d bytes) with no text read out of it; view %q reads its bytes as base64", rel, len(data), ViewOriginal), true
	}
	sum := sha256.Sum256(data)
	f, size, err := s.shareText(hex.EncodeToString(sum[:]))
	if err != nil {
		return fmt.Sprintf("fs_read: %s: the text read out of this file is not there (%v); view %q reads its bytes as base64", rel, err, ViewOriginal), true
	}
	defer func() { _ = f.Close() }()
	reader := readerWords[v.Tool]
	if reader == "" {
		reader = "the decode step"
	}
	header := fmt.Sprintf("[%s: the text %s read out of this file (offsets count the text's bytes); view %q reads the file itself]\n", rel, reader, ViewOriginal)
	page, isErr := readTextFrom(ToolFSRead, f, size, offset, maxBytes, readCap, bud.less(header))
	if isErr {
		return page, true
	}
	return header + page, false
}

func refusalOf(v ShareVerdict) string {
	switch v.Verdict {
	case ShareFound:
		return "ClamAV found " + v.Signature + " in this file, so it is not read; tell the person"
	case ShareClean, ShareUnchecked:
		return ""
	}
	return "the runner's verdict on this file is not one this server knows"
}

// shareWithheld says whether a search may not show the file at abs (a path
// as the search shows it): one in the share whose size and time are not the
// ones a verdict that is not a finding is for, nor the ones this server
// wrote.
func (s *server) shareWithheld(verdicts ShareVerdicts) func(abs string) bool {
	return func(abs string) bool {
		rel, ok := strings.CutPrefix(abs, s.spec.WorkspaceDir+"/")
		if !ok {
			return false
		}
		inShare, ok := s.shareRel(rel)
		if !ok || inShare == "." {
			return false
		}
		fi, err := os.Lstat(abs)
		if err != nil || !fi.Mode().IsRegular() {
			return true
		}
		st := stampOf(fi)
		s.share.mu.Lock()
		w, mine := s.share.wrote[inShare]
		s.share.mu.Unlock()
		if mine && w == st {
			return false
		}
		v, ok := verdicts.Files[inShare]
		return !ok || v.Verdict == ShareFound || v.Size != st.size || v.MTimeNano != st.mtime
	}
}

// shareWrote records a file fs_write wrote in the share, so the agent reads
// back what it wrote before the runner's next check.
func (s *server) shareWrote(inShare string, content []byte, abs string) {
	sum := sha256.Sum256(content)
	s.share.mu.Lock()
	defer s.share.mu.Unlock()
	if s.share.written == nil {
		s.share.written, s.share.wrote = map[string]bool{}, map[string]fileStamp{}
	}
	s.share.written[hex.EncodeToString(sum[:])] = true
	if fi, err := os.Lstat(abs); err == nil {
		s.share.wrote[inShare] = stampOf(fi)
	}
}
