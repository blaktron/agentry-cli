//go:build linux

package engine

import (
	"os"
	"strconv"
	"strings"
)

// procStartTime reads a process's start time in clock ticks since boot —
// field 22 of /proc/<pid>/stat. With the pid it identifies a process across a
// pid reuse, which is what makes signalling a process group by id safe to
// attempt at all (host_unix.go). ok is false when the process is gone or the
// record cannot be read.
func procStartTime(pid int) (v uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // a pid we started, not a path from outside
	if err != nil {
		return 0, false
	}
	s := string(b)
	// The comm field is parenthesised and may itself contain spaces and
	// parentheses, so the remaining fields are counted from the last ')':
	// state is field 3 and lands at index 0, so field 22 is index 19.
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	const startTimeField = 19
	if len(fields) <= startTimeField {
		return 0, false
	}
	if v, err = strconv.ParseUint(fields[startTimeField], 10, 64); err != nil {
		return 0, false
	}
	return v, true
}
