package toolsrv

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// TestMain lets this test binary be run_command's confine trampoline, as
// the runner binary is in the tools container: os.Executable is the test
// binary, and ConfineRole its first argument.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ConfineRole {
		confine.Trampoline(os.Args[2:])
	}
	code := m.Run()
	removeOpenDir()
	os.Exit(code)
}

// TestChooseConfinement is the confinement decision table: Landlock
// wherever the kernel has it; without it, nobody when the contract grants
// no workspace writes, and no command at all when it does or when the
// server cannot change user; nothing off Linux, the debugging path.
func TestChooseConfinement(t *testing.T) {
	noLandlock := errors.New("landlock: function not implemented")
	reads := &Spec{WorkspaceDir: "/workspace", Exec: Exec{Commands: []string{"tar"}}}
	writes := &Spec{WorkspaceDir: "/workspace", FSWrite: Access{Enabled: true}, Exec: Exec{Commands: []string{"tar"}}}

	c := chooseConfinement(reads, "linux", 7, nil, 0, "/usr/local/bin/agentry-runner", "/tmp")
	if c.landlock != 7 || c.asNobody || c.refused != "" || strings.Join(c.writable, ",") != "/tmp" {
		t.Errorf("Landlock, no workspace writes = %+v, want ABI 7 writing /tmp alone", c)
	}
	if name, argv := c.argv("/tmp/shims/tar", []string{"-xf", "/inputs/a.tar"}); name != "/usr/local/bin/agentry-runner" ||
		strings.Join(argv, " ") != "confine --write /tmp -- /tmp/shims/tar -xf /inputs/a.tar" {
		t.Errorf("Landlock argv = %s %q", name, argv)
	}
	c = chooseConfinement(writes, "linux", 7, nil, 0, "/usr/local/bin/agentry-runner", "/tmp")
	if strings.Join(c.writable, ",") != "/tmp,/workspace" {
		t.Errorf("Landlock with workspace writes = %+v, want /tmp and /workspace", c)
	}

	c = chooseConfinement(reads, "linux", 0, noLandlock, 0, "/usr/local/bin/agentry-runner", "/tmp")
	if !c.asNobody || c.landlock != 0 || c.refused != "" {
		t.Errorf("no Landlock, no workspace writes = %+v, want nobody", c)
	}
	if name, argv := c.argv("/tmp/shims/tar", []string{"-tf", "a"}); name != "/tmp/shims/tar" || strings.Join(argv, " ") != "-tf a" {
		t.Errorf("nobody argv = %s %q, want the command itself", name, argv)
	}
	c = chooseConfinement(writes, "linux", 0, noLandlock, 0, "/usr/local/bin/agentry-runner", "/tmp")
	if c.refused == "" || !strings.Contains(c.refused, "may write its workspace") || c.asNobody {
		t.Errorf("no Landlock, workspace writes = %+v, want refused", c)
	}
	c = chooseConfinement(reads, "linux", 0, noLandlock, 1000, "/usr/local/bin/agentry-runner", "/tmp")
	if c.refused == "" || c.asNobody {
		t.Errorf("no Landlock, not root = %+v, want refused", c)
	}

	if c := chooseConfinement(writes, "darwin", 0, noLandlock, 501, "/usr/local/bin/agentry", "/tmp"); !c.none {
		t.Errorf("off Linux = %+v, want the unconfined debugging path", c)
	}
}

// TestRunCommandRefusedWhenItCannotBeConfined: a confinement that refused
// runs nothing, says why, and the description says so first.
func TestRunCommandRefusedWhenItCannotBeConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec tests use sh semantics")
	}
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Exec = Exec{Commands: []string{"sh"}}
	s := newTestServer(t, spec)
	s.confine = chooseConfinement(spec, "linux", 0, errors.New("landlock: function not implemented"), 0, "", "/tmp")
	marker := filepath.Join(spec.WorkspaceDir, "ran")
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "touch " + marker}}))
	if !isErr || !strings.Contains(got, "no declared command runs") {
		t.Fatalf("refused confinement = %q (isError %v)", got, isErr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran although its confinement refused it")
	}
	if d := s.runCommandDescription(); !strings.Contains(d, "No declared command runs:") {
		t.Errorf("description = %q, want the refusal", d)
	}
}

