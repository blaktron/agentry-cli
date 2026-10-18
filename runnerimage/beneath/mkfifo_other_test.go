//go:build !unix

package beneath

import "errors"

func mkfifo(string) error { return errors.New("not supported") }
