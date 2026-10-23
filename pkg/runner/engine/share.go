package engine

// A persistent agent's folders on its tools plane: its data volume, and the
// person's share folder, read-write and live (spec.CapSummary.AgentFiles).
// When the plan checks the share (AgentFiles.CheckShare), the runner runs
// what is new or changed there through the decode step the inputs take
// before each turn, with ClamAV when the plan's malware check is on
// (AgentFiles.MalwareCheck), and writes the verdicts into a folder the tools
// server reads (toolsrv.ShareChecks), with the text the decode step read out
// of each document, by the digest of the bytes it read; the tools server
// reads a share file only when its bytes have a verdict that is not a
// finding, and serves a document by that text.

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
const ShareChecksLog = spec.ShareChecksLog

// maxShareCheck bounds one check: the files it takes (the decode step's
// own cap) and the bytes it copies.
const (
	maxShareCheckFiles = runnerdecode.DefaultMaxFiles
	maxShareCheckBytes = 512 << 20
)

// preparePersistentFolders holds a segment to its plan's folders, and gives
// the run's directories the share, the data volume and the verdicts folder.
func preparePersistentFolders(p Params, rd *runDirs) error {
	if err := prepareAppLink(p, rd); err != nil {
		return err
	}
	af := p.Plan.Capabilities.AgentFiles
	if af == nil {
		if p.Persistent != nil && p.Persistent.Share != "" {
			return Classify(ExitCompile, errors.New("the segment was given a share folder its plan does not mount"))
		}
		if p.Persistent != nil && p.Persistent.Memory != nil {
			return Classify(ExitCompile, errors.New("the segment was given a memory volume its plan does not mount"))
		}
		return nil
	}
	if p.Persistent == nil {
		return Classify(ExitCompile, errors.New("a plan with a persistent agent's folders runs only as that agent's segment"))
	}
	rd.data = p.Persistent.DataVolume
	if m := p.Persistent.Memory; af.Memory != (m != nil) || m != nil && m.Volume == "" {
		return Classify(ExitCompile, errors.New("the segment's memory volume and its plan disagree: recompile"))
	}
	if af.Memory {
		rd.memory = p.Persistent.Memory.Volume
	}
	if err := prepareSkills(p, rd); err != nil {
		return err
	}
	if err := prepareDependencyDB(p, rd); err != nil {
		return err
	}
	if af.Share != (p.Persistent.Share != "") {
		return Classify(ExitCompile, errors.New("the segment's share folder and its plan disagree: recompile"))
	}
	if !af.Share {
		if p.Persistent.ShareMirror != "" || spec.SharesAs(p.Plan.MCPServers, spec.ServerShareMirror) || spec.SharesAs(p.Plan.MCPServers, spec.ServerShareLive) {
			return Classify(ExitCompile, errors.New("the segment's servers are given a share folder its plan does not mount"))
		}
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
		if err := os.MkdirAll(filepath.Join(rd.shareChecks, toolsrv.ShareTextDir), 0o755); err != nil {
			return Classify(ExitCLI, fmt.Errorf("the share's verdicts folder: %w", err))
		}
		rd.shareCache, rd.shareTextKept = p.Persistent.ShareVerdicts, p.Persistent.ShareText
		rd.shareKept = readShareCache(rd.shareCache, rd.share, af.MalwareCheck)
		restoreShareText(rd)
		if err := writeShareVerdicts(rd.shareChecks, rd.shareKept); err != nil {
			return Classify(ExitCLI, err)
		}
	}
	if err := prepareShareMirror(p, rd); err != nil {
		return err
	}
	return prepareShareOut(p, rd)
}

