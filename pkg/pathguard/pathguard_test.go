package pathguard

import (
	"path/filepath"
	"testing"
)

func abs(parts ...string) string {
	return filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
}

func TestRefuseDangerousDir(t *testing.T) {
	home := abs("home", "me")
	for dir, refused := range map[string]bool{
		"":                                     true,
		"relative":                             true,
		string(filepath.Separator):             true,
		home:                                   true,
		abs("home"):                            true,
		abs("home", "me", ".claude", "skills"): false,
		abs("home", "me", ".claude", "skills", "x"): false,
		abs("tmp", "elsewhere"):                     false,
	} {
		err := RefuseDangerousDir(dir, home)
		if (err != nil) != refused {
			t.Errorf("RefuseDangerousDir(%q) = %v, want refused=%v", dir, err, refused)
		}
	}
}

func TestDirectlyUnder(t *testing.T) {
	root := abs("home", "me", ".claude", "skills")
	for dir, want := range map[string]bool{
		filepath.Join(root, "demo"):            true,
		filepath.Join(root, "demo", "nested"):  false,
		root:                                   false,
		filepath.Join(root, ".."):              false,
		filepath.Join(root, "..", "..", "x"):   false,
		filepath.Join(root, "a", "..", "demo"): true, // cleans to root/demo
		"":                                     false,
	} {
		if got := DirectlyUnder(root, dir); got != want {
			t.Errorf("DirectlyUnder(%s, %s) = %v, want %v", root, dir, got, want)
		}
	}
}

func TestUnder(t *testing.T) {
	dir := abs("home", "me", ".claude", "skills", "demo")
	for p, want := range map[string]bool{
		filepath.Join(dir, "SKILL.md"):        true,
		filepath.Join(dir, "docs", "more.md"): true,
		dir:                                   false,
		filepath.Join(dir, ".."):              false,
		filepath.Join(dir, "..", "other", "SKILL.md"):         false,
		filepath.Join(dir, "..", "..", "..", "..", ".bashrc"): false,
		abs("etc"): false,
	} {
		if got := Under(dir, p); got != want {
			t.Errorf("Under(%s, %s) = %v, want %v", dir, p, got, want)
		}
	}
}