// TestRunCommandCannotWriteTheOutputFolder checks write confinement end to end
// under Landlock: a declared command reaches its scratch directory and
// cannot write, link into or truncate anything in the output folder — the
// reports and the receipts' ledgers write_report and the web tools keep
// there — while write_report itself still writes.
func TestRunCommandCannotWriteTheOutputFolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Landlock is Linux's")
	}
	abi, err := confine.ABI()
	if err != nil {
		t.Skipf("this kernel or runtime has no Landlock (%v); TestRunCommandAsNobodyCannotWriteTheOutputFolder covers it", err)
	}
	spec := baseSpec(t)
	spec.Exec = Exec{Commands: []string{"sh"}}
	s := newTestServer(t, spec)
	if s.confine.landlock == 0 {
		t.Fatalf("Landlock is available (ABI %d) but the server chose %+v", abi, s.confine)
	}
	// The test's directories are all under /tmp, the real scratch; this
	// server's scratch is one of its own so the output folder is outside it.
	scratch := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.confine = chooseConfinement(spec, "linux", abi, nil, os.Geteuid(), exe, scratch)
	assertOutputFolderHeld(t, s, scratch)
}

// TestRunCommandAsNobodyCannotWriteTheOutputFolder is the same without
// Landlock (gVisor): the command runs as nobody. It needs a server that may
// change user, which only root is.
func TestRunCommandAsNobodyCannotWriteTheOutputFolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the uid fallback is Linux's")
	}
	if os.Geteuid() != 0 {
		t.Skip("starting a command as another user needs root, which the tools container is and this test is not")
	}
	spec := baseSpec(t)
	spec.Exec = Exec{Commands: []string{"sh"}}
	s := newTestServer(t, spec)
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0o1777); err != nil { //nolint:gosec // the scratch tmpfs is world-writable in the container too
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Dir(scratch), spec.WorkspaceDir, spec.OutDir, filepath.Dir(spec.OutDir)} {
		_ = os.Chmod(d, 0o755) //nolint:gosec // traversable, as the run's mounts are
	}
	s.confine = chooseConfinement(spec, "linux", 0, syscall.ENOSYS, 0, "", scratch)
	assertOutputFolderHeld(t, s, scratch)
}

func assertOutputFolderHeld(t *testing.T, s *server, scratch string) {
	t.Helper()
	ledger := filepath.Join(s.spec.OutDir, "receipts", "web.ndjson")
	mustWrite(t, ledger, []byte("kept\n"))
	if err := os.MkdirAll(filepath.Join(s.spec.OutDir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.Join([]string{
		`echo a > "$1/a" && echo scratch=ok`,
		`(echo x > "$2/reports/planted.md") 2>/dev/null || echo create=refused`,
		`(echo x >> "$2/receipts/web.ndjson") 2>/dev/null || echo append=refused`,
		`(: > "$2/receipts/web.ndjson") 2>/dev/null || echo truncate=refused`,
		`ln -s /etc/passwd "$2/receipts/receipt.json" 2>/dev/null || echo symlink=refused`,
	}, "\n")
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", script, "sh", scratch, s.spec.OutDir}}))
	if isErr {
		t.Fatalf("run_command: %q", got)
	}
	for _, want := range []string{"scratch=ok", "create=refused", "append=refused", "truncate=refused", "symlink=refused"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("run_command output lacks %q:\n%s", want, got)
		}
	}
	if b, _ := os.ReadFile(ledger); string(b) != "kept\n" {
		t.Errorf("the ledger changed: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(s.spec.OutDir, "receipts", "receipt.json")); err == nil {
		t.Error("a declared command planted receipts/receipt.json")
	}
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "report.md", "content": "still mine"})); isErr {
		t.Errorf("write_report after the confined command = %q", got)
	}
}
