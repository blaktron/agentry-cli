//go:build windows

package claudesignin

import (
	"context"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHostCommandOpensNoConsoleWindow(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"auth", "login"}, {"auth", "status"}, {"auth", "logout"}} {
		cmd := hostCommand(context.Background(), "claude.exe", dir, args...)
		if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
			t.Fatalf("claude %v: CREATE_NO_WINDOW not set: %+v", args, cmd.SysProcAttr)
		}
		if !slices.Contains(cmd.Env, "CLAUDE_CONFIG_DIR="+dir) {
			t.Fatalf("claude %v: config directory not in its environment", args)
		}
	}
}
