package build

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// plantedToken is a token a test gives a source and then looks for in
// everything the build leaves.
const plantedToken = "ghp_PLANTEDbuildTOKEN0123456789abcdef"

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// playFetch scripts the fetch step for one source: it writes files under
// /out and the report under /work as the runner's source role would.
func playFetch(t *testing.T, f *dockerx.Fake, id string, files map[string]string, edit func(rep *source.Report)) *dockerx.FakeForeground {
	t.Helper()
	fg := &dockerx.FakeForeground{}
	fg.Before = func(r dockerx.RunSpec) {
		out, work := mountOf(r, "/out"), mountOf(r, "/work")
		rep := source.Report{Schema: source.Schema, Kind: source.KindRepository, Ref: "refs/heads/main", Commit: testCommit, Protocol: "v2"}
		for name, body := range files {
			writeFile(t, filepath.Join(out, filepath.FromSlash(name)), body)
			rep.Files++
			rep.Bytes += int64(len(body))
		}
		if edit != nil {
			edit(&rep)
		}
		b, _ := json.Marshal(rep)
		writeFile(t, filepath.Join(work, source.ReportFile), string(b))
	}
	f.Foreground["-source-"+id] = fg
	return fg
}

// TestParseSources: a repository's address with @ref or #ref names its
// documents' folder after its host and path; an address is one file; the
// counts and the kinds' address rules hold.
func TestParseSources(t *testing.T) {
	srcs, err := ParseSources([]string{"https://github.com/o/r.git@v1.2", "https://gitlab.example.com/group/sub/proj#main"}, []string{"https://example.org/spec.pdf#page=2", "https://files.example.com/get?sig=abc"})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, url, ref, host string }{
		{"repo-github.com-o-r", "https://github.com/o/r.git", "v1.2", "github.com"},
		{"repo-gitlab.example.com-group-sub-proj", "https://gitlab.example.com/group/sub/proj", "main", "gitlab.example.com"},
		{"url-1", "https://example.org/spec.pdf", "", "example.org"},
		{"url-2", "https://files.example.com/get?sig=abc", "", "files.example.com"},
	}
	if len(srcs) != len(want) {
		t.Fatalf("%d sources", len(srcs))
	}
	for i, w := range want {
		s := srcs[i]
		if s.ID != w.id || s.URL != w.url || s.Ref != w.ref || s.Host != w.host {
			t.Errorf("%d: %+v", i, *s)
		}
	}
	if srcs[0].Kind != manifest.IntakeRepository || srcs[2].Kind != manifest.IntakeURL {
		t.Errorf("kinds %s %s", srcs[0].Kind, srcs[2].Kind)
	}
	for _, bad := range [][]string{
		{"https://github.com/o/r", "https://github.com/o/r@main"}, // one folder
		{"http://github.com/o/r"},
		{"https://user:pw@github.com/o/r"},
		{"git@github.com:o/r"},
		{"https://github.com/o/r@bad..ref"},
	} {
		if _, err := ParseSources(bad, nil); err == nil {
			t.Errorf("--repo %v was taken", bad)
		}
	}
	if _, err := ParseSources(nil, []string{"ftp://x/y"}); err == nil {
		t.Error("an ftp address was taken")
	}
	many := make([]string, MaxURLs+1)
	for i := range many {
		many[i] = "https://example.org/" + strings.Repeat("a", i+1)
	}
	if _, err := ParseSources(nil, many); err == nil || !strings.Contains(err.Error(), "at most 20") {
		t.Errorf("err = %v", err)
	}
}

