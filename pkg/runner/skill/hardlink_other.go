//go:build !unix

package skill

import "io/fs"

// hardLinked says the file has another name elsewhere; this platform does
// not say, and the copy is made from the bytes all the same.
func hardLinked(fs.FileInfo) bool { return false }
