//go:build linux

package confine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets the test binary be the trampoline: run with
// AGENTRY_CONFINE_HELPER=1, it is `confine` and never runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTRY_CONFINE_HELPER") == "1" {
		Trampoline(os.Args[1:])
	}
	os.Exit(m.Run())
}

// confined runs script under sh through the trampoline, writable beneath
// dirs, and returns its combined output.
func confined(t *testing.T, dirs []string, script string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}
	args := []string{}
	for _, d := range dirs {
		args = append(args, "--write", d)
	}
	args = append(args, "--", sh, "-c", script)
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // the test binary, re-executed as the trampoline
	cmd.Env = append(os.Environ(), "AGENTRY_CONFINE_HELPER=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestExecConfinesWrites is agentry-cli#596 at the kernel: beneath the
// writable directory a command creates, links and removes; anywhere else
// it can create nothing, link nothing, and neither change nor truncate a
// file it can read. /dev/null stays writable, and reading is unchanged.
func TestExecConfinesWrites(t *testing.T) {
	if _, err := ABI(); err != nil {
		t.Skipf("this kernel or runtime has no Landlock (%v); the tools server's uid fallback covers it", err)
	}
	scratch, out := t.TempDir(), t.TempDir()
	ledger := filepath.Join(out, "web.ndjson")
	if err := os.WriteFile(ledger, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := strings.Join([]string{
		`echo a > "$S/a" && echo scratch-write=ok`,
		`mkdir "$S/d" && ln -s /etc/passwd "$S/d/l" && rm "$S/d/l" && rmdir "$S/d" && echo scratch-tree=ok`,
		`(echo x > "$O/new") 2>/dev/null || echo out-create=refused`,
		`(echo x >> "$O/web.ndjson") 2>/dev/null || echo out-append=refused`,
		`(: > "$O/web.ndjson") 2>/dev/null || echo out-truncate=refused`,
		`ln -s /etc/passwd "$O/link" 2>/dev/null || echo out-symlink=refused`,
		`mkdir "$O/dir" 2>/dev/null || echo out-mkdir=refused`,
		`rm -f "$O/web.ndjson" 2>/dev/null; [ -e "$O/web.ndjson" ] && echo out-remove=refused`,
		`echo x > /dev/null && echo devnull=ok`,
		`cat "$O/web.ndjson"`,
	}, "\n")
	// Each refused write is in a subshell: a failed redirection of a
	// special builtin (`:`) ends a non-interactive shell.
	t.Setenv("S", scratch)
	t.Setenv("O", out)
	got, err := confined(t, []string{scratch}, script)
	if err != nil {
		t.Fatalf("confined sh: %v\n%s", err, got)
	}
	for _, want := range []string{"scratch-write=ok", "scratch-tree=ok", "out-create=refused", "out-append=refused",
		"out-truncate=refused", "out-symlink=refused", "out-mkdir=refused", "out-remove=refused", "devnull=ok", "kept"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("confined output lacks %q:\n%s", want, got)
		}
	}
	if b, _ := os.ReadFile(ledger); string(b) != "kept\n" {
		t.Errorf("the file outside the writable directory changed: %q", b)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Errorf("the directory outside the writable one holds %d entries, want only the ledger", len(entries))
	}
}

// TestMainRefusesBadUsage: the trampoline never runs a program it was
// not told how to confine.
func TestMainRefusesBadUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"no program":       {"--write", "/tmp", "--"},
		"no separator":     {"--write", "/tmp", "/bin/true"},
		"relative dir":     {"--write", "tmp", "--", "/bin/true"},
		"unknown flag":     {"--read", "/tmp", "--", "/bin/true"},
		"flag with no dir": {"--write"},
	} {
		if err := Main(args, nil); err == nil {
			t.Errorf("%s: Main(%q) = nil, want a refusal", name, args)
		}
	}
}
