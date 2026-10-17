//go:build !windows && !darwin

package deliver

import "os"

// markOpen and markPublished do nothing: no system this builds for keeps a
// download flag that a file manager or an office suite reads.
func markOpen(*os.File) error { return nil }

func markPublished(string, string) error { return nil }
