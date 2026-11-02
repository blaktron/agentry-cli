package deliver

import "testing"

// mkfifo reports false: Windows has no named pipe in the file system.
func mkfifo(t *testing.T, _ string) bool {
	t.Helper()
	return false
}
