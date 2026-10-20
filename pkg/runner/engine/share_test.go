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
	for _, bad := range []string{link, file, "relative/share", dir + "/../" + filepath.Base(dir), filepath.Join(dir, "missing")} {
		if err := ShareFolderUsable(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
