//go:build !windows

package managed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// maxMachineFile bounds the machine settings file.
const maxMachineFile = 256 << 10

// adminOwned says whether fi is owned by the administrator (uid 0) and
// writable by no one else. Tests replace it.
var adminOwned = func(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && fi.Mode().Perm()&0o022 == 0
}

// readAdminFile reads path when it is a regular file that only the
// administrator could have written: the file and every folder above it
// owned by root and writable by no group or other. found is false when
// there is no file.
func readAdminFile(path string) ([]byte, string, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, path, false, nil
	}
	if err != nil {
		return nil, path, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, path, true, errors.New("it is not a regular file")
	}
	if !adminOwned(fi) {
		return nil, path, true, errors.New("someone other than this computer's administrator can write it: it must be owned by root and writable by no group or other")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		di, err := os.Stat(dir)
		if err != nil {
			return nil, path, true, err
		}
		if !adminOwned(di) {
			return nil, path, true, fmt.Errorf("%s can be written by someone other than this computer's administrator", dir)
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if fi.Size() > maxMachineFile {
		return nil, path, true, fmt.Errorf("it is larger than %d bytes", maxMachineFile)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the administrator's settings file, checked above
	return raw, path, true, err
}
