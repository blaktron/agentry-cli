package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// shareSession is a segment's session with a checked share and nothing
// else: what checkShare needs.
func shareSession(t *testing.T) (*liveSession, string) {
	t.Helper()
	root, share := t.TempDir(), t.TempDir()
	rd := runDirsAt(root)
	rd.share, rd.shareChecks, rd.receiptsDir = share, filepath.Join(root, "cfg", "share-checks"), filepath.Join(root, "receipts")
	rd.shareTextKept = filepath.Join(t.TempDir(), "share-text")
	for _, d := range []string{filepath.Join(rd.shareChecks, toolsrv.ShareTextDir), rd.receiptsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pol := &policy.Policy{}
	plan := &spec.Plan{}
	plan.Capabilities.AgentFiles = &spec.AgentFiles{Share: true, CheckShare: true, MalwareCheck: true}
	return &liveSession{
		p:      Params{Plan: plan, Policy: pol, RunnerHome: t.TempDir()},
		rd:     rd,
		events: make(chan session.Event, 1024),
		done:   make(chan struct{}),
	}, share
}

func readVerdicts(t *testing.T, s *liveSession) map[string]toolsrv.ShareVerdict {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.rd.shareChecks, spec.ShareChecksFile))
	if err != nil {
		t.Fatal(err)
	}
	var v toolsrv.ShareVerdicts
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Files
}

func decodeRuns() int {
	decodeCalls.Lock()
	defer decodeCalls.Unlock()
	return decodeCalls.runs
}

