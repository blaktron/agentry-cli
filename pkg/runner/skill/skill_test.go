package skill

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodMD = "---\nname: release-notes\ndescription: Write release notes from the merged pull requests.\nlicense: MIT\n---\n\n# Release notes\n"

func writeSkill(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeZip(t *testing.T, path string, entries map[string]string, links ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for n, body := range entries {
		fw, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(body))
	}
	for _, n := range links {
		h := &zip.FileHeader{Name: n}
		h.SetMode(os.ModeSymlink | 0o777)
		fw, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("/etc/passwd"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestParseReadsTheFrontmatter(t *testing.T) {
	m, err := Parse([]byte(goodMD))
	if err != nil || m.Name != "release-notes" || m.Description != "Write release notes from the merged pull requests." {
		t.Fatalf("%+v %v", m, err)
	}
	// A folded description is one line.
	m, err = Parse([]byte("---\nname: a\ndescription: >\n  two\n  lines\n---\n"))
	if err != nil || m.Description != "two lines" {
		t.Errorf("folded: %+v %v", m, err)
	}
	for _, bad := range []string{
		"# no frontmatter\n",
		"---\nname: a\ndescription: x\n",
		"---\nname: Not-Lower\ndescription: x\n---\n",
		"---\nname: a--b\ndescription: x\n---\n",
		"---\nname: a\n---\n",
		"---\nname: " + strings.Repeat("a", 65) + "\ndescription: x\n---\n",
		"---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n",
		"---\nname: a\ndescription: \"a\\u202eb\"\n---\n",
		"---\nname: [a\n---\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("taken: %q", bad)
		}
	}
}

// TestStageTakesAFolderAsItIs: a folder named as its skill is copied, its
// scripts keep their executable bit, and its digest is its files'.
func TestStageTakesAFolderAsItIs(t *testing.T) {
	src := writeSkill(t, filepath.Join(t.TempDir(), "release-notes"), map[string]string{
		File: goodMD, "scripts/collect.sh": "#!/bin/sh\necho hi\n", "references/style.md": "be brief",
	})
	if err := os.Chmod(filepath.Join(src, "scripts/collect.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	dir, m, err := Stage(src, work)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "release-notes" {
		t.Errorf("meta: %+v", m)
	}
	fi, err := os.Stat(filepath.Join(dir, "scripts/collect.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the script: %v %v", fi.Mode(), err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "references/style.md")); fi.Mode().Perm() != 0o644 {
		t.Errorf("a reference: %v", fi.Mode())
	}
	d1, _ := Digest(src)
	d2, _ := Digest(dir)
	if d1 != d2 || !strings.HasPrefix(d1, "sha256:") {
		t.Errorf("digests %s %s", d1, d2)
	}
}

// TestStageRefusesWhatIsNotAPlainSkill: a link, a hard link, a name that
// is not the skill's, too many files, and no SKILL.md each refuse it, and
// nothing is left behind.
func TestStageRefusesWhatIsNotAPlainSkill(t *testing.T) {
	work := t.TempDir()
	cases := map[string]func(dir string){
		"a link": func(dir string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(dir, "passwd"))
		},
		"a hard link": func(dir string) {
			_ = os.Link(filepath.Join(dir, File), filepath.Join(dir, "again.md"))
		},
		"too many files": func(dir string) {
			for i := range MaxFiles + 1 {
				_ = os.WriteFile(filepath.Join(dir, "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+"-"+itoa(i)), []byte("x"), 0o644)
			}
		},
		"no SKILL.md": func(dir string) { _ = os.Remove(filepath.Join(dir, File)) },
	}
	for what, spoil := range cases {
		dir := writeSkill(t, filepath.Join(t.TempDir(), "release-notes"), map[string]string{File: goodMD})
		spoil(dir)
		if _, _, err := Stage(dir, work); err == nil {
			t.Errorf("%s was taken", what)
		}
	}
	other := writeSkill(t, filepath.Join(t.TempDir(), "other-name"), map[string]string{File: goodMD})
	if _, _, err := Stage(other, work); err == nil || !strings.Contains(err.Error(), "must agree") {
		t.Errorf("a folder of another name: %v", err)
	}
	if left, _ := os.ReadDir(work); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// TestStageTakesAZip: at its root or in one top folder; an entry that
// climbs out, a link and a bomb are refused.
func TestStageTakesAZip(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root.zip")
	writeZip(t, root, map[string]string{File: goodMD, "scripts/a.sh": "echo"})
	s, m, err := Stage(root, t.TempDir())
	if err != nil || m.Name != "release-notes" {
		t.Fatalf("at its root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s, "scripts/a.sh")); err != nil {
		t.Errorf("a nested file: %v", err)
	}
	top := filepath.Join(dir, "top.zip")
	writeZip(t, top, map[string]string{"release-notes/" + File: goodMD, "release-notes/x.md": "x"})
	if _, m, err := Stage(top, t.TempDir()); err != nil || m.Name != "release-notes" {
		t.Errorf("in a top folder: %+v %v", m, err)
	}
	wrongTop := filepath.Join(dir, "wrong.zip")
	writeZip(t, wrongTop, map[string]string{"notes/" + File: goodMD})
	if _, _, err := Stage(wrongTop, t.TempDir()); err == nil {
		t.Error("a top folder of another name was taken")
	}
	for name, entries := range map[string]map[string]string{
		"climb.zip": {File: goodMD, "../evil": "x"},
		"abs.zip":   {File: goodMD, "/etc/evil": "x"},
		"bomb.zip":  {File: goodMD, "big": strings.Repeat("0", MaxBytes+1)},
	} {
		p := filepath.Join(dir, name)
		writeZip(t, p, entries)
		if _, _, err := Stage(p, t.TempDir()); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	link := filepath.Join(dir, "link.zip")
	writeZip(t, link, map[string]string{File: goodMD}, "passwd")
	if _, _, err := Stage(link, t.TempDir()); err == nil {
		t.Error("a zip with a link was taken")
	}
	notZip := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(notZip, []byte(goodMD), 0o644)
	if _, _, err := Stage(notZip, t.TempDir()); err == nil {
		t.Error("a plain file was taken")
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}
