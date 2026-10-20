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
	for _, d := range []string{rd.shareChecks, rd.receiptsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pol := &policy.Policy{}
	return &liveSession{
		p:      Params{Plan: &spec.Plan{}, Policy: pol, RunnerHome: t.TempDir()},
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