// TestABuildFromARepositoryAndAnAddress: the repository is fetched by the
// step a run's source takes (the files image, its own network behind a
// proxy to its host alone, the token on stdin and nowhere else), staged as
// documents under repo-<host>-<path>/ with names a citation carries, held
// to the source rule in the decode, and kept in the pack under sources/;
// the file at the address is staged under its own name and decoded as an
// upload; the task and the receipt name both; and the token is in nothing
// the build leaves.
func TestABuildFromARepositoryAndAnAddress(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.opts.Documents = nil
	srcs, err := ParseSources([]string{"https://github.com/o/r@main"}, []string{"https://example.org/spec.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	srcs[0].Token, srcs[0].TokenFrom = plantedToken, sources.TokenFromFlag
	fx.opts.Sources = srcs
	repo := playFetch(t, fx.fake, "repo-github.com-o-r", map[string]string{
		"README.md": "# r\n", "docs/guide.md": "# Guide\n", "src/main.go": "package main\n", "notes/Q3 plan.md": "# Plan\n",
	}, func(rep *source.Report) { rep.Skipped = map[string]int{source.SkipLink: 1} })
	playFetch(t, fx.fake, "url-1", map[string]string{"spec.pdf": "%PDF spec"}, func(rep *source.Report) {
		*rep = source.Report{Schema: source.Schema, Kind: source.KindURL, Files: 1, Bytes: int64(len("%PDF spec")), Name: "spec.pdf"}
	})
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/topics/r.md":    page("R", "repo-github.com-o-r/README.md"),
		"wiki/topics/plan.md": page("Plan", "repo-github.com-o-r/notes/Q3-plan.md"),
		"wiki/topics/spec.md": page("Spec", "spec.pdf"),
	})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The fetch step's posture, and the token on stdin alone.
	fetch, ok := fx.fake.FindRun("-source-repo-github.com-o-r")
	if !ok {
		t.Fatal("no fetch step ran for the repository")
	}
	if got := strings.Join(fetch.Argv, " "); !strings.Contains(got, "--kind repository --url https://github.com/o/r") || !strings.Contains(got, "--ref main") || !strings.Contains(got, "--token-stdin") || strings.Contains(got, plantedToken) {
		t.Errorf("the fetch step's argv: %s", got)
	}
	if repo.GotStdin != plantedToken+"\n" {
		t.Errorf("the token did not reach the fetch step on stdin: %q", repo.GotStdin)
	}
	if !fetch.ReadOnly || fetch.Network != "agentry-cafe0002-src-repo-github.com-o-r" {
		t.Errorf("the fetch step's posture: read-only %v, network %q", fetch.ReadOnly, fetch.Network)
	}
	if !strings.Contains(strings.Join(fetch.Argv, " "), "--max-files 1700 ") {
		// The pack's 2,000-file cap less the documents staged so far (none
		// here) and the room kept for the pages, under the kind's 20,000.
		t.Errorf("the repository's file cap is not the pack's: %s", strings.Join(fetch.Argv, " "))
	}
	proxy, ok := fx.fake.FindRun("-source-proxy-repo-github.com-o-r")
	if !ok || !strings.Contains(strings.Join(proxy.Argv, " "), "--allow github.com:443 --public-only") {
		t.Errorf("the source proxy: %v %v", ok, proxy.Argv)
	}
	urlFetch, ok := fx.fake.FindRun("-source-url-1")
	if !ok || !strings.Contains(strings.Join(urlFetch.Argv, " "), "--kind url --url https://example.org/spec.pdf") || !strings.Contains(strings.Join(urlFetch.Argv, " "), "--max-files 1 ") {
		t.Errorf("the address's fetch step: %v %v", ok, urlFetch.Argv)
	}
	// The token reached no other container, in argv, environment or stdin.
	for _, c := range fx.fake.Calls {
		r := c.Run
		if r.Name == "" || r.Name == fetch.Name || r.Name == proxy.Name {
			continue
		}
		if strings.Contains(strings.Join(r.Argv, " "), plantedToken) {
			t.Errorf("%s carries the token in its argv", r.Name)
		}
		for k, v := range r.Env {
			if strings.Contains(v, plantedToken) {
				t.Errorf("%s carries the token in its environment (%s)", r.Name, k)
			}
		}
		if r.Network == fetch.Network {
			t.Errorf("%s is on the repository's network", r.Name)
		}
	}
	// The decode held the repository to the source rule, and the file to
	// an upload's.
	dec, ok := fx.fake.FindRun("agentry-decode-")
	if !ok || !strings.Contains(strings.Join(dec.Argv, " "), "--source repo-github.com-o-r") || strings.Contains(strings.Join(dec.Argv, " "), "--source url") || strings.Contains(strings.Join(dec.Argv, " "), "--source spec") {
		t.Errorf("the decode's argv: %v", dec.Argv)
	}
	// The documents: the repository's files under its folder, renamed to
	// names a citation carries, and the file under its own name.
	if got := strings.Join(res.Documents, ","); got != "repo-github.com-o-r/README.md,repo-github.com-o-r/docs/guide.md,repo-github.com-o-r/notes/Q3-plan.md,spec.pdf" {
		t.Errorf("documents %s", got)
	}
	if got := strings.Join(res.Skipped, ","); got != "repo-github.com-o-r/src/main.go" {
		t.Errorf("skipped %s", got)
	}
	// What the build reports of each source, with no token.
	if len(res.Sources) != 2 {
		t.Fatalf("sources %+v", res.Sources)
	}
	r, u := res.Sources[0], res.Sources[1]
	if r.Kind != "repository" || r.URL != "https://github.com/o/r" || r.Ref != "main" || r.Commit != testCommit || r.Document != "repo-github.com-o-r/" || r.Files != 4 || r.LeftOut[source.SkipLink] != 1 || r.Token != sources.TokenFromFlag {
		t.Errorf("repository %+v", r)
	}
	if u.Kind != "url" || u.URL != "https://example.org/spec.pdf" || u.Document != "spec.pdf" || u.Files != 1 || u.Bytes != 9 || u.Token != "" {
		t.Errorf("file %+v", u)
	}
	if b, _ := json.Marshal(res.Sources); strings.Contains(string(b), plantedToken) {
		t.Error("the sources' report carries the token")
	}
	// The pack keeps them under sources/.
	for rel, want := range map[string]string{
		"sources/repo-github.com-o-r/README.md":        "# r\n",
		"sources/repo-github.com-o-r/notes/Q3-plan.md": "# Plan\n",
		"sources/spec.pdf":                             "%PDF spec",
		"sources/spec.pdf.txt":                         "text of spec.pdf\n",
	} {
		got, err := os.ReadFile(filepath.Join(fx.opts.PackDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", rel, got, err, want)
		}
	}
	p, err := knowledge.Load(fx.opts.PackDir)
	if err != nil {
		t.Fatalf("the pack does not load: %v", err)
	}
	if findings, err := knowledge.Lint(fx.opts.PackDir, p.Manifest.Name); err != nil || len(findings) != 0 {
		t.Fatalf("the pack does not lint clean: %+v (%v)", findings, err)
	}
	// The task and the receipt name the sources, with no token.
	env := res.Envelope
	for key, want := range map[string]string{
		"source.repo-github.com-o-r": "repository https://github.com/o/r at \"main\" (refs/heads/main), commit " + testCommit + ": 4 files",
		"source.spec.pdf":            "file https://example.org/spec.pdf: spec.pdf, 9 bytes, fetched in",
	} {
		if line := env.CapabilitiesEnforced[key]; !strings.Contains(line, want) {
			t.Errorf("receipt line %s = %q, want %q in it", key, line, want)
		}
	}
	if !strings.Contains(env.CapabilitiesEnforced["source.repo-github.com-o-r"], "with an access token from the environment variable") {
		t.Errorf("the receipt does not say a token was used: %q", env.CapabilitiesEnforced["source.repo-github.com-o-r"])
	}
	task, _ := os.ReadFile(filepath.Join(fx.opts.OutDir, "receipts", "task.txt"))
	for _, want := range []string{"repo-github.com-o-r/ holds the git repository https://github.com/o/r at \"main\" (commit " + testCommit + "), 4 files", "spec.pdf is the file from https://example.org/spec.pdf"} {
		if !strings.Contains(string(task), want) {
			t.Errorf("the task does not say %q:\n%s", want, task)
		}
	}
	for _, dir := range []string{fx.opts.OutDir, fx.opts.PackDir} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if b, _ := os.ReadFile(path); strings.Contains(string(b), plantedToken) {
					t.Errorf("%s holds the token", path)
				}
			}
			return nil
		})
	}
	if entries, _ := os.ReadDir(filepath.Join(fx.opts.Home, "work")); len(entries) != 0 {
		t.Errorf("the build left its work behind: %v", entries)
	}
}

