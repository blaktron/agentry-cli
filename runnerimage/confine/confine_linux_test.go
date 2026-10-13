//go:build linux

package confine

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMain lets the test binary be the trampoline: run with
// AGENTRY_CONFINE_HELPER=1, it is `confine` and never runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTRY_CONFINE_HELPER") == "1" {
		if len(os.Args) == 1 {
			// The program the trampoline started (TestNoNetworkRefusesTCP).
			dial(os.Getenv("AGENTRY_CONFINE_DIAL"))
		}
		Trampoline(os.Args[1:])
	}
	os.Exit(m.Run())
}

// dial is the confined program of TestNoNetworkRefusesTCP: it says whether
// a TCP connection to addr was refused, and exits.
func dial(addr string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Println("dial refused:", err)
		os.Exit(0)
	}
	_ = c.Close()
	fmt.Println("dial connected")
	os.Exit(0)
}

// confined runs script under sh through the trampoline, writable beneath
// dirs, and returns its combined output.
func confined(t *testing.T, dirs []string, script string) (string, error) {
	t.Helper()
	args := []string{}
	for _, d := range dirs {
		args = append(args, "--write", d)
	}
	return trampoline(t, args, script)
}

// trampoline runs script under sh through the trampoline with flags.
func trampoline(t *testing.T, flags []string, script string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}
	args := append(append([]string{}, flags...), "--", sh, "-c", script)
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
		"no program":          {"--write", "/tmp", "--"},
		"no separator":        {"--write", "/tmp", "/bin/true"},
		"relative dir":        {"--write", "tmp", "--", "/bin/true"},
		"unknown flag":        {"--read", "/tmp", "--", "/bin/true"},
		"flag with no dir":    {"--write"},
		"memory not a number": {"--max-memory", "lots", "--", "/bin/true"},
		"memory zero":         {"--max-memory", "0", "--", "/bin/true"},
		"limits-only writes":  {"--limits-only", "--write", "/tmp", "--", "/bin/true"},
		"limits-only network": {"--limits-only", "--no-network", "--", "/bin/true"},
		"limits-only reads":   {"--limits-only", "--read", "/tmp", "--", "/bin/true"},
		"relative read":       {"--read", "tmp", "--", "/bin/true"},
	} {
		if err := Main(args, nil); err == nil {
			t.Errorf("%s: Main(%q) = nil, want a refusal", name, args)
		}
	}
}

// TestMaxMemoryLimitsTheAddressSpace (plan scratch-and-pdf-reports.md §5,
// D24): the program starts with RLIMIT_AS at the limit, with or without
// Landlock, and cannot raise it.
func TestMaxMemoryLimitsTheAddressSpace(t *testing.T) {
	flags := [][]string{{"--limits-only", "--max-memory", "1073741824"}}
	if _, err := ABI(); err == nil {
		flags = append(flags, []string{"--write", t.TempDir(), "--max-memory", "1073741824"})
	}
	for _, f := range flags {
		got, err := trampoline(t, f, `ulimit -v; ulimit -v unlimited 2>/dev/null && echo raised || echo held`)
		if err != nil {
			t.Fatalf("%q: %v\n%s", f, err, got)
		}
		if got != "1048576\nheld\n" {
			t.Errorf("%q: the program saw %q, want a 1048576 KiB address space it cannot raise", f, got)
		}
	}
}

