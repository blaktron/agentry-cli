package toolsrv

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

func scratchSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.ScratchDir = t.TempDir()
	return spec
}

// TestScratchToolsFollowTheirGrant: the scratch tools exist exactly when
// the spec names a scratch folder, and they are not the workspace's tools.
func TestScratchToolsFollowTheirGrant(t *testing.T) {
	spec := baseSpec(t)
	for _, name := range spec.GrantedTools() {
		if strings.HasPrefix(name, "scratch_") {
			t.Fatalf("no scratch folder, but %s is granted", name)
		}
	}
	spec.ScratchDir = t.TempDir()
	got := spec.GrantedTools()
	for _, want := range []string{ToolScratchList, ToolScratchRead, ToolScratchWrite} {
		if !slices.Contains(got, want) {
			t.Fatalf("scratch folder set, %s missing from %v", want, got)
		}
	}
	for _, fs := range []string{ToolFSRead, ToolFSList, ToolFSWrite} {
		if slices.Contains(got, fs) {
			t.Fatalf("the scratch folder granted the workspace's %s: %v", fs, got)
		}
	}
}

func TestScratchWriteListRead(t *testing.T) {
	spec := scratchSpec(t)
	s := newTestServer(t, spec)

	if got, isErr := s.toolScratchList(rawArgs(t, map[string]any{})); isErr || got != "(empty directory)" {
		t.Fatalf("a new scratch folder lists %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "notes/plan.md", "content": "one\n"})); isErr {
		t.Fatalf("write: %s", got)
	}
	if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "notes/plan.md", "content": "two\n", "append": true})); isErr || !strings.HasPrefix(got, "appended 4 bytes") {
		t.Fatalf("append: %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchRead(rawArgs(t, map[string]any{"path": "notes/plan.md"})); isErr || got != "one\ntwo\n" {
		t.Fatalf("read back %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchList(rawArgs(t, map[string]any{})); isErr || got != "notes/" {
		t.Fatalf("root lists %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchList(rawArgs(t, map[string]any{"path": "notes"})); isErr || got != "plan.md\t8" {
		t.Fatalf("notes/ lists %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchRead(rawArgs(t, map[string]any{"path": "notes"})); !isErr || !strings.Contains(got, "use scratch_list") {
		t.Fatalf("reading a directory answered %q (error %v)", got, isErr)
	}
	// Nothing of it lands in the output folder or the workspace.
	for _, dir := range []string{spec.OutDir, spec.WorkspaceDir} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("a scratch write left %v in %s", entries, dir)
		}
	}
	// A declared command run as nobody can change what the agent wrote.
	fi, err := os.Stat(filepath.Join(spec.ScratchDir, "notes", "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != scratchFileMode {
		t.Fatalf("a scratch file is %v, want %v", fi.Mode().Perm(), scratchFileMode)
	}
	di, err := os.Stat(filepath.Join(spec.ScratchDir, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != scratchDirMode {
		t.Fatalf("a scratch directory is %v, want %v", di.Mode().Perm(), scratchDirMode)
	}
}

// TestScratchStaysInTheScratchFolder: the scratch tools resolve every path
// under the folder, as the workspace's tools do under theirs.
func TestScratchStaysInTheScratchFolder(t *testing.T) {
	spec := scratchSpec(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("outside"))
	if err := os.Symlink(outside, filepath.Join(spec.ScratchDir, "link")); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)
	for _, p := range []string{"../x", "/etc/passwd", "link/secret.txt", `a\b`} {
		if got, isErr := s.toolScratchRead(rawArgs(t, map[string]any{"path": p})); !isErr {
			t.Fatalf("scratch_read %q answered %q", p, got)
		}
		if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": p, "content": "x"})); !isErr {
			t.Fatalf("scratch_write %q answered %q", p, got)
		}
	}
	if got, isErr := s.toolScratchList(rawArgs(t, map[string]any{"path": "link"})); !isErr {
		t.Fatalf("scratch_list through a link answered %q", got)
	}
}

func TestScratchWriteRefusesARunSecretAndTheCap(t *testing.T) {
	t.Setenv("FETCH_TOKEN", "scratch-fixture-value")
	spec := scratchSpec(t)
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.example.com"}, SecretEnv: []string{"FETCH_TOKEN"}}
	spec.Limits.MaxWriteBytes = 8
	s := newTestServer(t, spec)
	if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "a", "content": "scratch-fixture-value"})); !isErr || !strings.Contains(got, "FETCH_TOKEN") || strings.Contains(got, "scratch-fixture") {
		t.Fatalf("a run secret written to scratch answered %q (error %v)", got, isErr)
	}
	if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "a", "content": "123456789"})); !isErr || !strings.Contains(got, "over the 8-byte limit") {
		t.Fatalf("an over-cap write answered %q (error %v)", got, isErr)
	}
	if _, err := os.Stat(filepath.Join(spec.ScratchDir, "a")); !os.IsNotExist(err) {
		t.Fatalf("a refused write left a file: %v", err)
	}
}

