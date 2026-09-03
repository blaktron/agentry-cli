package minimize

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Minimize keeps the declared commands (and the busybox binary behind a
// declared applet), the runner binary, and nothing else executable.
func TestMinimizeKeepsOnlyDeclared(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ln := func(rel, target string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	mk("bin/busybox", "bb")
	ln("bin/sh", filepath.Join(root, "bin/busybox"))
	ln("bin/ls", filepath.Join(root, "bin/busybox"))
	ln("usr/bin/wget", filepath.Join(root, "bin/busybox"))
	mk("usr/bin/jq", "jq")
	mk("usr/bin/curl", "curl")
	mk("usr/local/bin/agentry-runner", "runner")
	mk("usr/lib/libz.so", "lib") // libraries are not touched

	if err := Minimize(root, []string{"jq", "ls"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bin/busybox", "bin/ls", "usr/bin/jq", "usr/local/bin/agentry-runner", "usr/lib/libz.so"} {
		if _, err := os.Lstat(filepath.Join(root, want)); err != nil {
			t.Errorf("%s must survive: %v", want, err)
		}
	}
	for _, gone := range []string{"bin/sh", "usr/bin/wget", "usr/bin/curl"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s must be removed", gone)
		}
	}

	// No busybox applet declared: busybox itself goes too.
	root2 := t.TempDir()
	root = root2
	mk("bin/busybox", "bb")
	ln("bin/sh", filepath.Join(root, "bin/busybox"))
	mk("usr/bin/jq", "jq")
	mk("usr/local/bin/agentry-runner", "runner")
	if err := Minimize(root, []string{"jq"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "bin/busybox")); err == nil {
		t.Error("busybox must be removed when no applet is declared")
	}
	if err := Minimize(root, []string{"../evil"}, io.Discard); err == nil {
		t.Error("a path must be refused")
	}
}
