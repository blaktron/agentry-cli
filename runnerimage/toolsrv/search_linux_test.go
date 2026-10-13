//go:build linux

package toolsrv

import (
	"context"
	"testing"
)

// TestRipgrepRunsAsItsOwnUserWithNoEnvironment: as root, the command the
// server starts is the confine trampoline, as searchID, in the folder the
// call named, with an empty environment (plan ripgrep-search.md invariant
// 3), so ripgrep can read neither the server's environment nor its
// /proc/<pid>/environ.
func TestRipgrepRunsAsItsOwnUserWithNoEnvironment(t *testing.T) {
	s := searchServer(t, scratchSpec(t), "/opt/agentry/ripgrep/rg")
	s.search = chooseSearchConfinement("linux", 4, nil, 0, "/r")
	cmd := s.ripgrepCommand(context.Background(), s.spec.ScratchDir, []string{"-e", "x", "--", "."}, []string{s.spec.ScratchDir})
	if cmd.Path != "/r" || cmd.Dir != s.spec.ScratchDir || len(cmd.Env) != 0 || cmd.Env == nil {
		t.Errorf("command %s in %s with env %q, want /r in the scratch folder with an empty environment", cmd.Path, cmd.Dir, cmd.Env)
	}
	if c := cmd.SysProcAttr; c == nil || c.Credential == nil || c.Credential.Uid != searchID || c.Credential.Gid != searchID || len(c.Credential.Groups) != 0 {
		t.Errorf("credential %+v, want uid and gid %d with no groups", cmd.SysProcAttr, searchID)
	}
}
