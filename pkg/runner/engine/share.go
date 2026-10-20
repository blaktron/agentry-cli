package engine

// A persistent agent's folders on its tools plane: its data volume, and the
// person's share folder, read-write and live (spec.CapSummary.AgentFiles).
// When the plan checks the share (AgentFiles.CheckShare), the runner checks
// what is new or changed there with ClamAV before each turn, through the
// decode step the inputs take, and writes the verdicts into a folder the
// tools server reads (toolsrv.ShareChecks); the tools server reads a share
// file only when its bytes have a verdict that is not a finding.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// ShareChecksLog is the receipt each check of the share appends to: one
// JSON line a file checked.
const ShareChecksLog = "share-checks.ndjson"

// maxShareCheck bounds one check: the files it takes (the decode step's
// own cap) and the bytes it copies.
const (
	maxShareCheckFiles = runnerdecode.DefaultMaxFiles
	maxShareCheckBytes = 512 << 20
)

// preparePersistentFolders holds a segment to its plan's folders, and gives
// the run's directories the share, the data volume and the verdicts folder.
func preparePersistentFolders(p Params, rd *runDirs) error {
	af := p.Plan.Capabilities.AgentFiles
	if af == nil {
		if p.Persistent != nil && p.Persistent.Share != "" {
			return Classify(ExitCompile, errors.New("the segment was given a share folder its plan does not mount"))
		}
		return nil
	}
	if p.Persistent == nil {
		return Classify(ExitCompile, errors.New("a plan with a persistent agent's folders runs only as that agent's segment"))
	}
	rd.data = p.Persistent.DataVolume
	if af.Share != (p.Persistent.Share != "") {
		return Classify(ExitCompile, errors.New("the segment's share folder and its plan disagree: recompile"))
	}
	if !af.Share {
		return nil
	}
	if err := ShareFolderUsable(p.Persistent.Share); err != nil {
		return Classify(ExitCLI, err)
	}
	if err := gatewayCanMount(p.Persistent.Share, gatewayOS); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the share folder %s: %w", p.Persistent.Share, err))
	}
	rd.share = p.Persistent.Share
	if af.CheckShare {
		rd.shareChecks = filepath.Join(rd.cfg, "share-checks")
		if err := os.MkdirAll(rd.shareChecks, 0o755); err != nil {
			return Classify(ExitCLI, fmt.Errorf("the share's verdicts folder: %w", err))
		}
		rd.shareCache = p.Persistent.ShareVerdicts
		rd.shareKept = readShareCache(rd.shareCache, rd.share)
		if err := writeShareVerdicts(rd.shareChecks, rd.shareKept); err != nil {
			return Classify(ExitCLI, err)
		}
	}
	return nil
}

// shareCache is the verdicts kept between segments, for one share folder.
type shareCache struct {
	Share string                          `json:"share"`
	Files map[string]toolsrv.ShareVerdict `json:"files"`
}

// readShareCache is the verdicts kept for share; none when the file is
// missing, unreadable or for another folder.
func readShareCache(path, share string) map[string]toolsrv.ShareVerdict {
	out := map[string]toolsrv.ShareVerdict{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path) //nolint:gosec // the agent's own state file
	if err != nil {
		return out
	}
	var c shareCache
	if json.Unmarshal(b, &c) != nil || c.Share != share {
		return out
	}
	for rel, v := range c.Files {
		if v.Verdict == toolsrv.ShareClean || v.Verdict == toolsrv.ShareFound || v.Verdict == toolsrv.ShareUnchecked {
			out[rel] = v
		}
	}
	return out
}

// keepShareCache writes the verdicts for the next segment (0600).
func keepShareCache(path, share string, files map[string]toolsrv.ShareVerdict) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(shareCache{Share: share, Files: files})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ShareFolderUsable says whether dir can be a share folder as it stands: an
// absolute path to a folder that is not a link. Where it may be is the
// caller's to say (the persistent package refuses the runner's own folders
// and the system's); this is the engine's last look before it binds it.
func ShareFolderUsable(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("the share folder %q is not a clean absolute path", dir)
	}
	// The gateway's bind lists are joined with colons.
	if strings.Contains(strings.TrimPrefix(dir, filepath.VolumeName(dir)), ":") {
		return fmt.Errorf("the share folder %q has a colon in its path", dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the share folder %s: %w", dir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("the share folder %s is a link; name the folder it points to", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("the share folder %s is not a folder", dir)
	}
	return nil
}