// TestShareCheckWritesAVerdictPerFile: the check before a turn gives each
// new file in the share ClamAV's verdict, found or clean, for the bytes it
// copied; an unchanged file is not checked again, a changed one is, and one
// that went is dropped.
func TestShareCheckWritesAVerdictPerFile(t *testing.T) {
	s, share := shareSession(t)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(share, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.md", "the person's notes\n")
	write("in/eicar.txt", "X5O EICAR\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(share, "link")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.checkShare(ctx)
	v := readVerdicts(t, s)
	if v["notes.md"].Verdict != toolsrv.ShareClean || v["notes.md"].SHA256 == "" {
		t.Fatalf("notes.md: %+v", v["notes.md"])
	}
	if v["in/eicar.txt"].Verdict != toolsrv.ShareFound || !strings.Contains(v["in/eicar.txt"].Signature, "Eicar") {
		t.Fatalf("in/eicar.txt: %+v", v["in/eicar.txt"])
	}
	if _, ok := v["link"]; ok {
		t.Fatal("a link in the share was checked")
	}
	runs := decodeRuns()
	s.checkShare(ctx)
	if decodeRuns() != runs {
		t.Fatal("an unchanged share was checked again")
	}
	// A file whose size or time of last change moved is checked again, though
	// the other is as it was.
	notes := filepath.Join(share, "notes.md")
	then := time.Unix(1_700_000_000, 0)
	for name, c := range map[string]struct {
		body    string
		sameMod bool
	}{"another time": {"the person's notes\n", false}, "another size": {"the person's longer notes\n", true}} {
		before, err := os.Stat(notes)
		if err != nil {
			t.Fatal(err)
		}
		write("notes.md", c.body)
		mod := then.Add(time.Hour * time.Duration(len(name)))
		if c.sameMod {
			mod = before.ModTime()
		}
		if err := os.Chtimes(notes, mod, mod); err != nil {
			t.Fatal(err)
		}
		was := decodeRuns()
		s.checkShare(ctx)
		if decodeRuns() != was+1 {
			t.Errorf("%s: the file was not checked again", name)
		}
	}
	runs = decodeRuns()
	time.Sleep(10 * time.Millisecond)
	write("notes.md", "changed\n")
	if err := os.Remove(filepath.Join(share, "in", "eicar.txt")); err != nil {
		t.Fatal(err)
	}
	s.checkShare(ctx)
	v = readVerdicts(t, s)
	if decodeRuns() != runs+1 || v["notes.md"].Verdict != toolsrv.ShareClean || len(v) != 1 {
		t.Fatalf("after a change and a removal: %d runs, %+v", decodeRuns()-runs, v)
	}
	log, err := os.ReadFile(filepath.Join(s.rd.receiptsDir, ShareChecksLog))
	if err != nil || strings.Count(string(log), "\n") != 5 {
		t.Fatalf("the receipt holds %q (%v); want a line a check of a file", log, err)
	}
}

// TestShareFolderUsable: the engine binds a clean absolute path to a folder,
// never a link or a file.
func TestShareFolderUsable(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ShareFolderUsable(dir); err != nil {
		t.Fatalf("a folder: %v", err)
	}
	bads := []string{link, file, "relative/share", ".", dir + ":/etc", dir + "/../" + filepath.Base(dir), filepath.Join(dir, "missing")}
	// A folder that exists with a colon in its name, where a file system takes one.
	if coloned := filepath.Join(dir, "a:b"); os.Mkdir(coloned, 0o755) == nil {
		bads = append(bads, coloned)
	}
	for _, bad := range bads {
		if err := ShareFolderUsable(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// TestShareVerdictsAreKeptBetweenSegments: a segment starts from the
// verdicts the last one kept for the same share, so an unchanged file is
// not checked again; verdicts kept for another folder are not used.
func TestShareVerdictsAreKeptBetweenSegments(t *testing.T) {
	s, share := shareSession(t)
	cache := filepath.Join(t.TempDir(), "share-verdicts.json")
	s.rd.shareCache = cache
	if err := os.WriteFile(filepath.Join(share, "notes.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.checkShare(context.Background())
	runs := decodeRuns()
	next, _ := shareSession(t)
	next.rd.share, next.rd.shareCache = share, cache
	next.rd.shareKept = readShareCache(cache, share, true)
	next.checkShare(context.Background())
	if decodeRuns() != runs || next.shares.verdicts["notes.md"].Verdict != toolsrv.ShareClean {
		t.Fatalf("the next segment checked again (%d runs) or lost the verdict: %+v", decodeRuns()-runs, next.shares.verdicts)
	}
	if kept := readShareCache(cache, filepath.Join(share, "other"), true); len(kept) != 0 {
		t.Fatalf("verdicts kept for another folder were used: %+v", kept)
	}
	// Only a verdict of a kind the check gives is used: clean, found or
	// unchecked.
	mixed := filepath.Join(t.TempDir(), "mixed.json")
	if err := keepShareCache(mixed, share, true, map[string]toolsrv.ShareVerdict{
		"clean.md": {Verdict: toolsrv.ShareClean}, "found.exe": {Verdict: toolsrv.ShareFound}, "unchecked.md": {Verdict: toolsrv.ShareUnchecked},
		"odd.md": {Verdict: "trusted"}, "none.md": {},
	}); err != nil {
		t.Fatal(err)
	}
	kept := readShareCache(mixed, share, true)
	if len(kept) != 3 || kept["clean.md"].Verdict != toolsrv.ShareClean || kept["found.exe"].Verdict != toolsrv.ShareFound || kept["unchecked.md"].Verdict != toolsrv.ShareUnchecked {
		t.Fatalf("the verdicts used: %+v", kept)
	}
}

// TestShareFilesUncheckedWithoutSignatures: with no signatures on the
// machine a file's verdict is unchecked, with the reason, and the chat is
// told it is read unchecked.
func TestShareFilesUncheckedWithoutSignatures(t *testing.T) {
	decodeCalls.Lock()
	signaturesOff = true
	decodeCalls.Unlock()
	t.Cleanup(func() {
		decodeCalls.Lock()
		signaturesOff = false
		decodeCalls.Unlock()
	})
	s, share := shareSession(t)
	if err := os.WriteFile(filepath.Join(share, "a.md"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.checkShare(context.Background())
	v := readVerdicts(t, s)["a.md"]
	if v.Verdict != toolsrv.ShareUnchecked || !strings.Contains(v.Reason, "no ClamAV signatures") {
		t.Fatalf("verdict %+v", v)
	}
	told := false
	for len(s.events) > 0 {
		ev := <-s.events
		told = told || ev.Kind == session.KindNotice && strings.Contains(ev.Text, "read unchecked")
	}
	if !told {
		t.Error("the chat was not told the file is read unchecked")
	}

	// The verdict of a file from what the check made of it and of what is
	// inside it: found anywhere is found; clean only by the file itself; a
	// file the check did not take is unchecked, with its reason, when the
	// signatures are held; with the check off every top-level file is unchecked.
	top := func(check runnerdecode.Check) runnerdecode.File {
		return runnerdecode.File{Path: "a.zip", Check: check}
	}
	member := func(check runnerdecode.Check, sig string) runnerdecode.File {
		return runnerdecode.File{Path: "a.zip/x", In: "a.zip", Check: check, Signature: sig}
	}
	skipped := top(runnerdecode.CheckUnchecked)
	skipped.Unchecked = runnerdecode.UncheckedNotExecutable
	held, none := testSignatures, clamdb.DB{Problem: "no bundle"}
	for name, c := range map[string]struct {
		files                   []runnerdecode.File
		sigs                    clamdb.DB
		check                   bool
		verdict, signature, why string
	}{
		"clean":                  {[]runnerdecode.File{top(runnerdecode.CheckClean)}, held, true, toolsrv.ShareClean, "", ""},
		"found in a member":      {[]runnerdecode.File{top(runnerdecode.CheckClean), member(runnerdecode.CheckFound, "Eicar")}, held, true, toolsrv.ShareFound, "Eicar", ""},
		"found with no name":     {[]runnerdecode.File{member(runnerdecode.CheckFound, "")}, held, true, toolsrv.ShareFound, "a signature", ""},
		"only a member clean":    {[]runnerdecode.File{skipped, member(runnerdecode.CheckClean, "")}, held, true, toolsrv.ShareUnchecked, "", string(runnerdecode.UncheckedNotExecutable)},
		"not taken by the check": {[]runnerdecode.File{skipped}, held, true, toolsrv.ShareUnchecked, "", string(runnerdecode.UncheckedNotExecutable)},
		"no signatures":          {[]runnerdecode.File{skipped}, none, true, toolsrv.ShareUnchecked, "", "no ClamAV signatures on this computer: no bundle"},
		"nothing to say":         {[]runnerdecode.File{member(runnerdecode.CheckClean, "")}, held, true, "", "", ""},
		"the check off":          {[]runnerdecode.File{top(runnerdecode.CheckClean)}, none, false, toolsrv.ShareUnchecked, "", shareCheckOff},
		"the check off, members": {[]runnerdecode.File{member(runnerdecode.CheckClean, "")}, none, false, "", "", ""},
	} {
		verdict, signature, why := shareVerdictOf(c.files, c.sigs, c.check)
		if verdict != c.verdict || signature != c.signature || why != c.why {
			t.Errorf("%s: %q, %q, %q; want %q, %q, %q", name, verdict, signature, why, c.verdict, c.signature, c.why)
		}
	}
}

// TestFilterConfigAdmitsTheSegmentsFolders: a segment's filter admits its
// data volume to the tools server by name and takes the share as a
// writable root of its own, from the gateway's lists.
func TestFilterConfigAdmitsTheSegmentsFolders(t *testing.T) {
	rd := runDirsAt(t.TempDir())
	rd.data, rd.share = "agentry-persistent-assistant-data", "/home/p/agentry/assistantshare"
	env := map[string]string{
		"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS":        rd.workspace + ":" + rd.share,
		"MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS": rd.outDir + ":" + rd.share,
	}
	cfg, err := filterConfig(rd, "run1", env, policy.Limits{}, &spec.Plan{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ToolsVolumes) != 1 || cfg.ToolsVolumes[0] != rd.data {
		t.Errorf("tools volumes %v", cfg.ToolsVolumes)
	}
	if !slicesContains(cfg.WritableRoots, rd.share) || !slicesContains(cfg.BindRoots, rd.share) {
		t.Errorf("roots %v, writable %v", cfg.BindRoots, cfg.WritableRoots)
	}
	// The share is the tools server's to write, its alone to bind, and no
	// container's that joins a network.
	share := rd.gatewayPath(rd.share)
	if !slicesContains(cfg.ToolsWritableRoots, share) || !slicesContains(cfg.ToolsOnlyRoots, share) || !slicesContains(cfg.OfflineRoots, share) {
		t.Errorf("tools writable %v, tools only %v, offline %v; each should hold %s", cfg.ToolsWritableRoots, cfg.ToolsOnlyRoots, cfg.OfflineRoots, share)
	}
	// A server the plan lets see the folder itself binds it too: it is no
	// longer the tools server's alone.
	live := &spec.Plan{MCPServers: []spec.MCPServerSpec{{Name: "mcp-pypi-x", Source: "pypi", Share: spec.ServerShareLive}}}
	cfg, err = filterConfig(rd, "run1", env, policy.Limits{}, live, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slicesContains(cfg.ToolsOnlyRoots, share) || !slicesContains(cfg.ToolsWritableRoots, share) {
		t.Errorf("a plan whose server sees the share: tools only %v, tools writable %v", cfg.ToolsOnlyRoots, cfg.ToolsWritableRoots)
	}
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// writeShare writes a file of the share.
func writeShare(t *testing.T, share, rel, body string) {
	t.Helper()
	p := filepath.Join(share, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// keptText is the text kept for a verdict in dir, or "" with none.
func keptText(dir string, v toolsrv.ShareVerdict) string {
	b, err := os.ReadFile(filepath.Join(dir, toolsrv.ShareTextName(v.SHA256)))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestShareCheckKeepsEachDocumentsText: the check keeps the text the decode
// step read out of each document, in the run's text folder and the agent's
// own, under the digest of the bytes it checked, naming what read it; a text
// file keeps none, a file no reader could read says why in the decode step's
// words, and a document that went takes its text with it.
func TestShareCheckKeepsEachDocumentsText(t *testing.T) {
	s, share := shareSession(t)
	writeShare(t, share, "cv.docx", "a word file")
	writeShare(t, share, "scan.png", "an image")
	writeShare(t, share, "papers/report.pdf", "a pdf")
	writeShare(t, share, "notes.md", "notes\n")
	writeShare(t, share, "blob.bin", "\x00\x01\xff")
	s.checkShare(context.Background())
	v := readVerdicts(t, s)
	if b := v["blob.bin"]; b.Text || b.Unread == "" {
		t.Errorf("a file no reader reads: %+v", b)
	}
	run := filepath.Join(s.rd.shareChecks, toolsrv.ShareTextDir)
	for rel, tool := range map[string]string{"cv.docx": "pandoc", "scan.png": "tesseract", "papers/report.pdf": "pdftotext"} {
		got := v[rel]
		if got.Verdict != toolsrv.ShareClean || !got.Text || got.Tool != tool || got.Unread != "" {
			t.Errorf("%s: %+v", rel, got)
		}
		base := filepath.Base(rel)
		if text := keptText(run, got); !strings.Contains(text, tool+" read "+base) {
			t.Errorf("%s: the run's text is %q", rel, text)
		}
		if keptText(s.rd.shareTextKept, got) != keptText(run, got) {
			t.Errorf("%s: the agent's kept text differs from the run's", rel)
		}
	}
	if n := v["notes.md"]; n.Text || n.Unread != "" || n.Verdict != toolsrv.ShareClean {
		t.Errorf("a text file: %+v", n)
	}
	// A document ClamAV finds is never read: it has no text and no reason it
	// went unread.
	writeShare(t, share, "bad.docx", "X5O EICAR a word file")
	s.checkShare(context.Background())
	if b := readVerdicts(t, s)["bad.docx"]; b.Verdict != toolsrv.ShareFound || b.Text || b.Unread != "" || b.Tool != "" {
		t.Errorf("a document ClamAV finds: %+v", b)
	}
	// Past the bytes one check reads, a file waits for the next turn.
	big, err := os.Create(filepath.Join(share, "zz-big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sparse := big.Truncate(maxShareCheckBytes+1) == nil // a file system that takes a sparse file
	_ = big.Close()
	if !sparse {
		_ = os.Remove(filepath.Join(share, "zz-big.bin"))
	}
	writeShare(t, share, "zz-new.md", "new\n")
	s.checkShare(context.Background())
	if got := readVerdicts(t, s); got["zz-new.md"].Verdict != toolsrv.ShareClean {
		t.Errorf("the small file beside it was not checked: %+v", got["zz-new.md"])
	} else if _, ok := got["zz-big.bin"]; ok {
		t.Error("a file past the bytes a check reads was checked")
	}
	waiting := false
	for len(s.events) > 0 {
		ev := <-s.events
		waiting = waiting || ev.Kind == session.KindNotice && strings.Contains(ev.Text, "1 more file(s) in the share wait for the next turn's check")
	}
	if sparse && !waiting {
		t.Error("the chat was not told a file waits for the next turn")
	}
	if sparse {
		if err := os.Remove(filepath.Join(share, "zz-big.bin")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(share, "cv.docx")); err != nil {
		t.Fatal(err)
	}
	gone := v["cv.docx"]
	s.checkShare(context.Background())
	if keptText(run, gone) != "" || keptText(s.rd.shareTextKept, gone) != "" {
		t.Error("the text of a document that went is still kept")
	}
	if keptText(run, v["scan.png"]) == "" {
		t.Error("the text of a document still there was removed")
	}
}

// TestShareCheckOffDecodesAndMarksUnchecked: with the plan's malware check
// off the check fetches no signatures and runs no ClamAV, still reads each
// document's text, and marks every file unchecked with the reason, a file
// ClamAV would have found among them; the chat is never told a file was
// checked for malware.
func TestShareCheckOffDecodesAndMarksUnchecked(t *testing.T) {
	s, share := shareSession(t)
	s.p.Plan.Capabilities.AgentFiles.MalwareCheck = false
	writeShare(t, share, "cv.docx", "a word file")
	writeShare(t, share, "eicar.txt", "X5O EICAR\n")
	s.checkShare(context.Background())
	v := readVerdicts(t, s)
	for _, rel := range []string{"cv.docx", "eicar.txt"} {
		if v[rel].Verdict != toolsrv.ShareUnchecked || v[rel].Reason != "the malware check is off on this computer" {
			t.Errorf("%s: %+v", rel, v[rel])
		}
	}
	if !v["cv.docx"].Text || v["cv.docx"].Tool != "pandoc" {
		t.Errorf("the document's text was not kept: %+v", v["cv.docx"])
	}
	if s.shares.img.Signatures.Held() {
		t.Error("signatures were fetched with the check off")
	}
	for len(s.events) > 0 {
		if ev := <-s.events; strings.Contains(ev.Text, "for malware") || strings.Contains(ev.Text, "could not be checked by ClamAV") {
			t.Errorf("the chat was told %q with the check off", ev.Text)
		}
	}
}

// TestKeptShareTextFollowsTheNextSegment: a segment starts with the text
// the last one kept for each document; a kept verdict whose text is gone,
// and verdicts kept before the text was, are dropped, so their files are
// checked, and read, again.
func TestKeptShareTextFollowsTheNextSegment(t *testing.T) {
	s, share := shareSession(t)
	cache := filepath.Join(t.TempDir(), "share-verdicts.json")
	s.rd.shareCache = cache
	writeShare(t, share, "cv.docx", "a word file")
	writeShare(t, share, "report.pdf", "a pdf")
	s.checkShare(context.Background())
	first := readVerdicts(t, s)
	if err := os.Remove(filepath.Join(s.rd.shareTextKept, toolsrv.ShareTextName(first["report.pdf"].SHA256))); err != nil {
		t.Fatal(err)
	}

	next, _ := shareSession(t)
	next.rd.share, next.rd.shareCache, next.rd.shareTextKept = share, cache, s.rd.shareTextKept
	next.rd.shareKept = readShareCache(cache, share, true)
	restoreShareText(next.rd)
	if _, ok := next.rd.shareKept["report.pdf"]; ok {
		t.Fatal("a kept verdict whose text is gone was kept")
	}
	if got := keptText(filepath.Join(next.rd.shareChecks, toolsrv.ShareTextDir), first["cv.docx"]); !strings.Contains(got, "pandoc read cv.docx") {
		t.Fatalf("the next segment's text for cv.docx: %q", got)
	}
	runs := decodeRuns()
	next.checkShare(context.Background())
	if decodeRuns() != runs+1 || !readVerdicts(t, next)["report.pdf"].Text {
		t.Fatalf("the document whose text was gone was not read again: %d runs, %+v", decodeRuns()-runs, readVerdicts(t, next))
	}

	// A kept text is linked or copied only from a regular file of the run, and
	// a share file is copied only when it is one.
	folder := t.TempDir()
	if err := linkOrCopy(folder, filepath.Join(t.TempDir(), "text")); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("a folder was kept as a text: %v", err)
	}
	if err := os.Symlink(filepath.Join(share, "cv.docx"), filepath.Join(folder, "link")); err == nil {
		root, err := os.OpenRoot(folder)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := linkOrCopy(filepath.Join(folder, "link"), filepath.Join(t.TempDir(), "text")); err == nil {
			t.Error("a link was kept as a text")
		}
		if _, err := copyShareFile(root, "link", filepath.Join(t.TempDir(), "copy")); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Errorf("a link in the share was copied: %v", err)
		}
	}
	// A kept text whose digest is not a plain one is never looked for.
	odd := "NotAHexDigest"
	if err := os.MkdirAll(next.rd.shareTextKept, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(next.rd.shareTextKept, toolsrv.ShareTextName(odd)), "a text\n")
	next.rd.shareKept = map[string]toolsrv.ShareVerdict{"odd.md": {Verdict: toolsrv.ShareClean, SHA256: odd, Text: true}}
	restoreShareText(next.rd)
	if _, ok := next.rd.shareKept["odd.md"]; ok {
		t.Error("a kept verdict with a digest that is not plain was restored")
	}
	good := strings.Repeat("ab", 32)
	for digest, want := range map[string]bool{good: true, strings.ToUpper(good): false, good[1:]: false, good + "a": false, strings.Repeat("g", 64): false, "": false} {
		if got := plainDigest(digest); got != want {
			t.Errorf("plainDigest(%q) = %v, want %v", digest, got, want)
		}
	}

	// Verdicts kept before the text was are not used.
	old, err := json.Marshal(map[string]any{"share": share, "files": first})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if kept := readShareCache(cache, share, true); len(kept) != 0 {
		t.Fatalf("verdicts of an older version were used: %+v", kept)
	}
}

// TestTurningTheCheckOnChecksWhatWasReadUnchecked: verdicts kept under one
// malware-check setting are not used under the other, so a file read
// unchecked while the check was off is checked with ClamAV, and found,
// once it is on.
func TestTurningTheCheckOnChecksWhatWasReadUnchecked(t *testing.T) {
	s, share := shareSession(t)
	s.p.Plan.Capabilities.AgentFiles.MalwareCheck = false
	cache := filepath.Join(t.TempDir(), "share-verdicts.json")
	s.rd.shareCache = cache
	writeShare(t, share, "eicar.txt", "X5O EICAR\n")
	s.checkShare(context.Background())
	if v := readVerdicts(t, s)["eicar.txt"]; v.Verdict != toolsrv.ShareUnchecked {
		t.Fatalf("with the check off: %+v", v)
	}
	if kept := readShareCache(cache, share, true); len(kept) != 0 {
		t.Fatalf("verdicts made with the check off were kept for the check on: %+v", kept)
	}
	next, _ := shareSession(t)
	next.rd.share, next.rd.shareCache = share, cache
	next.rd.shareKept = readShareCache(cache, share, true)
	next.checkShare(context.Background())
	if v := readVerdicts(t, next)["eicar.txt"]; v.Verdict != toolsrv.ShareFound {
		t.Fatalf("with the check turned on: %+v", v)
	}
	if kept := readShareCache(cache, share, false); len(kept) != 0 {
		t.Fatalf("verdicts made with the check on were kept for the check off: %+v", kept)
	}
}
