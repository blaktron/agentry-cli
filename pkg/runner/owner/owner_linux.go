//go:build linux

package owner

import (
	"os"
	"strconv"
	"strings"
)

// Identity names the process namespace this process's pids mean something
// in: the kernel's boot id (pids and start times restart at boot), the pid
// namespace (a container's pids are its own), and the uid (a /proc mounted
// with hidepid shows a process only its own user's processes, so another
// user's live run would read as dead). Two processes with the same Identity
// can judge each other's liveness by pid and start time; nothing else can.
func Identity() (string, bool) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	ns, err := os.Readlink("/proc/self/ns/pid") // "pid:[4026531836]"
	if err != nil {
		return "", false
	}
	ns = strings.TrimSuffix(strings.TrimPrefix(ns, "pid:["), "]")
	b := strings.TrimSpace(string(boot))
	if b == "" || ns == "" {
		return "", false
	}
	return b + "." + ns + "." + strconv.Itoa(os.Getuid()), true
}

// StartTime reads a process's start time in clock ticks since boot — field
// 22 of /proc/<pid>/stat. With the pid it identifies a process across a pid
// reuse, which is what makes signalling a process group by id safe to attempt
// at all (the engine's host_unix.go) and what tells a crashed run's owner from
// a live one. ok is false when the process is gone or the record cannot be
// read.
func StartTime(pid int) (v uint64, ok bool) {
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
