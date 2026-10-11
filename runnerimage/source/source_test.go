package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/decode"
)

// fixture builds a bare repository served by git's own http-backend over
// TLS, with two commits on main and an annotated tag on the first. The
// head commit holds what a hostile or awkward repository holds: a link out
// of the tree, a submodule, an LFS pointer, an executable, a .git folder
// and names no file system shares.
type fixture struct {
	srv   *httptest.Server
	url   string
	first string
	head  string
	bare  string
}

const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"

func gitBin(t *testing.T) string {
	t.Helper()
	g, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	out, err := exec.Command(g, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	return g
}

func newFixture(t *testing.T, v2 bool) *fixture {
	t.Helper()
	g := gitBin(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(g, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run(work, "init", "-q", "-b", "main")
	write("README.md", "first\n", 0o644)
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "first")
	first := run(work, "rev-parse", "HEAD")
	run(work, "tag", "-a", "v1", "-m", "v1")

	// Two large, nearly equal files, so the pack carries a delta.
	big := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 4000)
	write("README.md", "second\n", 0o644)
	write("src/a.txt", big, 0o644)
	write("src/b.txt", big+"one more line\n", 0o644)
	write("src/deep/nested/c.go", "package c\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write("assets/model.bin", lfsPointer, 0o644)
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	// A submodule: a gitlink entry, without its repository.
	run(work, "update-index", "--add", "--cacheinfo", "160000,"+first+",vendor/sub")
	// Names git stores but no file system shares, a case collision, and
	// .git folders below the root, built with mktree as a hostile
	// repository would hold them (git's own index refuses a .git path).
	empty := run(work, "hash-object", "-w", "src/deep/nested/c.go")
	for _, name := range []string{"docs/CON.txt", "docs/a:b.txt", "docs/Readme.md", "docs/README.md"} {
		run(work, "update-index", "--add", "--cacheinfo", "100644,"+empty+","+name)
	}
	run(work, "commit", "-q", "-m", "second")
	mktree := func(lines string) string {
		t.Helper()
		cmd := exec.Command(g, "mktree")
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(lines)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mktree: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	inner := mktree("100644 blob " + empty + "\tconfig\n")
	nested := mktree("040000 tree " + inner + "\t.git\n040000 tree " + inner + "\t.GIT.\n")
	rootTree := mktree(run(work, "ls-tree", "HEAD") + "\n040000 tree " + nested + "\tnested\n")
	head := run(work, "commit-tree", rootTree, "-p", first, "-m", "second")
	run(work, "update-ref", "refs/heads/main", head)

	bare := filepath.Join(root, "srv", "repo.git")
	run(root, "clone", "-q", "--bare", work, bare)
	run(bare, "config", "http.uploadpack", "true")
	if !v2 {
		run(bare, "config", "protocol.version", "0")
	}
	execPath := run(root, "--exec-path")
	h := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "srv"), "GIT_HTTP_EXPORT_ALL=1", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1"},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v2 {
			r.Header.Del("Git-Protocol")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, url: srv.URL + "/repo.git", first: first, head: head, bare: bare}
}

func (f *fixture) options(t *testing.T) Options {
	return Options{Kind: KindRepository, URL: f.url, Out: t.TempDir(), Work: t.TempDir(), Transport: f.srv.Client().Transport, anyPort: true}
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.Type()&os.ModeSymlink != 0 {
			t.Errorf("%s is a link", rel)
		}
		if !d.IsDir() {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestFetchStagesPlainFilesOnly(t *testing.T) {
	for _, v2 := range []bool{true, false} {
		name := "v0"
		if v2 {
			name = "v2"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, v2)
			o := f.options(t)
			rep, err := Fetch(context.Background(), o)
			if err != nil {
				t.Fatalf("Fetch: %v (report %+v)", err, rep)
			}
			if rep.Protocol != name {
				t.Errorf("protocol %q, want %q", rep.Protocol, name)
			}
			if rep.Commit != f.head || rep.Ref != "refs/heads/main" {
				t.Errorf("commit %s ref %s, want %s refs/heads/main", rep.Commit, rep.Ref, f.head)
			}
			got := listFiles(t, o.Out)
			want := []string{"README.md", "assets/model.bin", "bin/run.sh", "docs/README.md", "src/a.txt", "src/b.txt", "src/deep/nested/c.go"}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("files %v, want %v", got, want)
			}
			if rep.Files != len(want) {
				t.Errorf("report files %d, want %d", rep.Files, len(want))
			}
			for why, n := range map[string]int{SkipLink: 1, SkipSubmodule: 1, SkipGit: 2, SkipName: 2, SkipCase: 1} {
				if rep.Skipped[why] != n {
					t.Errorf("skipped %s: %d, want %d (%v)", why, rep.Skipped[why], n, rep.SkippedPaths)
				}
			}
			if rep.LFSPointers != 1 {
				t.Errorf("LFS pointers %d, want 1", rep.LFSPointers)
			}
			if b, _ := os.ReadFile(filepath.Join(o.Out, "assets", "model.bin")); string(b) != lfsPointer {
				t.Errorf("the LFS pointer was not kept as it is: %q", b)
			}
			fi, err := os.Stat(filepath.Join(o.Out, "bin", "run.sh"))
			if err != nil || fi.Mode().Perm() != 0o644 {
				t.Errorf("an executable is staged as %v, want 0644", fi.Mode())
			}
			b, _ := os.ReadFile(filepath.Join(o.Out, "src", "b.txt"))
			if !strings.HasSuffix(string(b), "one more line\n") || len(b) < 100000 {
				t.Errorf("src/b.txt was not rebuilt whole (%d bytes)", len(b))
			}
			var onDisk Report
			raw, err := os.ReadFile(filepath.Join(o.Work, ReportFile))
			if err != nil || json.Unmarshal(raw, &onDisk) != nil || onDisk.Commit != f.head {
				t.Errorf("the report on disk is not the fetch's: %s", raw)
			}
			if _, err := os.Stat(filepath.Join(o.Work, "pack")); !os.IsNotExist(err) {
				t.Errorf("the pack is left in the work directory")
			}
		})
	}
}