// prepareDependencyDB holds a segment to its plan's dependency scan: OSV's
// databases are given exactly when the plan scans, as a real folder, not
// reached through a link, labelled for the tools server on an SELinux host.
func prepareDependencyDB(p Params, rd *runDirs) error {
	c := p.Plan.Capabilities.Coder
	want := c != nil && c.Commands && c.DependencyScan
	dir := p.Persistent.DependencyDB
	switch {
	case want != (dir != ""):
		return Classify(ExitCompile, errors.New("the segment's dependency databases and its plan disagree: recompile"))
	case !want:
		return nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Classify(ExitCLI, fmt.Errorf("the dependency databases' folder %s is not a clean absolute path", dir))
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return Classify(ExitCLI, fmt.Errorf("the dependency databases' folder %s is not a folder", dir))
	}
	if err := gatewayCanMount(dir, gatewayOS); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the dependency databases' folder %s: %w", dir, err))
	}
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	relabelShared(context.Background(), stderr, dir)
	rd.dependencyDB = dir
	return nil
}

// prepareSkills holds a segment to its plan's skills: the folder is given
// exactly when the plan has a skill, and is a real folder, not reached
// through a link, labelled for the tools server on an SELinux host.
func prepareSkills(p Params, rd *runDirs) error {
	want := len(p.Plan.Capabilities.AgentFiles.Skills) > 0
	dir := p.Persistent.Skills
	switch {
	case want != (dir != ""):
		return Classify(ExitCompile, errors.New("the segment's skills and its plan disagree: recompile"))
	case !want:
		return nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Classify(ExitCLI, fmt.Errorf("the skills folder %s is not a clean absolute path", dir))
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return Classify(ExitCLI, fmt.Errorf("the skills folder %s is not a folder", dir))
	}
	if err := gatewayCanMount(dir, gatewayOS); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the skills folder %s: %w", dir, err))
	}
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	relabelShared(context.Background(), stderr, dir)
	rd.skills = dir
	return nil
}

// prepareShareMirror holds a segment to its plan's servers that see the
// share: the mirror is given exactly when one sees it, only where the share
// is checked, and is made to hold what the kept verdicts passed before any
// server starts; the live share, read-only, only for the Coding agent.
func prepareShareMirror(p Params, rd *runDirs) error {
	af := p.Plan.Capabilities.AgentFiles
	mirror, live := spec.SharesAs(p.Plan.MCPServers, spec.ServerShareMirror), spec.SharesAs(p.Plan.MCPServers, spec.ServerShareLive)
	switch {
	case mirror != (p.Persistent.ShareMirror != ""):
		return Classify(ExitCompile, errors.New("the segment's share mirror and its plan disagree: recompile"))
	case mirror && !af.CheckShare, live && (af.CheckShare || p.Plan.Capabilities.Coder == nil):
		return Classify(ExitCompile, errors.New("a server sees the share folder itself only beside the Coding agent, and its mirror only where the share is checked"))
	case !mirror:
		return nil
	}
	m, err := openShareMirror(p.Persistent.ShareMirror)
	if err != nil {
		return Classify(ExitCLI, err)
	}
	if err := gatewayCanMount(m.dir, gatewayOS); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the share mirror %s: %w", m.dir, err))
	}
	// The mirror is the runner's own, so it is labelled for the servers'
	// containers on an SELinux host, the staging folder too, whose label
	// each copy keeps as it is moved in.
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	relabelShared(context.Background(), stderr, m.dir)
	relabelShared(context.Background(), stderr, m.staging)
	if err := m.sync(rd.share, rd.shareKept); err != nil && p.Stderr != nil {
		_, _ = fmt.Fprintf(p.Stderr, "note: the share mirror: %v\n", err)
	}
	rd.shareMirror = m
	return nil
}

// syncShareMirror brings the share mirror to the verdicts, saying in the
// session what it could not copy.
func (s *liveSession) syncShareMirror() {
	if err := s.rd.shareMirror.sync(s.rd.share, s.shares.verdicts); err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "the share's mirror for the MCP servers could not be brought up to date: " + s.redact.String(err.Error())})
	}
}

// shareCache is the verdicts kept between segments, for one share folder.
type shareCache struct {
	Version int    `json:"version"`
	Share   string `json:"share"`
	// MalwareCheck is the setting the verdicts were made under.
	MalwareCheck bool                            `json:"malwareCheck"`
	Files        map[string]toolsrv.ShareVerdict `json:"files"`
}

