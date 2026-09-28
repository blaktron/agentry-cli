// Package plainpath checks a path under an os.Root before a tree that a
// container wrote is read: every part of the path is looked at with Lstat
// and must be a plain directory, and the last must be a regular file (or a
// plain directory, for Dir). A link, a junction, a device or any other
// special file anywhere on the way is refused, never followed.
//
// os.Root already refuses a link that leads out of the root. This does not
// rest on that: a Windows build run under Wine followed one, because
// os.Root's escape check there depends on reparse-point handling
// (agentry-cli#421). It also refuses links that stay inside the root, which
// os.Root follows. The trees it guards are written by a container that has
// ended before they are read, so nothing races the check.
package plainpath

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File checks that rel, a slash path under root, is a regular file reached
// through plain directories only, and returns its Lstat info (for a size
// check). A last part that is not a regular file is refused with "not a
// regular file".
func File(root *os.Root, rel string) (fs.FileInfo, error) {
	return check(root, rel, false)
}

// Dir checks that rel, a slash path under root, is a plain directory
// reached through plain directories only.
func Dir(root *os.Root, rel string) error {
	_, err := check(root, rel, true)
	return err
}

func check(root *os.Root, rel string, dir bool) (fs.FileInfo, error) {
	parts := strings.Split(rel, "/")
	var fi fs.FileInfo
	for i := range parts {
		sofar := strings.Join(parts[:i+1], "/")
		var err error
		if fi, err = root.Lstat(filepath.FromSlash(sofar)); err != nil {
			return nil, err
		}
		if i < len(parts)-1 || dir {
			// Exactly a directory: a link, a junction or a Windows
			// directory that is also a reparse point (ModeDir|ModeIrregular)
			// is not.
			if fi.Mode().Type() != fs.ModeDir {
				return nil, fmt.Errorf("%s is not a directory (a link, a file or a special file stands there)", sofar)
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("not a regular file")
		}
	}
	return fi, nil
}
