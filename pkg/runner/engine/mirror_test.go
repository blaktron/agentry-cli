package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// mirrored lists the mirror's files, beneath it, and what each holds.
func mirrored(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMirrorLeavesOutAFinding: the share's mirror holds a copy of each file
// the check passed, a file ClamAV found never; a file changed is the new
// bytes once checked, a file removed from the share is removed, and
// anything else put in the mirror goes at the next check.
func TestMirrorLeavesOutAFinding(t *testing.T) {
	s, share := shareSession(t)
	m, err := openShareMirror(filepath.Join(t.TempDir(), "share-mirror"))
	if err != nil {
		t.Fatal(err)
	}
	s.rd.shareMirror = m
	writeShare(t, share, "notes.md", "the person's notes\n")
	writeShare(t, share, "docs/report.txt", "a report\n")
	writeShare(t, share, "in/eicar.txt", "X5O EICAR\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(share, "passwd")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.checkShare(ctx)
	got := mirrored(t, m.dir)
	if !slices.Equal(keys(got), []string{"docs/report.txt", "notes.md"}) || got["notes.md"] != "the person's notes\n" {
		t.Fatalf("the mirror holds %v", got)
	}
	if v := readVerdicts(t, s)["in/eicar.txt"]; v.Verdict != toolsrv.ShareFound {
		t.Fatalf("the finding's verdict: %+v", v)
	}
	// A file that becomes a finding leaves the mirror; a removed file
	// goes; a stray file in the mirror goes.
	time.Sleep(10 * time.Millisecond)
	writeShare(t, share, "notes.md", "now X5O EICAR\n")
	if err := os.Remove(filepath.Join(share, "docs", "report.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeShare(t, share, "new.md", "new\n")
	s.checkShare(ctx)
	if got := mirrored(t, m.dir); !slices.Equal(keys(got), []string{"new.md"}) {
		t.Fatalf("after a finding, a removal and a stray file the mirror holds %v", got)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "docs")); err == nil {
		t.Error("an emptied folder was left in the mirror")
	}
}

// TestMirrorCopiesOnlyTheCheckedBytes: a segment that starts with kept
// verdicts copies each passed file from the share only when its bytes are
// still the ones checked; a file changed since is left out for the next
// check, and a copy changed in the mirror is made again.
func TestMirrorCopiesOnlyTheCheckedBytes(t *testing.T) {
	_, share := shareSession(t)
	writeShare(t, share, "a.md", "checked\n")
	writeShare(t, share, "b.md", "changed after its check\n")
	verdicts := map[string]toolsrv.ShareVerdict{
		"a.md": {Verdict: toolsrv.ShareClean, SHA256: sha256Hex([]byte("checked\n"))},
		"b.md": {Verdict: toolsrv.ShareUnchecked, SHA256: sha256Hex([]byte("as it was checked\n"))},
		"../x": {Verdict: toolsrv.ShareClean, SHA256: sha256Hex([]byte("checked\n"))},
	}
	dir := filepath.Join(t.TempDir(), "share-mirror")
	m, err := openShareMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.sync(share, verdicts); err != nil {
		t.Fatal(err)
	}
	if got := mirrored(t, dir); !slices.Equal(keys(got), []string{"a.md"}) {
		t.Fatalf("the mirror holds %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("tampered with\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The next segment reads the index the last one left.
	m, err = openShareMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.sync(share, verdicts); err != nil {
		t.Fatal(err)
	}
	if got := mirrored(t, dir); got["a.md"] != "checked\n" || len(got) != 1 {
		t.Fatalf("a copy changed in the mirror was not made again: %v", got)
	}
	t.Run("what the mirror refuses", mirrorRefusesWhatIsNotTheShareOrItsBytes)
}

// mirrorRefusesWhatIsNotTheShareOrItsBytes: the mirror opens only on a clean
// absolute path without a colon, reads back only an index of its own version,
// copies only what passed with a plain digest, and refuses a path outside the
// share and bytes other than the ones checked.
func mirrorRefusesWhatIsNotTheShareOrItsBytes(t *testing.T) {
	base := t.TempDir()
	for name, dir := range map[string]string{
		"a relative path":   "share-mirror",
		"an unclean path":   base + "/a/../share-mirror",
		"a path with colon": filepath.Join(base, "share:mirror"),
	} {
		if m, err := openShareMirror(dir); err == nil || m != nil {
			t.Errorf("%s: the mirror opened", name)
		}
	}
	if _, err := openShareMirror(filepath.Join(base, "a:b")); err == nil || !strings.Contains(err.Error(), "colon") {
		t.Errorf("a colon in the path: %v", err)
	}

	// An index of another version is not read back.
	dir := filepath.Join(base, "mirror-versions")
	_, index := ShareMirrorBeside(dir)
	for version, want := range map[int]int{mirrorIndexVersion: 1, mirrorIndexVersion + 1: 0} {
		b, _ := json.Marshal(mirrorIndex{Version: version, Files: map[string]mirrorFile{"a.md": {SHA256: "x", Size: 1}}})
		if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(index, b, 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := openShareMirror(dir)
		if err != nil || len(m.files) != want {
			t.Errorf("an index of version %d read back as %v (%v), want %d file(s)", version, m.files, err, want)
		}
	}

	// A verdict passes when it is clean or unchecked and its digest is plain.
	_, share := shareSession(t)
	for _, name := range []string{"clean.md", "unchecked.md", "found.md", "nodigest.md", "badcase.md"} {
		writeShare(t, share, name, name+" bytes\n")
	}
	sum := func(name string) string { return sha256Hex([]byte(name + " bytes\n")) }
	m, err := openShareMirror(filepath.Join(base, "mirror-verdicts"))
	if err != nil {
		t.Fatal(err)
	}
	err = m.sync(share, map[string]toolsrv.ShareVerdict{
		"clean.md":     {Verdict: toolsrv.ShareClean, SHA256: sum("clean.md")},
		"unchecked.md": {Verdict: toolsrv.ShareUnchecked, SHA256: sum("unchecked.md")},
		"found.md":     {Verdict: toolsrv.ShareFound, SHA256: sum("found.md")},
		"nodigest.md":  {Verdict: toolsrv.ShareClean, SHA256: "not a digest"},
		"badcase.md":   {Verdict: toolsrv.ShareClean, SHA256: strings.ToUpper(sum("badcase.md"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mirrored(t, m.dir); !slices.Equal(keys(got), []string{"clean.md", "unchecked.md"}) {
		t.Errorf("the mirror holds %v", keys(got))
	}

	// A copy whose size changed, or whose time changed, is made again.
	for name, tamper := range map[string]func(path string, fi os.FileInfo) error{
		"a copy of another size": func(path string, fi os.FileInfo) error {
			if err := os.WriteFile(path, []byte("longer than it was"), 0o644); err != nil {
				return err
			}
			return os.Chtimes(path, fi.ModTime(), fi.ModTime())
		},
		"a copy of another time": func(path string, fi os.FileInfo) error {
			when := fi.ModTime().Add(-time.Hour)
			return os.Chtimes(path, when, when)
		},
	} {
		path := filepath.Join(m.dir, "clean.md")
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := tamper(path, fi); err != nil {
			t.Fatal(err)
		}
		if err := m.sync(share, map[string]toolsrv.ShareVerdict{"clean.md": {Verdict: toolsrv.ShareClean, SHA256: sum("clean.md")}}); err != nil {
			t.Fatal(err)
		}
		if got := mirrored(t, m.dir); got["clean.md"] != "clean.md bytes\n" || len(got) != 1 {
			t.Errorf("%s was not made again: %v", name, got)
		}
	}

	// A run with no mirror has nothing to sync.
	var none *shareMirror
	if err := none.sync(share, nil); err != nil || none.folder() != "" {
		t.Errorf("a nil mirror: %v %q", err, none.folder())
	}

	// A path outside the share, and bytes other than the ones checked.
	src := filepath.Join(share, "clean.md")
	if err := m.put("../escape.md", src, sum("clean.md")); err == nil || !strings.Contains(err.Error(), "is not a path beneath the share") {
		t.Errorf("a path outside the share: %v", err)
	}
	if pathExists(filepath.Join(filepath.Dir(m.dir), "escape.md")) {
		t.Error("a file was written outside the mirror")
	}
	if err := m.put("other.md", src, sum("unchecked.md")); !errors.Is(err, errChangedSinceCheck) {
		t.Errorf("bytes other than the checked ones: %v", err)
	}
	if pathExists(filepath.Join(m.dir, "other.md")) {
		t.Error("bytes other than the checked ones were mirrored")
	}
}

// TestFilterHoldsTheShareToTheToolsServer: the docker filter lets the run's
// tools server alone write the share, and alone bind it unless a server
// sees the share folder itself (the Coding agent's).
func TestFilterHoldsTheShareToTheToolsServer(t *testing.T) {
	rd := runDirsAt(t.TempDir())
	rd.share = "/home/p/agentry/codershare"
	env := map[string]string{
		"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS":        rd.workspace + ":" + rd.share,
		"MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS": rd.outDir + ":" + rd.share,
	}
	for _, tc := range []struct {
		name  string
		share spec.ServerShare
		only  bool
	}{
		{"no server sees it", "", true},
		{"a server sees the mirror", spec.ServerShareMirror, true},
		{"a server sees the folder", spec.ServerShareLive, false},
	} {
		plan := &spec.Plan{MCPServers: []spec.MCPServerSpec{{Name: "mcp-pypi-x", Source: "pypi", Share: tc.share}}}
		cfg, err := filterConfig(rd, "run1", env, policy.Limits{}, plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.ToolsWritableRoots, []string{rd.share}) || slices.Equal(cfg.ToolsOnlyRoots, []string{rd.share}) != tc.only ||
			!slices.Contains(cfg.OfflineRoots, rd.share) {
			t.Errorf("%s: tools-only writable %v, tools-only %v, offline %v", tc.name, cfg.ToolsWritableRoots, cfg.ToolsOnlyRoots, cfg.OfflineRoots)
		}
	}
}

// TestFilterHoldsTheSkillsToTheToolsServer: the agent's skills folder is a
// root the tools server alone may bind, whatever a server sees of the share,
// and never a writable one.
func TestFilterHoldsTheSkillsToTheToolsServer(t *testing.T) {
	rd := runDirsAt(t.TempDir())
	rd.share = "/home/p/agentry/codershare"
	rd.skills = "/home/p/.agentry/runner/persistent/coder/skills"
	env := map[string]string{
		"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS":        rd.workspace + ":" + rd.share + ":" + rd.skills,
		"MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS": rd.outDir + ":" + rd.share,
	}
	for _, share := range []spec.ServerShare{"", spec.ServerShareLive} {
		plan := &spec.Plan{MCPServers: []spec.MCPServerSpec{{Name: "mcp-pypi-x", Source: "pypi", Share: share}}}
		cfg, err := filterConfig(rd, "run1", env, policy.Limits{}, plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(cfg.ToolsOnlyRoots, rd.skills) || slices.Contains(cfg.ToolsWritableRoots, rd.skills) || slices.Contains(cfg.WritableRoots, rd.skills) {
			t.Errorf("share %q: tools-only %v, tools-writable %v, writable %v", share, cfg.ToolsOnlyRoots, cfg.ToolsWritableRoots, cfg.WritableRoots)
		}
	}
}
