//go:build linux

package toolsrv

import (
	"fmt"
	"os"
	"syscall"
)

// tmpfsMagic is the filesystem type statfs reports for a tmpfs (linux
// magic.h TMPFS_MAGIC; gVisor's tmpfs reports the same).
const tmpfsMagic = 0x01021994

// PrepareMounts readies the tools container's /tmp and scratch folder
// before the tools role serves (agentry-cli#599, #729). Where the run's
// docker filter mounts them as size-capped tmpfs (spec.TmpfsBytes, on a
// Linux host), it refuses to serve unless both are tmpfs no larger than
// that cap: without them a
// declared command or scratch_write would write the host's disk without
// bound, so the tools server never answers, the gateway's health check
// fails, and the run stops (exit 13) rather than run unbounded. Under
// Docker Desktop, which runs no filter, it makes the scratch folder in the
// container's own layer instead, anyone may write it, and neither is
// bounded. The image's tools role calls it; `agentry runner-tools`, the
// debugging path that never runs a QuickAgent, does not.
func PrepareMounts(spec *Spec) error {
	if spec.TmpfsBytes <= 0 {
		if spec.ScratchDir == "" {
			return nil
		}
		if err := os.MkdirAll(spec.ScratchDir, 0o755); err != nil {
			return fmt.Errorf("make the scratch folder: %w", err)
		}
		return os.Chmod(spec.ScratchDir, os.ModeSticky|0o777)
	}
	// os.TempDir is the directory a declared command is let write
	// (newConfinement), so it is the one held to the bound.
	dirs := []string{os.TempDir()}
	if spec.ScratchDir != "" {
		dirs = append(dirs, spec.ScratchDir)
	}
	for _, d := range dirs {
		if err := isBoundedTmpfs(d, spec.TmpfsBytes); err != nil {
			return err
		}
	}
	return nil
}

// isBoundedTmpfs says whether dir is a tmpfs of at most max bytes: the
// type alone would pass a tmpfs the gateway asked for itself, of any size.
func isBoundedTmpfs(dir string, max int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("%s is not the run's bounded tmpfs: %w", dir, err)
	}
	if int64(st.Type) != tmpfsMagic {
		return fmt.Errorf("%s is not the run's bounded tmpfs (filesystem type %#x); the docker filter did not mount it", dir, st.Type)
	}
	if size := int64(st.Blocks) * st.Bsize; size > max {
		return fmt.Errorf("%s is a tmpfs of %d bytes, over the run's %d-byte bound", dir, size, max)
	}
	return nil
}
