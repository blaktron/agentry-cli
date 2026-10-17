//go:build windows

package schedule

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestSchtasksOpensNoConsoleWindow(t *testing.T) {
	cmd := schtasksCommand(`C:\Windows\System32\schtasks.exe`, "/Query", "/TN", `\Agentry\`)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CREATE_NO_WINDOW not set: %+v", cmd.SysProcAttr)
	}
}