// TestASourceIsHeldToThePacksCap: a repository is fetched within what the
// pack's cap leaves after the documents; a fetch step that refuses one
// over it fails the build before the Creator runs, with the count; and a
// file whose name a given document has is refused.
func TestASourceIsHeldToThePacksCap(t *testing.T) {
	t.Run("the caps are the pack's within the kind's", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx.opts.MaxBytes = 2 << 20
		srcs, _ := ParseSources([]string{"https://github.com/o/r"}, nil)
		fx.opts.Sources = srcs
		playFetch(t, fx.fake, "repo-github.com-o-r", map[string]string{"README.md": "# r\n"}, nil)
		fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{"wiki/topics/r.md": page("R", "repo-github.com-o-r/README.md")})
		if _, err := Build(context.Background(), fx.fake, fx.opts); err != nil {
			t.Fatal(err)
		}
		fetch, _ := fx.fake.FindRun("-source-repo-github.com-o-r")
		argv := strings.Join(fetch.Argv, " ")
		// Three documents were staged first (incident report.pdf,
		// runbook.md, blob.bin), 22 bytes in all.
		if !strings.Contains(argv, "--max-files 1697 ") || !strings.HasSuffix(argv, "--max-bytes "+strconv.Itoa(2<<20-22)) {
			t.Errorf("the caps: %s", argv)
		}
	})
	t.Run("a refusal stops the build before the Creator runs", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		srcs, _ := ParseSources([]string{"https://github.com/o/big"}, nil)
		fx.opts.Sources = srcs
		playFetch(t, fx.fake, "repo-github.com-o-big", nil, func(rep *source.Report) {
			rep.Refusal = "the repository holds 2,500 files, past the 1,997-file cap"
			rep.Over = &source.Over{Cap: source.CapFiles, Of: source.OfFiles, Count: 2500}
			rep.Commit, rep.Ref, rep.Protocol = "", "", ""
		})
		_, err := Build(context.Background(), fx.fake, fx.opts)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "2,500 files") {
			t.Fatalf("err = %v", err)
		}
		if _, ok := fx.fake.FindRun("-agent"); ok {
			t.Error("the Creator ran")
		}
	})
	t.Run("documents past the pack's files are refused before the Creator runs", func(t *testing.T) {
		docs := make([]decode.Document, 0, knowledge.MaxFiles)
		for range knowledge.MaxFiles - PageReserve - 10 {
			docs = append(docs, decode.Document{Read: "x"})
		}
		if err := keptFilesWithin(10, docs); err != nil {
			t.Fatalf("a pack exactly at its cap was refused: %v", err)
		}
		docs[0].DecodedBy = "pdftotext"
		if err := keptFilesWithin(10, docs); err == nil || !strings.Contains(err.Error(), "at most 2000") {
			t.Fatalf("one file past the cap: %v", err)
		}
		unread := append(docs[1:], decode.Document{Unread: "not text"})
		if err := keptFilesWithin(10, unread); err != nil {
			t.Fatalf("an unread document counted: %v", err)
		}
	})
	t.Run("a file named as a given document is refused", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		srcs, _ := ParseSources(nil, []string{"https://example.org/runbook.md"})
		fx.opts.Sources = srcs
		playFetch(t, fx.fake, "url-1", map[string]string{"runbook.md": "# other\n"}, func(rep *source.Report) {
			*rep = source.Report{Schema: source.Schema, Kind: source.KindURL, Files: 1, Bytes: 8, Name: "runbook.md"}
		})
		_, err := Build(context.Background(), fx.fake, fx.opts)
		if err == nil || !strings.Contains(err.Error(), "would both be the document runbook.md") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("two files of a repository that would share a name are refused", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		srcs, _ := ParseSources([]string{"https://github.com/o/r"}, nil)
		fx.opts.Sources = srcs
		playFetch(t, fx.fake, "repo-github.com-o-r", map[string]string{"a b.md": "x\n", "a-b.md": "y\n"}, nil)
		_, err := Build(context.Background(), fx.fake, fx.opts)
		if err == nil || !strings.Contains(err.Error(), "would both be the document a-b.md") {
			t.Fatalf("err = %v", err)
		}
	})
}
