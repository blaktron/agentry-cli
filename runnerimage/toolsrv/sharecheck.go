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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
)

// ShareVerdicts is the verdicts file the runner writes (ShareChecks.File).
type ShareVerdicts struct {
	Version int `json:"version"`
	// Files are the share's checked files, by their slash path beneath the
	// share folder.
	Files map[string]ShareVerdict `json:"files"`
}

// ShareVerdictsVersion is the verdicts file's version.
const ShareVerdictsVersion = 1

// The verdicts a checked file can have.
const (
	// ShareClean: ClamAV checked the bytes and found nothing.
	ShareClean = "clean"
	// ShareFound: ClamAV found Signature in the bytes.
	ShareFound = "found"
	// ShareUnchecked: the runner could not check the bytes (Reason says
	// why: no signatures on the machine, a file larger than the check
	// takes). Read as it is, as a run's inputs are then.
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

// shareRefusal is why an open file in the share may not be read, or "" when
// it may: its bytes are the ones a verdict that is not a finding is for, or
// the ones this server wrote.
func (s *server) shareRefusal(f *os.File, inShare string) string {
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "the file could not be read to check it: " + err.Error()
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "the file could not be read to check it: " + err.Error()
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	s.share.mu.Lock()
	mine := s.share.written[digest]
	s.share.mu.Unlock()
	if mine {
		return ""
	}
	verdicts, err := s.shareVerdicts()
	if err != nil {
		return err.Error()
	}
	if v, ok := verdicts.Files[inShare]; ok && v.SHA256 == digest {
		return refusalOf(v)
	}
	// The same bytes under another name: moved or copied since the check.
	for _, v := range verdicts.Files {
		if v.SHA256 == digest {
			return refusalOf(v)
		}
	}
	return "this file has not been checked for malware yet: a file added to the share or changed there is checked before your next turn, so ask again then"
}

func refusalOf(v ShareVerdict) string {
	if v.Verdict == ShareFound {
		return "ClamAV found " + v.Signature + " in this file, so it is not read; tell the person"
	}
	return ""
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