// writeShareVerdicts replaces the verdicts file in dir in one rename, so the
// tools server never reads half of it.
func writeShareVerdicts(dir string, files map[string]toolsrv.ShareVerdict) error {
	b, err := json.Marshal(toolsrv.ShareVerdicts{Version: toolsrv.ShareVerdictsVersion, Files: files})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".verdicts-*")
	if err != nil {
		return fmt.Errorf("the share's verdicts: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the share's verdicts: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, spec.ShareChecksFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the share's verdicts: %w", err)
	}
	return nil
}

// shareChecker is a segment's check of its share: the verdicts so far, by
// each file's path beneath the share, and the decode image once ensured.
type shareChecker struct {
	verdicts map[string]toolsrv.ShareVerdict
	img      *decode.Image
	seq      int
}

// shareFile is one regular file of the share as the walk found it.
type shareFile struct {
	rel   string
	size  int64
	mtime int64
}

// walkShare lists the share's regular files, links and anything else left
// out, in a fixed order.
func walkShare(share string) ([]shareFile, error) {
	var out []shareFile
	err := filepath.WalkDir(share, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == share {
				return err
			}
			return nil // a folder that cannot be read is left out
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(share, p)
		if err != nil {
			return nil
		}
		out = append(out, shareFile{rel: filepath.ToSlash(rel), size: fi.Size(), mtime: fi.ModTime().UnixNano()})
		return nil
	})
	slices.SortFunc(out, func(a, b shareFile) int { return strings.Compare(a.rel, b.rel) })
	return out, err
}

// checkShare checks what is new or changed in the share since the last
// check and rewrites the verdicts; a file that went is dropped. It runs
// before a turn reaches the harness, and a check that fails leaves the files
// it was given without a verdict, so they stay unread, and says so in the
// session. The caller holds turnMu.
func (s *liveSession) checkShare(ctx context.Context) {
	rd := s.rd
	if rd == nil || rd.shareChecks == "" {
		return
	}
	if s.shares == nil {
		s.shares = &shareChecker{verdicts: rd.shareKept}
		if s.shares.verdicts == nil {
			s.shares.verdicts = map[string]toolsrv.ShareVerdict{}
		}
	}
	sc := s.shares
	files, err := walkShare(rd.share)
	if err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "the share folder could not be read for its check: " + s.redact.String(err.Error())})
		return
	}
	present := map[string]bool{}
	var todo []shareFile
	var bytes int64
	left := 0
	for _, f := range files {
		present[f.rel] = true
		if v, ok := sc.verdicts[f.rel]; ok && v.Size == f.size && v.MTimeNano == f.mtime {
			continue
		}
		if len(todo) >= maxShareCheckFiles || bytes+f.size > maxShareCheckBytes {
			left++
			continue
		}
		todo = append(todo, f)
		bytes += f.size
	}
	gone := false
	for rel := range sc.verdicts {
		if !present[rel] {
			delete(sc.verdicts, rel)
			gone = true
		}
	}
	if len(todo) == 0 {
		if gone {
			_ = writeShareVerdicts(rd.shareChecks, sc.verdicts)
			_ = keepShareCache(rd.shareCache, rd.share, sc.verdicts)
		}
		return
	}
	s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("checking %d new or changed file(s) in the share for malware", len(todo))})
	checked, err := s.checkShareFiles(ctx, todo)
	if err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "the share's new files could not be checked, so they stay unread this turn: " + s.redact.String(err.Error())})
	}
	unchecked := 0
	for rel, v := range checked {
		if v.Verdict == "" {
			delete(checked, rel)
			continue
		}
		if v.Verdict == toolsrv.ShareUnchecked {
			unchecked++
		}
		sc.verdicts[rel] = v
	}
	if err := writeShareVerdicts(rd.shareChecks, sc.verdicts); err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "the share's verdicts could not be written: " + s.redact.String(err.Error())})
		return
	}
	_ = keepShareCache(rd.shareCache, rd.share, sc.verdicts)
	s.logShareChecks(checked)
	if unchecked > 0 {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%d file(s) in the share could not be checked by ClamAV and are read unchecked; receipts/%s says why", unchecked, ShareChecksLog)})
	}
	if left > 0 {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%d more file(s) in the share wait for the next turn's check", left)})
	}
}