// shareCacheVersion is the kept verdicts' version: version 2 holds, beside
// each verdict, whether a text read out of the file is kept. Verdicts kept
// in another version are dropped, so each file is checked, and its text
// read, once more.
const shareCacheVersion = 2

// readShareCache is the verdicts kept for share under the malware-check
// setting malwareCheck; none when the file is missing, unreadable, of
// another version, for another folder or made under the other setting, so
// a file read unchecked while the check was off is checked once it is on.
func readShareCache(path, share string, malwareCheck bool) map[string]toolsrv.ShareVerdict {
	out := map[string]toolsrv.ShareVerdict{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path) //nolint:gosec // the agent's own state file
	if err != nil {
		return out
	}
	var c shareCache
	if json.Unmarshal(b, &c) != nil || c.Version != shareCacheVersion || c.Share != share || c.MalwareCheck != malwareCheck {
		return out
	}
	for rel, v := range c.Files {
		if v.Verdict == toolsrv.ShareClean || v.Verdict == toolsrv.ShareFound || v.Verdict == toolsrv.ShareUnchecked {
			out[rel] = v
		}
	}
	return out
}

// keepShareCache writes the verdicts made under the malware-check setting
// malwareCheck for the next segment (0600).
func keepShareCache(path, share string, malwareCheck bool, files map[string]toolsrv.ShareVerdict) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(shareCache{Version: shareCacheVersion, Share: share, MalwareCheck: malwareCheck, Files: files})
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
			_ = keepShareCache(rd.shareCache, rd.share, s.shareMalwareCheck(), sc.verdicts)
			pruneShareText(rd, sc.verdicts)
			s.syncShareMirror()
		}
		return
	}
	if s.shareMalwareCheck() {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("checking %d new or changed file(s) in the share for malware", len(todo))})
	} else {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("reading %d new or changed file(s) in the share; the malware check is off on this computer", len(todo))})
	}
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
	_ = keepShareCache(rd.shareCache, rd.share, s.shareMalwareCheck(), sc.verdicts)
	pruneShareText(rd, sc.verdicts)
	s.syncShareMirror()
	s.logShareChecks(checked)
	if unchecked > 0 && s.shareMalwareCheck() {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%d file(s) in the share could not be checked by ClamAV and are read unchecked; receipts/%s says why", unchecked, ShareChecksLog)})
	}
	if left > 0 {
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%d more file(s) in the share wait for the next turn's check", left)})
	}
}

// checkShareFiles copies the files into a folder of the run's own, runs the
// decode step over the copies, with its malware check when the plan's is on,
// and answers a verdict for each copy's bytes, keeping the text read out of
// each document that is not a finding (keepShareText).
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
	malwareCheck := s.shareMalwareCheck()
	for rel, v := range copied {
		v.Verdict, v.Signature, v.Reason = shareVerdictOf(byPath[rel], img.Signatures, malwareCheck)
		if v.Verdict != "" && v.Verdict != toolsrv.ShareFound {
			v.Text, v.Tool, v.Unread = keepShareText(rd, res, rel, v.SHA256, byPath[rel])
		}
		// The servers that see the share get the bytes that were checked,
		// from the check's own copy, before it is removed.
		// A copy that fails here is left out of the index, so the sync
		// after the check copies it from the share, held to the same
		// digest.
		if rd.shareMirror != nil && passed(v) {
			_ = rd.shareMirror.put(rel, filepath.Join(in, filepath.FromSlash(rel)), v.SHA256)
		}
		copied[rel] = v
	}
	return copied, nil
}

// shareCheckOff is the verdict's reason for every file of the share while
// the malware check is off.
const shareCheckOff = "the malware check is off on this computer"

// shareMalwareCheck says the plan checks the share's files with ClamAV:
// the policy's malwareCheck, as the plan was compiled with it.
func (s *liveSession) shareMalwareCheck() bool {
	a := s.p.Plan.Capabilities.AgentFiles
	return a != nil && a.MalwareCheck
}

// shareArchiveUnread is why fs_read reads no text for an archive in the
// share.
const shareArchiveUnread = "an archive: the files inside it are not read out of it in the share; unpack it there to read them"