// TestMaxMemoryNeverKillsTheTrampoline (agentry-cli#887): between the
// address-space limit and the execve no goroutine of the trampoline may
// need memory, or the Go runtime dies of the limit, on the program's stderr
// or in its place. Many limited runs at once load the machine as a busy
// tools server does; each must print the program's output and nothing else.
// Under the race detector, as `make test` runs it, it failed about one run
// in ten before the fix; a plain build needs a busier machine to show it.
func TestMaxMemoryNeverKillsTheTrampoline(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this machine")
	}
	const runs = 96
	var wg sync.WaitGroup
	errs := make(chan string, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := trampoline(t, []string{"--limits-only", "--max-memory", "1073741824"}, `echo ran`)
			if err != nil || got != "ran\n" {
				errs <- fmt.Sprintf("%q (%v)", got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("a limited run printed %s, want only the program's \"ran\"", e)
	}
}

// TestLimitsOnlyAppliesNoLandlock: --limits-only runs the program with no
// Landlock ruleset, so it writes where its user may (the caller picks the
// user), and still under no_new_privs.
func TestLimitsOnlyAppliesNoLandlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("D", dir)
	got, err := trampoline(t, []string{"--limits-only"}, `echo x > "$D/f" && echo wrote; grep NoNewPrivs /proc/self/status`)
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if !strings.Contains(got, "wrote\n") || !strings.Contains(got, "NoNewPrivs:\t1") {
		t.Errorf("limits-only = %q, want the write and no_new_privs", got)
	}
}

// TestNoNetworkRefusesTCP: at Landlock ABI 4 and later a program started
// with --no-network can neither connect nor bind over TCP; the caller is
// told beforehand where it cannot be refused (NetworkDenied).
func TestNoNetworkRefusesTCP(t *testing.T) {
	abi, err := ABI()
	if err != nil || !NetworkDenied(abi) {
		t.Skipf("this kernel's Landlock (ABI %d, %v) cannot refuse TCP", abi, err)
	}
	if NetworkDenied(netABI - 1) {
		t.Fatal("NetworkDenied claims ABI 3 refuses TCP")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	// The test binary is the program, re-executed to dial.
	cmd := exec.Command(os.Args[0], "--write", t.TempDir(), "--no-network", "--", os.Args[0]) //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), "AGENTRY_CONFINE_HELPER=1", "AGENTRY_CONFINE_DIAL="+ln.Addr().String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "dial refused") {
		t.Errorf("the confined dial = %q, want refused", got)
	}
	// And the same dial unconfined reaches the listener, so the refusal
	// is the rule's.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("the unconfined dial failed: %v", err)
	}
	_ = c.Close()
}

// TestReadConfinesReads is plan ripgrep-search.md §5 at the kernel: with
// --read, a program reads and lists beneath the named directories and
// nowhere else, so a link planted inside a readable directory cannot take
// it out (grep and glob's ripgrep). Without --read, reading is unchanged
// (TestExecConfinesWrites).
func TestReadConfinesReads(t *testing.T) {
	if _, err := ABI(); err != nil {
		t.Skipf("this kernel or runtime has no Landlock (%v); the tools server's uid fallback covers it", err)
	}
	in, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "a.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "secret.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "secret.txt"), filepath.Join(in, "link")); err != nil {
		t.Fatal(err)
	}
	// sh, its loader and its libraries live under the system directories,
	// which the program must be let read and run.
	flags := []string{"--read", in}
	for _, d := range []string{"/usr", "/lib", "/lib64", "/bin", "/etc"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			flags = append(flags, "--read", d)
		}
	}
	t.Setenv("I", in)
	t.Setenv("O", out)
	got, err := trampoline(t, flags, strings.Join([]string{
		`cat "$I/a.txt"`,
		`ls "$I" >/dev/null && echo in-list=ok`,
		`cat "$O/secret.txt" 2>/dev/null || echo out-read=refused`,
		`cat "$I/link" 2>/dev/null || echo link-read=refused`,
		`ls "$O" >/dev/null 2>&1 || echo out-list=refused`,
		`echo x > /dev/null && echo devnull=ok`,
	}, "\n"))
	if err != nil {
		t.Fatalf("confined sh: %v\n%s", err, got)
	}
	for _, want := range []string{"inside", "in-list=ok", "out-read=refused", "link-read=refused", "out-list=refused", "devnull=ok"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("confined output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outside") {
		t.Errorf("the program read a file outside the readable directories:\n%s", got)
	}
}
