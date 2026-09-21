//go:build !windows

package procx

import (
	"os/exec"
	"syscall"
	"testing"
)

// The unix engine puts the harness in its own process group before the
// window rule is applied; the rule must leave that alone.
func TestNoWindowKeepsTheCallersAttributesOutsideWindows(t *testing.T) {
	cmd := exec.Command("true")
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd.SysProcAttr = attr
	NoWindow(cmd)
	if cmd.SysProcAttr != attr || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr changed: %+v", cmd.SysProcAttr)
	}
	bare := exec.Command("true")
	NoWindow(bare)
	if bare.SysProcAttr != nil {
		t.Fatalf("SysProcAttr created where nothing was asked: %+v", bare.SysProcAttr)
	}
}
