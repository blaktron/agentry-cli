//go:build linux

package toolsrv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Without Landlock, a command that may write its workspace runs as nobody,
// who owns the workspace: it writes there, files the server wrote there
// before become its own, and it cannot write the output folder.
func TestWithoutLandlockOwnershipHoldsAWorkspaceWritingCommand(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root on Linux, to change a command's user and a file's owner")
	}
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Exec = Exec{Commands: []string{"sh"}}
	spec.WorkspaceOwnership = true
	s := newTestServer(t, spec)
	s.confine = chooseConfinement(spec, "linux", 0, errors.New("landlock: function not implemented"), 0, "", "/tmp")
	before := filepath.Join(spec.WorkspaceDir, "before.txt")
	if err := os.WriteFile(before, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The test's temporary folders are root's alone; a container's are
	// traversable, so are these for the unprivileged command.
	for _, d := range []string{spec.WorkspaceDir, spec.OutDir} {
		for p := d; p != "/" && p != os.TempDir(); p = filepath.Dir(p) {
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.workOwner = &Owner{UID: unprivilegedID, GID: unprivilegedID}
	s.ownTree(spec.WorkspaceDir)
	inWorkspace := filepath.Join(spec.WorkspaceDir, "made.txt")
	inOut := filepath.Join(spec.OutDir, "forged.txt")
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "echo start; echo y >> " + before + " && echo before-ok; : > " + inWorkspace + " && echo made-ok; (: > " + inOut + ") 2>/dev/null && echo forged-ok; echo end"}}))
	if isErr || !strings.Contains(got, "exit code 0") || !strings.Contains(got, "before-ok") || !strings.Contains(got, "made-ok") || strings.Contains(got, "forged-ok") || !strings.Contains(got, "end") {
		t.Fatalf("run_command = %q (isError %v)", got, isErr)
	}
	if fi, err := os.Stat(inWorkspace); err != nil || fi.Sys().(*syscall.Stat_t).Uid != unprivilegedID {
		t.Fatalf("the command did not write its workspace as the unprivileged user: %v", err)
	}
	if _, err := os.Stat(inOut); err == nil {
		t.Fatal("the command wrote the output folder")
	}
	if err := s.writeWorkspace("server.txt", "z"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(spec.WorkspaceDir, "server.txt")); err != nil || fi.Sys().(*syscall.Stat_t).Uid != unprivilegedID {
		t.Fatalf("the server's own write is not the command's to change: %v", err)
	}
}
