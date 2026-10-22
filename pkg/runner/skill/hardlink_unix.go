//go:build unix

package skill

import (
	"io/fs"
	"syscall"
)

// hardLinked says the file has another name elsewhere.
func hardLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