// TestADeclaredCommandMayWriteTheScratchFolder: confine lets a declared
// command write the scratch folder beside /tmp (DR-63 extended).
func TestADeclaredCommandMayWriteTheScratchFolder(t *testing.T) {
	spec := scratchSpec(t)
	c := chooseConfinement(spec, "linux", 7, nil, 0, "/usr/local/bin/agentry-runner", "/tmp")
	if !slices.Equal(c.writable, []string{"/tmp", spec.ScratchDir}) {
		t.Fatalf("writable = %v", c.writable)
	}
	if runtime.GOOS != "linux" {
		return
	}
	abi, err := confine.ABI()
	if err != nil {
		t.Skipf("no Landlock here (%v)", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec.Exec = Exec{Commands: []string{"sh"}}
	s := newTestServer(t, spec)
	s.confine = chooseConfinement(spec, "linux", abi, nil, os.Geteuid(), exe, t.TempDir())
	script := `echo hi > "$1/made.txt" && echo scratch=ok; (echo x > "$2/planted") 2>/dev/null || echo out=refused`
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", script, "sh", spec.ScratchDir, spec.OutDir}}))
	if isErr || !strings.Contains(got, "scratch=ok") || !strings.Contains(got, "out=refused") {
		t.Fatalf("run_command answered %q (error %v)", got, isErr)
	}
}

// TestScratchWritesNeverFollowALinkOut: a declared command may write the
// scratch folder, so it may plant a link there, or swap a path for one
// after the name was checked. The scratch tools work through an os.Root,
// which never resolves outside the folder: a link out to the output folder
// is refused at the moment of use, so nothing lands in reports/.
func TestScratchWritesNeverFollowALinkOut(t *testing.T) {
	spec := scratchSpec(t)
	reports := filepath.Join(spec.OutDir, "reports")
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(reports, filepath.Join(spec.ScratchDir, "notes")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(spec.ScratchDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	// What the tool does after its name check, as a swap would meet it.
	if err := mkdirAllIn(root, "notes/deeper", scratchDirMode); err == nil {
		t.Fatal("mkdirAllIn followed a link out of the scratch folder")
	}
	if f, err := root.OpenFile("notes/planted.md", os.O_CREATE|os.O_WRONLY, scratchFileMode); err == nil {
		_ = f.Close()
		t.Fatal("a write followed a link out of the scratch folder")
	}
	s := newTestServer(t, spec)
	if got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "notes/planted.md", "content": "x"})); !isErr {
		t.Fatalf("scratch_write through a link answered %q", got)
	}
	if entries, _ := os.ReadDir(reports); len(entries) != 0 {
		t.Fatalf("a scratch write landed in reports/: %v", entries)
	}
}

// TestAFullScratchFolderIsNoFault: a write past the folder's cap is the
// agent's own doing and never counts as a fault of the run's channel in
// the calls ledger (DR-72's lesson); another I/O error still does.
func TestAFullScratchFolderIsNoFault(t *testing.T) {
	s := newTestServer(t, scratchSpec(t))
	if got, _ := s.scratchFailed(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}); s.callFault || !strings.Contains(got, "the scratch folder is full") {
		t.Fatalf("ENOSPC: fault %v, %q", s.callFault, got)
	}
	if _, _ = s.scratchFailed(&os.PathError{Op: "write", Path: "x", Err: syscall.EIO}); !s.callFault {
		t.Fatal("EIO in the scratch folder is not a fault")
	}
}
