//go:build windows

package procx

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNoWindowSetsTheFlagAndKeepsTheCommandLine(t *testing.T) {
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"cmd" /S`}
	NoWindow(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CREATE_NO_WINDOW not set: %#x", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.SysProcAttr.CmdLine != `"cmd" /S` {
		t.Fatalf("CmdLine lost: %q", cmd.SysProcAttr.CmdLine)
	}
	bare := exec.Command("cmd")
	NoWindow(bare)
	if bare.SysProcAttr == nil || bare.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("flag not set on a bare command: %+v", bare.SysProcAttr)
	}
}
