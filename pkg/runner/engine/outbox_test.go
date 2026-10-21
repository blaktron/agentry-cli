package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// outboxFolders are an outbox and a share, each a folder of its own.
func outboxFolders(t *testing.T) (outbox, share string) {
	t.Helper()
	outbox, share = filepath.Join(t.TempDir(), shareOutName), t.TempDir()
	if err := os.Mkdir(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	return outbox, share
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own folder
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// byOutbox is the copy's results by each entry's name in the outbox.
func byOutbox(copies []outboxCopy) map[string]outboxCopy {
	out := map[string]outboxCopy{}
	for _, c := range copies {
		out[c.Outbox] = c
	}
	return out
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

// TestOutboxRefusesOverwriteAndLinks: a file the server saved reaches the
// share under its own name, or "<name> (2)" when the share holds that name,
// and never replaces a file there; a link, a file with another link to it,
// a folder, a pipe, a hidden file and a name that reads as something else
// are refused; the outbox is emptied either way.
func TestOutboxRefusesOverwriteAndLinks(t *testing.T) {
	outbox, share := outboxFolders(t)
	writeFile(t, filepath.Join(share, "report.docx"), "the person's report\n")
	writeFile(t, filepath.Join(share, "report (2).docx"), "the person's second report\n")
	writeFile(t, filepath.Join(outbox, "report.docx"), "the server's report\n")
	writeFile(t, filepath.Join(outbox, "notes.md"), "the server's notes\n")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "not the share's\n")
	if err := os.Symlink(secret, filepath.Join(outbox, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("report.docx", filepath.Join(outbox, "inside.docx")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outbox, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outbox, "folder", "deep.txt"), "beneath\n")
	writeFile(t, filepath.Join(outbox, ".profile"), "hidden\n")
	turned := "invoice" + string(rune(0x202e)) + "fdp.exe"
	writeFile(t, filepath.Join(outbox, turned), "turned around\n")
	hard := runtime.GOOS != "windows"
	if hard {
		other := filepath.Join(t.TempDir(), "other.txt")
		writeFile(t, other, "another file's bytes\n")
		if err := os.Link(other, filepath.Join(outbox, "hard.txt")); err != nil {
			hard = false // a temp folder on another file system
		}
	}
	pipe := makePipe(t, filepath.Join(outbox, "pipe"))

	copies, err := copyShareOut(outbox, share)
	if err != nil {
		t.Fatal(err)
	}
	got := byOutbox(copies)
	if c := got["report.docx"]; c.Path != "report (3).docx" || c.Refused != "" || c.SHA256 == "" || c.Size != int64(len("the server's report\n")) {
		t.Fatalf("report.docx: %+v", c)
	}
	if c := got["notes.md"]; c.Path != "notes.md" || c.Refused != "" {
		t.Fatalf("notes.md: %+v", c)
	}
	if readFile(t, filepath.Join(share, "report.docx")) != "the person's report\n" || readFile(t, filepath.Join(share, "report (2).docx")) != "the person's second report\n" {
		t.Fatal("a file in the share was written over")
	}
	if readFile(t, filepath.Join(share, "report (3).docx")) != "the server's report\n" {
		t.Fatal("the server's report is not in the share under a new name")
	}
	refused := []string{"link.txt", "inside.docx", "folder", ".profile", turned}
	if hard {
		refused = append(refused, "hard.txt")
	}
	if pipe {
		refused = append(refused, "pipe")
	}
	for _, name := range refused {
		if c := got[name]; c.Refused == "" || c.Path != "" {
			t.Errorf("%q was not refused: %+v", name, c)
		}
	}
	if want := []string{"notes.md", "report (2).docx", "report (3).docx", "report.docx"}; !slices.Equal(entries(t, share), want) {
		t.Errorf("the share holds %v, want %v", entries(t, share), want)
	}
	if left := entries(t, outbox); len(left) != 0 {
		t.Errorf("the outbox still holds %v", left)
	}
	if readFile(t, secret) != "not the share's\n" {
		t.Error("the file a link named was changed")
	}
}

// TestOutboxNamesForTheShare: a clash takes " (n)" before the extension; a
// name the share is not given is refused.
func TestOutboxNamesForTheShare(t *testing.T) {
	for name, want := range map[string]string{"report.docx": "report (2).docx", "notes": "notes (2)", "a.tar.gz": "a.tar (2).gz"} {
		if got := numberedName(name, 2); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	for _, bad := range []string{".env", "a.", "a ", "a:b", `a\b`, "con.txt", "NUL", "a\x01b", "a" + string(rune(0x200b)) + "b", strings.Repeat("a", shareOutNameMax+1)} {
		if outboxNameError(bad) == nil {
			t.Errorf("%q was given to the share", bad)
		}
	}
	for _, good := range []string{"report.docx", "Résumé 2026.pdf", "notes (copy).md", "console.log"} {
		if err := outboxNameError(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// TestOutboxCaps: past the files a turn, a file larger than the file cap,
// and one past the bytes a turn are refused and removed; the share keeps
// what fit.
func TestOutboxCaps(t *testing.T) {
	old := shareOutCaps
	t.Cleanup(func() { shareOutCaps = old })
	shareOutCaps.files, shareOutCaps.file, shareOutCaps.total = 3, 10, 25
	outbox, share := outboxFolders(t)
	writeFile(t, filepath.Join(outbox, "a.txt"), "123456789\n") // 10, fits
	writeFile(t, filepath.Join(outbox, "b.txt"), "12345678901") // 11, past the file cap
	writeFile(t, filepath.Join(outbox, "c.txt"), "123456789\n") // 10, fits: 20
	writeFile(t, filepath.Join(outbox, "d.txt"), "123456789\n") // 10, past the 25 a turn
	writeFile(t, filepath.Join(outbox, "e.txt"), "12345")       // 5: 25, the third file
	writeFile(t, filepath.Join(outbox, "f.txt"), "1")           // past the 3 files
	copies, err := copyShareOut(outbox, share)
	if err != nil {
		t.Fatal(err)
	}
	got := byOutbox(copies)
	for name, want := range map[string]string{"b.txt": "larger than 10 bytes", "d.txt": "past the 25 bytes copied a turn", "f.txt": "past the 3 files copied a turn"} {
		if got[name].Refused != want {
			t.Errorf("%s: %+v, want refused %q", name, got[name], want)
		}
	}
	if want := []string{"a.txt", "c.txt", "e.txt"}; !slices.Equal(entries(t, share), want) {
		t.Errorf("the share holds %v, want %v", entries(t, share), want)
	}
	if left := entries(t, outbox); len(left) != 0 {
		t.Errorf("the outbox still holds %v", left)
	}
}

// TestACopiedFileIsCheckedAtTheNextTurn: what a server saved is in the share
// once the turn is over, a line of the share's receipt, and a note carried
// into the next turn with the name it took; the next turn's check reads it
// as any file the person adds, so a file ClamAV finds is refused.
func TestACopiedFileIsCheckedAtTheNextTurn(t *testing.T) {
	stubShareOutHandOver(t)
	s, share := shareSession(t)
	s.rd.shareOut, _ = outboxFolders(t)
	writeFile(t, filepath.Join(share, "made.md"), "the person's\n")
	writeFile(t, filepath.Join(s.rd.shareOut, "made.md"), "the server's\n")
	writeFile(t, filepath.Join(s.rd.shareOut, "eicar.txt"), "X5O EICAR\n")
	ctx := context.Background()
	s.copyShareOut(ctx)
	if left := entries(t, s.rd.shareOut); len(left) != 0 {
		t.Fatalf("the outbox still holds %v", left)
	}
	if len(s.pendingNotices) != 1 || !strings.Contains(s.pendingNotices[0], spec.AgentRoot+"/"+spec.AgentShareOutDir+"/made.md as share/made (2).md") {
		t.Fatalf("the next turn is told %q", s.pendingNotices)
	}
	s.checkShare(ctx)
	v := readVerdicts(t, s)
	if v["made (2).md"].Verdict != toolsrv.ShareClean || v["eicar.txt"].Verdict != toolsrv.ShareFound {
		t.Fatalf("the copies' verdicts: %+v", v)
	}
	f, err := os.Open(filepath.Join(s.rd.receiptsDir, ShareChecksLog))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	copied := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct{ Outbox, Path string }
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Outbox != "" {
			copied[line.Outbox] = line.Path
		}
	}
	if copied["made.md"] != "made (2).md" || copied["eicar.txt"] != "eicar.txt" {
		t.Fatalf("the receipt's copies: %v", copied)
	}
}

// stubShareOutHandOver stands in for the one-shot that hands the outbox to
// the person, which needs an engine.
func stubShareOutHandOver(t *testing.T) {
	t.Helper()
	old := shareOutHandOver
	t.Cleanup(func() { shareOutHandOver = old })
	shareOutHandOver = func(context.Context, dockerx.Client, Params, *runDirs) error { return nil }
}

// TestShareOutReachesTheServersAlone: a plan whose servers see the share
// gives the segment an outbox of its own beside its cfg; the docker filter
// gives it to no container that joins a network; a plan with none has none.
func TestShareOutReachesTheServersAlone(t *testing.T) {
	rd := runDirsAt(t.TempDir())
	rd.share = t.TempDir()
	plan := &spec.Plan{MCPServers: []spec.MCPServerSpec{{Name: "mcp-pypi-x", Source: "pypi", Share: spec.ServerShareMirror}}}
	if err := prepareShareOut(Params{Plan: plan}, rd); err != nil {
		t.Fatal(err)
	}
	if rd.shareOut != filepath.Join(rd.root, shareOutName) || strings.HasPrefix(rd.shareOut, rd.cfg) {
		t.Fatalf("the outbox is %s", rd.shareOut)
	}
	if fi, err := os.Lstat(rd.shareOut); err != nil || !fi.IsDir() {
		t.Fatalf("the outbox: %v", err)
	}
	if got := rd.replacer().Replace(spec.PHShareOut); got != rd.shareOut {
		t.Errorf("the outbox's placeholder is %s", got)
	}
	cfg, err := filterConfig(rd, "run1", map[string]string{}, policy.Limits{}, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.OfflineRoots, rd.gatewayPath(rd.shareOut)) {
		t.Errorf("the filter's offline roots %v leave out the outbox", cfg.OfflineRoots)
	}
	none := runDirsAt(t.TempDir())
	none.share = t.TempDir()
	if err := prepareShareOut(Params{Plan: &spec.Plan{MCPServers: []spec.MCPServerSpec{{Name: "mcp-pypi-x", Source: "pypi"}}}}, none); err != nil || none.shareOut != "" {
		t.Errorf("a plan whose servers do not see the share has an outbox: %q, %v", none.shareOut, err)
	}
}