// checkShareFiles copies the files into a folder of the run's own, runs the
// decode step's malware check over the copies, and answers a verdict for
// each copy's bytes.
func (s *liveSession) checkShareFiles(ctx context.Context, todo []shareFile) (map[string]toolsrv.ShareVerdict, error) {
	rd, sc := s.rd, s.shares
	work := filepath.Join(rd.root, "share-check", strconv.Itoa(sc.seq))
	sc.seq++
	defer func() { _ = os.RemoveAll(work) }()
	in, out := filepath.Join(work, "in"), filepath.Join(work, "out")
	for _, d := range []string{in, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(rd.share)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	copied := map[string]toolsrv.ShareVerdict{}
	for _, f := range todo {
		sum, err := copyShareFile(root, f.rel, filepath.Join(in, filepath.FromSlash(f.rel)))
		if err != nil {
			continue // gone or changed under the walk: checked next turn
		}
		copied[f.rel] = toolsrv.ShareVerdict{SHA256: sum, Size: f.size, MTimeNano: f.mtime}
	}
	if len(copied) == 0 {
		return nil, nil
	}
	img, err := s.shareDecodeImage(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runDecode(ctx, s.d, *img, in, out, s.p.limits(), decode.Options{}, io.Discard)
	if err != nil {
		return nil, err
	}
	byPath := map[string][]runnerdecode.File{}
	for _, f := range res.Report.Files {
		top := f.Path
		if f.In != "" {
			top = f.In
		}
		for top != "" {
			if _, ok := copied[top]; ok {
				break
			}
			if i := strings.LastIndex(top, "/"); i > 0 && path.Clean(top) == top {
				top = top[:i]
				continue
			}
			top = ""
		}
		if top != "" {
			byPath[top] = append(byPath[top], f)
		}
	}
	for rel, v := range copied {
		v.Verdict, v.Signature, v.Reason = shareVerdictOf(byPath[rel], img.Signatures)
		copied[rel] = v
	}
	return copied, nil
}

// shareVerdictOf is a file's verdict from what the check made of it and of
// anything unpacked from it: found when ClamAV found anything in either,
// clean when it read the file and found nothing, unchecked otherwise.
func shareVerdictOf(files []runnerdecode.File, sigs clamdb.DB) (verdict, signature, reason string) {
	for _, f := range files {
		if f.Check == runnerdecode.CheckFound {
			sig := f.Signature
			if sig == "" {
				sig = "a signature"
			}
			return toolsrv.ShareFound, sig, ""
		}
	}
	for _, f := range files {
		if f.In == "" && f.Check == runnerdecode.CheckClean {
			return toolsrv.ShareClean, "", ""
		}
	}
	if !sigs.Held() {
		return toolsrv.ShareUnchecked, "", "no ClamAV signatures on this computer: " + sigs.Problem
	}
	for _, f := range files {
		if f.In == "" && f.Unchecked != "" {
			return toolsrv.ShareUnchecked, "", string(f.Unchecked)
		}
	}
	// The check named no result for the file: it gets no verdict, so it
	// stays unread and is checked again at the next turn.
	return "", "", ""
}

// shareDecodeImage is the decode image with ClamAV's signatures, ensured on
// the segment's first check.
func (s *liveSession) shareDecodeImage(ctx context.Context) (*decode.Image, error) {
	sc := s.shares
	if sc.img != nil {
		return sc.img, nil
	}
	if s.p.Policy == nil {
		return nil, errors.New("no policy names the files image")
	}
	img, err := ensureDecode(ctx, s.d, s.p.Plan.Proxy.Image, s.p.Policy, filepath.Join(s.rd.root, "image"), s.p.Stderr)
	if err != nil {
		return nil, err
	}
	home := s.p.RunnerHome
	if home == "" {
		home, _ = policy.Home()
	}
	img.Signatures = ensureSignatures(ctx, home, s.p.Policy.Images.ClamAVSignaturesRef(), s.p.Stderr)
	sc.img = &img
	return sc.img, nil
}

// copyShareFile copies rel, a regular file beneath the share, to dest, through
// the share's root so a link swapped in cannot take the copy out of it, and
// answers the copy's SHA-256.
func copyShareFile(root *os.Root, rel, dest string) (string, error) {
	fi, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	src, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // the run's own folder
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, cerr := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, maxShareCheckBytes+1))
	if err := dst.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// logShareChecks appends the verdicts to the segment's receipt.
func (s *liveSession) logShareChecks(checked map[string]toolsrv.ShareVerdict) {
	if len(checked) == 0 {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.rd.receiptsDir, ShareChecksLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt the run owns
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	rels := make([]string, 0, len(checked))
	for rel := range checked {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	now := time.Now().UTC()
	for _, rel := range rels {
		v := checked[rel]
		line, err := json.Marshal(struct {
			Time time.Time `json:"time"`
			Path string    `json:"path"`
			toolsrv.ShareVerdict
		}{now, rel, v})
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
		}
		if v.Verdict == toolsrv.ShareFound {
			s.emit(session.Event{Kind: session.KindNotice, Text: "ClamAV found " + v.Signature + " in share/" + rel + "; it will not be read"})
		}
	}
}
