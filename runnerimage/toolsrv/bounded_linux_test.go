//go:build linux

package toolsrv

import (
	"os"
	"strings"
	"testing"
)

// TestTheToolsRoleRefusesAnUnboundedScratch: a /tmp or scratch folder that
// is not a tmpfs stops the tools role before it serves:
// /dev/shm is a tmpfs on Linux, /proc is not.
func TestTheToolsRoleRefusesAnUnboundedScratch(t *testing.T) {
	if err := isBoundedTmpfs("/dev/shm", 1<<62); err != nil {
		t.Skipf("/dev/shm is not a tmpfs here: %v", err)
	}
	if err := isBoundedTmpfs("/dev/shm", 4096); err == nil || !strings.Contains(err.Error(), "over the run's 4096-byte bound") {
		t.Fatalf("a tmpfs larger than the bound passed: %v", err)
	}
	if err := isBoundedTmpfs("/proc", 1<<62); err == nil || !strings.Contains(err.Error(), "not the run's bounded tmpfs") {
		t.Fatalf("/proc passed as a tmpfs: %v", err)
	}
	if err := PrepareMounts(&Spec{TmpfsBytes: 1 << 62, ScratchDir: "/proc"}); err == nil {
		t.Fatal("a scratch folder on /proc passed")
	}
	// Without the filter's tmpfs (Docker Desktop) nothing is checked, and
	// the scratch folder is made, writable by anyone.
	dir := t.TempDir() + "/scratch"
	if err := PrepareMounts(&Spec{ScratchDir: dir}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o777 || fi.Mode()&os.ModeSticky == 0 {
		t.Fatalf("the unbounded scratch folder is %v (%v)", fi, err)
	}
}