// keepShareText keeps the text the decode step read out of the share file
// rel, whose copy's SHA-256 is sum, where the tools server serves it: the
// run's text folder and the agent's own, both by the digest. files are
// the report's entries for the file and anything unpacked from it. It
// answers whether a text is kept and what read it, or why none could be
// read; a file that is text already keeps none and needs none.
func keepShareText(rd *runDirs, res *decode.Result, rel, sum string, files []runnerdecode.File) (kept bool, tool, unread string) {
	for _, f := range files {
		if f.Path != rel || f.In != "" {
			continue
		}
		switch {
		case f.Outcome == runnerdecode.OutcomeText:
			return false, "", ""
		case f.Outcome == runnerdecode.OutcomeUnpacked:
			return false, "", shareArchiveUnread
		case !f.HasText():
			return false, "", decode.UnreadReason(f)
		}
		dst := filepath.Join(rd.shareChecks, toolsrv.ShareTextDir, toolsrv.ShareTextName(sum))
		if err := res.CopyText(rel, dst); err != nil {
			return false, "", "the text read out of it could not be kept"
		}
		if rd.shareTextKept != "" {
			if err := os.MkdirAll(rd.shareTextKept, 0o700); err == nil {
				_ = linkOrCopy(dst, filepath.Join(rd.shareTextKept, toolsrv.ShareTextName(sum)))
			}
		}
		return true, f.Tool, ""
	}
	return false, "", ""
}

// restoreShareText puts the text kept for each kept verdict into the run's
// text folder; a verdict whose text is gone is dropped, so its file is
// checked, and its text read, again.
func restoreShareText(rd *runDirs) {
	for rel, v := range rd.shareKept {
		if !v.Text {
			continue
		}
		name := toolsrv.ShareTextName(v.SHA256)
		if !plainDigest(v.SHA256) || rd.shareTextKept == "" ||
			linkOrCopy(filepath.Join(rd.shareTextKept, name), filepath.Join(rd.shareChecks, toolsrv.ShareTextDir, name)) != nil {
			delete(rd.shareKept, rel)
		}
	}
}

// pruneShareText removes from the run's text folder and the agent's own
// every text no verdict holds.
func pruneShareText(rd *runDirs, verdicts map[string]toolsrv.ShareVerdict) {
	keep := map[string]bool{}
	for _, v := range verdicts {
		if v.Text {
			keep[toolsrv.ShareTextName(v.SHA256)] = true
		}
	}
	for _, dir := range []string{filepath.Join(rd.shareChecks, toolsrv.ShareTextDir), rd.shareTextKept} {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !keep[e.Name()] {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// plainDigest says sum is a SHA-256 in lower-case hex, so a name made of it
// stays in its folder.
func plainDigest(sum string) bool {
	if len(sum) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil && strings.ToLower(sum) == sum
}

// linkOrCopy makes dst the file src, a regular file: a hard link where the
// two are on one filesystem, else a copy. An existing dst is replaced.
func linkOrCopy(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	_ = os.Remove(dst)
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src) //nolint:gosec // the runner's own text folder
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // read by the tools server
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, decode.MaxTextBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// shareVerdictOf is a file's verdict from what the check made of it and of
// anything unpacked from it: found when ClamAV found anything in either,
// clean when it read the file and found nothing, unchecked otherwise, and
// unchecked for every file when the malware check is off.
func shareVerdictOf(files []runnerdecode.File, sigs clamdb.DB, malwareCheck bool) (verdict, signature, reason string) {
	if !malwareCheck {
		for _, f := range files {
			if f.In == "" {
				return toolsrv.ShareUnchecked, "", shareCheckOff
			}
		}
		return "", "", ""
	}
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

// shareDecodeImage is the decode image, with ClamAV's signatures when the
// malware check is on, ensured on the segment's first check.
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
	if !s.shareMalwareCheck() {
		// No signatures, so the decode step reads the files without
		// ClamAV.
		img.Signatures = clamdb.DB{Problem: shareCheckOff}
		sc.img = &img
		return sc.img, nil
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