func TestFetchAtATagOrCommit(t *testing.T) {
	f := newFixture(t, true)
	for _, ref := range []string{"v1", "refs/tags/v1", f.first} {
		o := f.options(t)
		o.Ref = ref
		rep, err := Fetch(context.Background(), o)
		if err != nil {
			t.Fatalf("ref %s: %v", ref, err)
		}
		if rep.Commit != f.first {
			t.Errorf("ref %s: commit %s, want %s", ref, rep.Commit, f.first)
		}
		if got := listFiles(t, o.Out); strings.Join(got, ",") != "README.md" {
			t.Errorf("ref %s: files %v", ref, got)
		}
	}
	o := f.options(t)
	o.Ref = "no-such-branch"
	if _, err := Fetch(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no branch or tag") {
		t.Errorf("an unknown ref: %v", err)
	}
}

func TestFetchOverACapStagesNothing(t *testing.T) {
	f := newFixture(t, true)
	for _, tc := range []struct {
		name  string
		files int
		bytes int64
		want  string
	}{
		{"files", 3, 0, "holds 7 files"},
		{"bytes", 0, 1000, "past the 1000 bytes cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := f.options(t)
			o.MaxFiles, o.MaxBytes = tc.files, tc.bytes
			rep, err := Fetch(context.Background(), o)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !strings.Contains(rep.Refusal, tc.want) {
				t.Errorf("refusal %q, want it to say %q", rep.Refusal, tc.want)
			}
			if got := listFiles(t, o.Out); len(got) != 0 {
				t.Errorf("a refused source staged %v", got)
			}
		})
	}
}

func TestFetchSaysWhyAHostRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	o := Options{Kind: KindRepository, URL: srv.URL + "/o/r", Out: t.TempDir(), Work: t.TempDir(), Transport: srv.Client().Transport, anyPort: true}
	rep, err := Fetch(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "no token was given") || !strings.Contains(rep.Error, "404") {
		t.Errorf("anonymous: %v / %q", err, rep.Error)
	}
	o.Work = t.TempDir()
	o.Token = "ghp_plantedtoken0123456789"
	rep, err = Fetch(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "refused the access token") {
		t.Errorf("with a token: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(o.Work, ReportFile))
	if strings.Contains(string(raw), o.Token) || strings.Contains(rep.Error, o.Token) {
		t.Errorf("the token reached the report")
	}
}

func TestFetchFollowsNoRedirectOffTheHost(t *testing.T) {
	var sawAuth bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = sawAuth || r.Header.Get("Authorization") != ""
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	o := Options{Kind: KindRepository, URL: srv.URL + "/o/r", Token: "t0ken", Out: t.TempDir(), Work: t.TempDir(), Transport: srv.Client().Transport, anyPort: true}
	if _, err := Fetch(context.Background(), o); err == nil || !strings.Contains(err.Error(), "redirected to another address") {
		t.Errorf("a redirect off the host: %v", err)
	}
	if sawAuth {
		t.Error("the token followed the redirect")
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{
		"http://github.com/o/r", "ssh://git@github.com/o/r", "git://github.com/o/r", "git@github.com:o/r",
		"https://user:pass@github.com/o/r", "https://github.com:8443/o/r", "https://github.com/o/r?x=1",
		"https://github.com/o/r#main", "https://github.com", "file:///etc",
		"https://github.com,evil.example/o/r", "https://git_hub.com/o/r", "https://github.com./o/r",
	} {
		if _, err := CheckURL(bad); err == nil {
			t.Errorf("CheckURL(%q) passed", bad)
		}
	}
	for _, good := range []string{"https://github.com/o/r", "https://github.com:443/o/r.git", "https://gitlab.example.com/group/sub/r"} {
		if _, err := CheckURL(good); err != nil {
			t.Errorf("CheckURL(%q): %v", good, err)
		}
	}
}

func TestPortableName(t *testing.T) {
	for _, bad := range []string{"CON", "con.txt", "LPT1", "com9.log", "a:b", "a?b", "trail.", "trail ", "tab\tname", "NUL.tar.gz"} {
		if portableName(bad) {
			t.Errorf("portableName(%q) = true", bad)
		}
	}
	for _, good := range []string{"README.md", "console.log", "COM10", "a b", ".gitignore", "ünïcode.txt"} {
		if !portableName(good) {
			t.Errorf("portableName(%q) = false", good)
		}
	}
	for _, bidi := range []string{"a\u202ebad", "x\u0085y", "zero\ufeffwidth"} {
		if decode.PlainMemberPath("dir/" + bidi) {
			t.Errorf("DR-39's rule passed %q", bidi)
		}
	}
	tok := strings.Repeat("t", 16)
	forms := TokenForms(tok)
	if len(forms) != 2 || forms[1] != base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)) {
		t.Errorf("TokenForms = %q", forms)
	}
	if f := TokenForms("me:supersecretpass"); len(f) != 3 || f[1] != "supersecretpass" {
		t.Errorf("TokenForms(user:pass) = %q", f)
	}
	for _, g := range []string{".git", ".GIT", ".git.", ".git ", "GIT~1"} {
		if !isGitName(g) {
			t.Errorf("isGitName(%q) = false", g)
		}
	}
}

// TestATreeBombIsRefusedBeforeItIsWalkedWhole: a tree that names one
// subtree many times (a million files from a few hundred objects) is
// stopped at the walk's bound and refused, never enumerated whole.
func TestATreeBombIsRefusedBeforeItIsWalkedWhole(t *testing.T) {
	f := newFixture(t, true)
	g := gitBin(t)
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(g, args...)
		cmd.Dir = f.bare
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("x\n", "hash-object", "-w", "--stdin")
	level := func(mode, typ, oid, prefix string) string {
		var b strings.Builder
		for i := 0; i < 100; i++ {
			b.WriteString(mode + " " + typ + " " + oid + "\t" + prefix + strconv.Itoa(i) + "\n")
		}
		return git(b.String(), "mktree")
	}
	t1 := level("100644", "blob", blob, "f")
	t2 := level("040000", "tree", t1, "d")
	t3 := level("040000", "tree", t2, "e")
	commit := git("", "commit-tree", t3, "-m", "bomb")
	git("", "update-ref", "refs/heads/bomb", commit)
	o := f.options(t)
	o.Ref = "bomb"
	rep, err := Fetch(context.Background(), o)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(rep.Refusal, "more than 50000 files, folders and links") {
		t.Errorf("refusal %q", rep.Refusal)
	}
	if got := listFiles(t, o.Out); len(got) != 0 {
		t.Errorf("a refused tree staged %d files", len(got))
	}
}
