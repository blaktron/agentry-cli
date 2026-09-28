package knowledge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var applyNow = time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)

// outputs lays a Creator's output out: the files and a changes.json.
func outputs(t *testing.T, c Changes, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if c.Schema == "" {
		c.Schema = ChangesSchema
	}
	b, _ := json.Marshal(c)
	files[ChangesFile] = string(b)
	write(t, dir, files)
	return dir
}

func opts() ApplyOptions {
	return ApplyOptions{Name: "aws-docs", Now: applyNow, Creator: Creator{CLI: "agentry 0.9.0 (test)", Model: "test-model"}}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyMakesANewPack(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "aws-docs")
	out := outputs(t, Changes{
		Write:     []string{"wiki/topics/buckets.md", "sources/guide.txt"},
		Documents: []string{"guide.pdf"},
		Summary:   "one page from the guide",
	}, map[string]string{
		"wiki/topics/buckets.md": page("Buckets", "Naming", "Names are unique [src: doc:guide.txt].\n"),
		"sources/guide.txt":      "the guide\n",
	})
	res, err := Apply(packDir, out, opts())
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(packDir)
	if err != nil {
		t.Fatalf("the result does not load: %v", err)
	}
	m := p.Manifest
	if m.Name != "aws-docs" || m.Pages != 1 || m.ID == "" || !m.Created.Equal(applyNow) || !m.Updated.Equal(applyNow) ||
		m.Creator.Model != "test-model" || m.Scan.Scanned || m.SHA256 != res.Manifest.SHA256 || m.ID != res.Manifest.ID {
		t.Fatalf("manifest %+v (result %+v)", m, res.Manifest)
	}
	if fs, _ := Lint(packDir, "aws-docs"); len(fs) != 0 {
		t.Fatalf("the result does not lint: %v", fs)
	}
	log := read(t, filepath.Join(packDir, "wiki", "log.md"))
	if !strings.Contains(log, "## [2026-09-25] ingest | guide.pdf\n\none page from the guide\n") || !strings.Contains(log, "- wrote: sources/guide.txt, wiki/topics/buckets.md") {
		t.Fatalf("log =\n%s", log)
	}
	if !strings.Contains(read(t, filepath.Join(packDir, "wiki", "index.md")), "- [Buckets](topics/buckets.md) — Naming") {
		t.Fatal("the index does not list the page")
	}
}

func TestApplyIsIncremental(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "aws-docs")
	first := outputs(t, Changes{Write: []string{"wiki/topics/buckets.md", "sources/guide.txt"}, Documents: []string{"guide.pdf"}},
		map[string]string{"wiki/topics/buckets.md": page("Buckets", "Naming", "v1 [src: doc:guide.txt]\n"), "sources/guide.txt": "guide\n"})
	r1, err := Apply(packDir, first, opts())
	if err != nil {
		t.Fatal(err)
	}
	later := opts()
	later.Now = applyNow.Add(time.Hour)
	second := outputs(t, Changes{Write: []string{"wiki/topics/buckets.md", "wiki/topics/policies.md", "sources/iam.txt"}, Documents: []string{"iam.pdf"}},
		map[string]string{
			"wiki/topics/buckets.md":  page("Buckets", "Naming", "v2, see [policies](policies.md) [src: doc:guide.txt]\n"),
			"wiki/topics/policies.md": page("Policies", "Who", "JSON [src: doc:iam.txt]\n"),
			"sources/iam.txt":         "iam\n",
		})
	r2, err := Apply(packDir, second, later)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Manifest.ID != r1.Manifest.ID || !r2.Manifest.Created.Equal(applyNow) || !r2.Manifest.Updated.Equal(later.Now) || r2.Manifest.Pages != 2 {
		t.Fatalf("second manifest %+v", r2.Manifest)
	}
	if got := read(t, filepath.Join(packDir, "sources", "guide.txt")); got != "guide\n" {
		t.Fatalf("an earlier source was lost: %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(packDir, "wiki", "topics", "buckets.md")), "v2") {
		t.Fatal("the rewrite did not land")
	}
	if log := read(t, filepath.Join(packDir, "wiki", "log.md")); strings.Count(log, "] ingest | ") != 2 {
		t.Fatalf("the log is not append-only:\n%s", log)
	}
}

