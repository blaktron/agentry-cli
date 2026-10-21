package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
	if err != nil || strings.Count(string(log), "\n") != 3 {
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
	for _, bad := range []string{link, file, "relative/share", dir + ":/etc", dir + "/../" + filepath.Base(dir), filepath.Join(dir, "missing")} {
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
	next.rd.shareKept = readShareCache(cache, share)
	next.checkShare(context.Background())
	if decodeRuns() != runs || next.shares.verdicts["notes.md"].Verdict != toolsrv.ShareClean {
		t.Fatalf("the next segment checked again (%d runs) or lost the verdict: %+v", decodeRuns()-runs, next.shares.verdicts)
	}
	if kept := readShareCache(cache, filepath.Join(share, "other")); len(kept) != 0 {
		t.Fatalf("verdicts kept for another folder were used: %+v", kept)
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
// ClamAV would have found among them; the chat is not told each turn.
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
	next.rd.shareKept = readShareCache(cache, share)
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

	// Verdicts kept before the text was are not used.
	old, err := json.Marshal(map[string]any{"share": share, "files": first})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if kept := readShareCache(cache, share); len(kept) != 0 {
		t.Fatalf("verdicts of an older version were used: %+v", kept)
	}
}
