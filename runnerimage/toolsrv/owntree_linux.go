//go:build linux

package toolsrv

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ownTree gives every entry beneath dir that another user owns to
// workOwner, without following a link. A workspace an earlier segment's
// server wrote as root becomes the command's to write.
func (s *server) ownTree(dir string) {
	o := s.workOwner
	if o == nil || dir == "" {
		return
	}
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == o.UID && int(st.Gid) == o.GID {
			return nil
		}
		if err := os.Lchown(p, o.UID, o.GID); err == nil {
			n++
		}
		return nil
	})
	if err != nil {
		s.logf("run_command: own %s to the command's user: %v", dir, err)
	}
	if n > 0 {
		s.logf("run_command: %d entries beneath %s are now the command's user's", n, dir)
	}
}