func TestApplyDropsAFailingPageNotTheBuild(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "aws-docs")
	if _, err := Apply(packDir, outputs(t, Changes{Write: []string{"wiki/topics/buckets.md", "sources/guide.txt"}},
		map[string]string{"wiki/topics/buckets.md": page("Buckets", "Naming", "v1 [src: doc:guide.txt]\n"), "sources/guide.txt": "g\n"}), opts()); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(packDir, outputs(t, Changes{
		Write:  []string{"wiki/topics/buckets.md", "wiki/topics/broken.md", "wiki/topics/good.md", "wiki/topics/leans.md"},
		Delete: []string{"sources/guide.txt"},
	}, map[string]string{
		// A rewrite that fails keeps the page as it was.
		"wiki/topics/buckets.md": "no frontmatter now\n",
		// A new page that fails is not added.
		"wiki/topics/broken.md": page("Broken", "s", "cites a document the pack lacks [src: doc:missing.txt]\n"),
		// A page that passes lands.
		"wiki/topics/good.md": page("Good", "s", "fine [src: doc:guide.txt]\n"),
		// A page whose only fault is a link to a dropped page is kept, the
		// link made plain text, so the drop does not cascade (agentry-cli#433).
		"wiki/topics/leans.md": page("Leans", "s", "see [the broken page](broken.md) [src: doc:guide.txt]\n"),
	}), opts())
	if err != nil {
		t.Fatalf("the build failed on its pages: %v", err)
	}
	if !strings.Contains(read(t, filepath.Join(packDir, "wiki", "topics", "buckets.md")), "v1") {
		t.Fatal("a failing rewrite replaced the page")
	}
	if _, err := os.Stat(filepath.Join(packDir, "wiki", "topics", "broken.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("broken.md landed")
	}
	if got := read(t, filepath.Join(packDir, "wiki", "topics", "leans.md")); !strings.Contains(got, "see the broken page [src: doc:guide.txt]") || strings.Contains(got, "](broken.md)") {
		t.Fatalf("leans.md was not kept with its dead link unlinked:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(packDir, "wiki", "topics", "good.md")); err != nil {
		t.Fatal("the good page did not land")
	}
	// Three pages cite the source, so its delete is kept back, once, and
	// none of them is dropped for it.
	if _, err := os.Stat(filepath.Join(packDir, "sources", "guide.txt")); err != nil {
		t.Fatal("a delete that broke citations went through")
	}
	var dropped []string
	for _, d := range res.Dropped {
		dropped = append(dropped, d.Path)
	}
	if strings.Join(dropped, ",") != "sources/guide.txt,wiki/topics/broken.md,wiki/topics/buckets.md,wiki/topics/leans.md" {
		t.Fatalf("dropped %v", res.Dropped)
	}
	log := read(t, filepath.Join(packDir, "wiki", "log.md"))
	for _, want := range []string{"- dropped wiki/topics/broken.md: ", "- dropped wiki/topics/buckets.md: ", "- kept sources/guide.txt: ", "- kept wiki/topics/leans.md: links to pages not in the pack made plain text: broken.md"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if fs, _ := Lint(packDir, "aws-docs"); len(fs) != 0 {
		t.Fatalf("the result does not lint: %v", fs)
	}
}

func TestApplyRefusals(t *testing.T) {
	tests := []struct {
		name    string
		changes Changes
		files   map[string]string
		opts    func(*ApplyOptions)
		wantErr string
		is      error
	}{
		{name: "the index is generated", changes: Changes{Write: []string{"wiki/index.md"}}, files: map[string]string{"wiki/index.md": "x"}, wantErr: "generated"},
		{name: "the log is appended", changes: Changes{Write: []string{"wiki/log.md"}}, files: map[string]string{"wiki/log.md": "x"}, wantErr: "appended"},
		{name: "the manifest is the step's", changes: Changes{Write: []string{"manifest.json"}}, files: map[string]string{"manifest.json": "{}"}, wantErr: "wiki/ or sources/"},
		{name: "a path out of the pack", changes: Changes{Write: []string{"wiki/../../x.md"}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		{name: "an absolute path", changes: Changes{Delete: []string{"/etc/passwd"}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		// Windows reads a backslash as a separator and a colon as a drive or
		// a stream: neither is ever a name here.
		{name: "a backslash climbing out", changes: Changes{Delete: []string{`wiki/..\..\..\x`}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		{name: "a drive", changes: Changes{Delete: []string{"sources/C:x"}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		{name: "a dotted part", changes: Changes{Delete: []string{"wiki/./x.md"}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		{name: "an empty part", changes: Changes{Delete: []string{"wiki//x.md"}}, files: map[string]string{}, wantErr: "wiki/ or sources/"},
		{name: "a section's generated index", changes: Changes{Write: []string{"wiki/topics/index.md"}}, files: map[string]string{"wiki/topics/index.md": "x"}, wantErr: "generated"},
		{name: "a file the changes name but do not carry", changes: Changes{Write: []string{"sources/absent.txt"}}, files: map[string]string{}, wantErr: "sources/absent.txt"},
		{name: "another changes schema", changes: Changes{Schema: "knowledge/changes/v9"}, files: map[string]string{}, wantErr: "knowledge/changes/v9"},
		{name: "no name for a new pack", changes: Changes{}, files: map[string]string{}, opts: func(o *ApplyOptions) { o.Name = "" }, wantErr: "name"},
		{name: "past the cap", changes: Changes{Write: []string{"sources/big.bin"}}, files: map[string]string{"sources/big.bin": strings.Repeat("x", MaxBytes)}, is: ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packDir := filepath.Join(t.TempDir(), "aws-docs")
			if _, err := Apply(packDir, outputs(t, Changes{Write: []string{"sources/guide.txt"}}, map[string]string{"sources/guide.txt": "g\n"}), opts()); err != nil {
				t.Fatal(err)
			}
			before, _ := TreeDigest(packDir)
			if tc.name == "no name for a new pack" {
				packDir = filepath.Join(t.TempDir(), "fresh")
			}
			o := opts()
			if tc.opts != nil {
				tc.opts(&o)
			}
			_, err := Apply(packDir, outputs(t, tc.changes, tc.files), o)
			if err == nil {
				t.Fatal("Apply accepted it")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.name != "no name for a new pack" {
				if after, _ := TreeDigest(packDir); after != before {
					t.Fatal("a refused apply changed the pack")
				}
			}
		})
	}

	t.Run("a linked directory in the outputs does not reach out of them", func(t *testing.T) {
		packDir := filepath.Join(t.TempDir(), "aws-docs")
		outside := t.TempDir()
		write(t, outside, map[string]string{"secret.txt": "host file\n"})
		out := outputs(t, Changes{Write: []string{"sources/linked/secret.txt"}}, map[string]string{})
		if err := os.MkdirAll(filepath.Join(out, "sources"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(out, "sources", "linked")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		// Removed before the temporary directories are: under Wine,
		// RemoveAll does not remove a directory link inside a directory.
		t.Cleanup(func() { _ = os.Remove(link) })
		if _, err := Apply(packDir, out, opts()); err == nil {
			t.Fatal("Apply read through a linked directory out of the outputs")
		}
		if _, err := os.Stat(packDir); err == nil {
			t.Fatal("a pack was made from it")
		}
	})
	t.Run("a linked directory that stays in the outputs is refused too", func(t *testing.T) {
		// os.Root follows a link inside its root; only the check refuses
		// it, so this case holds the check on any host (agentry-cli#421).
		packDir := filepath.Join(t.TempDir(), "aws-docs")
		out := outputs(t, Changes{Write: []string{"sources/linked/page.md"}}, map[string]string{"other/page.md": "# Page\n"})
		if err := os.MkdirAll(filepath.Join(out, "sources"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(out, "sources", "linked")
		// Relative, so os.Root follows it (it refuses an absolute
		// target by itself): only the check refuses this.
		if err := os.Symlink(filepath.Join("..", "other"), link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		if _, err := Apply(packDir, out, opts()); err == nil || !strings.Contains(err.Error(), "sources/linked is not a directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a changes.json that is a link is not read", func(t *testing.T) {
		packDir := filepath.Join(t.TempDir(), "aws-docs")
		outside := outputs(t, Changes{Write: []string{"wiki/page.md"}}, map[string]string{"wiki/page.md": "# Page\n"})
		out := t.TempDir()
		if err := os.Symlink(filepath.Join(outside, ChangesFile), filepath.Join(out, ChangesFile)); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := Apply(packDir, out, opts()); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Apply read a changes.json through a link: err = %v", err)
		}
		if _, err := os.Stat(packDir); err == nil {
			t.Fatal("a pack was made from it")
		}
	})
	t.Run("a link in the outputs is refused", func(t *testing.T) {
		packDir := filepath.Join(t.TempDir(), "aws-docs")
		out := outputs(t, Changes{Write: []string{"sources/link.txt"}}, map[string]string{})
		if err := os.MkdirAll(filepath.Join(out, "sources"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/hostname", filepath.Join(out, "sources", "link.txt")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := Apply(packDir, out, opts()); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("err = %v", err)
		}
	})
}

// A stopped run's pages link to pages it never wrote. Each is kept with
// those links made plain text rather than dropped, so a chain of pages
// linking forward survives; a link that points outside the pack, or any
// other fault, still drops the page (agentry-cli#433).
func TestApplyKeepsAPageWhoseOnlyFaultIsADeadLink(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "aws-docs")
	res, err := Apply(packDir, outputs(t, Changes{
		Write: []string{"wiki/topics/a.md", "wiki/topics/b.md", "wiki/topics/outside.md", "wiki/topics/twofaults.md", "sources/guide.txt"},
	}, map[string]string{
		"wiki/topics/a.md":         page("A", "s", "see [B](b.md) and [C, not written](c.md \"C\") [src: doc:guide.txt]\n"),
		"wiki/topics/b.md":         page("B", "s", "see [D](../entities/d.md) [src: doc:guide.txt]\n\n`[code](e.md)` stays\n"),
		"wiki/topics/outside.md":   page("Outside", "s", "see [x](../../../escape.md) [src: doc:guide.txt]\n"),
		"wiki/topics/twofaults.md": page("Two", "s", "see [y](y.md) [src: doc:missing.txt]\n"),
		"sources/guide.txt":        "g\n",
	}), opts())
	if err != nil {
		t.Fatal(err)
	}
	a := read(t, filepath.Join(packDir, "wiki", "topics", "a.md"))
	if !strings.Contains(a, "see [B](b.md) and C, not written [src") {
		t.Fatalf("a.md =\n%s", a)
	}
	b := read(t, filepath.Join(packDir, "wiki", "topics", "b.md"))
	if !strings.Contains(b, "see D [src") || !strings.Contains(b, "`[code](e.md)` stays") {
		t.Fatalf("b.md =\n%s", b)
	}
	for _, gone := range []string{"outside.md", "twofaults.md"} {
		if _, err := os.Stat(filepath.Join(packDir, "wiki", "topics", gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s landed", gone)
		}
	}
	var kept []string
	for _, d := range res.Dropped {
		if d.Kept {
			kept = append(kept, d.Path)
		}
	}
	if strings.Join(kept, ",") != "wiki/topics/a.md,wiki/topics/b.md" || res.Manifest.Pages != 2 {
		t.Fatalf("dropped %+v, pages %d", res.Dropped, res.Manifest.Pages)
	}
	if fs, _ := Lint(packDir, "aws-docs"); len(fs) != 0 {
		t.Fatalf("the result does not lint: %v", fs)
	}
}

// The unlink reads a page as the lint does, and a page it cannot make pass
// is dropped rather than landing with a finding: a bracket lines above a
// dead link is not taken for the link's start, the same link in code is
// left as written, an image loses its '!', the frontmatter is not
// touched; a link whose text holds a code span with ']' (the raw text
// cannot match it) and a nest of links whose unlink uncovers one that
// points outside the pack are dropped, and the result lints clean
// (agentry-cli#433's review).
func TestApplyUnlinksAsTheLintReads(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "aws-docs")
	res, err := Apply(packDir, outputs(t, Changes{
		Write: []string{"wiki/topics/prose.md", "wiki/topics/stuck.md", "wiki/topics/nested.md", "sources/guide.txt"},
	}, map[string]string{
		"wiki/topics/prose.md":  page("Prose", "s", "arr[i is odd [src: doc:guide.txt]\n\nsee [D](d.md), `[D](d.md)` and ![a map](d.md)\n\n```\n[D](d.md)\n```\n"),
		"wiki/topics/stuck.md":  page("Stuck", "s", "see [a `]` b](d.md) [src: doc:guide.txt]\n"),
		"wiki/topics/nested.md": page("Nested", "s", "[[[[a](d.md)](e.md)](f.md)](../../../x.md) [src: doc:guide.txt]\n"),
		"sources/guide.txt":     "g\n",
	}), opts())
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(packDir, "wiki", "topics", "prose.md"))
	for _, want := range []string{"arr[i is odd [src: doc:guide.txt]", "see D, `[D](d.md)` and a map", "```\n[D](d.md)\n```", "title: Prose"} {
		if !strings.Contains(got, want) {
			t.Errorf("prose.md lacks %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"stuck.md", "nested.md"} {
		if _, err := os.Stat(filepath.Join(packDir, "wiki", "topics", gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s landed", gone)
		}
	}
	notes := map[string]int{}
	for _, d := range res.Dropped {
		notes[d.Path]++
		if d.Kept && d.Path != "wiki/topics/prose.md" {
			t.Errorf("%s is reported kept although it was dropped: %+v", d.Path, d)
		}
	}
	for p, n := range notes {
		if n != 1 {
			t.Errorf("%s is reported %d times: %+v", p, n, res.Dropped)
		}
	}
	if fs, _ := Lint(packDir, "aws-docs"); len(fs) != 0 {
		t.Fatalf("the result does not lint: %v", fs)
	}
}
