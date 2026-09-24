//go:build linux

package owner

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
)

// StartTime reads a process's start time in clock ticks since boot — field
// 22 of /proc/<pid>/stat. With the pid it identifies a process across a pid
// reuse, which is what makes signalling a process group by id safe to attempt
// at all (the engine's host_unix.go). ok is false when the process is gone or
// the record cannot be read.
func StartTime(pid int) (v uint64, ok bool) {
	v, st := lookup(pid)
	return v, st == stateLive
}

// procRoot is where process records are read; a test points it elsewhere.
var procRoot = "/proc"

type procState int

const (
	stateUnknown procState = iota // the record exists or may exist, but could not be read
	stateGone                     // no such process in this pid namespace
	stateLive                     // read: v is its start time
)

// lookup reads pid's start time and says which of the three it found. Only
// an absent record is "gone": a record that exists but cannot be read (a
// /proc mounted with hidepid and a non-dumpable owner, an LSM denying the
// read) says nothing about whether the process is alive.
func lookup(pid int) (uint64, procState) {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // an integer pid, never a path from outside
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, stateGone
		}
		return 0, stateUnknown
	}
	s := string(b)
	// The comm field is parenthesised and may itself contain spaces and
	// parentheses, so the remaining fields are counted from the last ')':
	// state is field 3 and lands at index 0, so field 22 is index 19.
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, stateUnknown
	}
	fields := strings.Fields(s[i+1:])
	const startTimeField = 19
	if len(fields) <= startTimeField {
		return 0, stateUnknown
	}
	v, err := strconv.ParseUint(fields[startTimeField], 10, 64)
	if err != nil {
		return 0, stateUnknown
	}
	return v, stateLive
}

// ownerDead: gone, or live under that pid with another start time (the pid
// reused). Unreadable is not dead.
func ownerDead(pid int, start uint64) bool {
	now, st := lookup(pid)
	switch st {
	case stateGone:
		return true
	case stateLive:
		return now != start
	default:
		return false
	}
}

// Identity names the scope inside which a pid and a start time identify a
// process: the kernel's boot id (pids and start times restart at boot), the
// pid namespace (a container's pids are its own), the time namespace (the
// start time is reported as the reader's time namespace sees it), and the
// uid (a /proc mounted with hidepid shows a process only its own user's
// processes). Two processes with the same Identity can judge each other's
// liveness by pid and start time; nothing else can. ok is false where any
// part cannot be read, or where /proc is not this process's own (a pid
// namespace entered without remounting /proc), and then nothing is labelled.
var Identity = sync.OnceValues(func() (string, bool) {
	if self, err := os.Readlink("/proc/self"); err != nil || self != strconv.Itoa(os.Getpid()) {
		return "", false
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	b := strings.TrimSpace(string(boot))
	pidNS, ok := nsInode("pid")
	if b == "" || !ok {
		return "", false
	}
	// Kernels before 5.6 have no time namespace: every process shares the
	// one clock, which "-" stands for.
	timeNS, ok := nsInode("time")
	if !ok {
		if _, err := os.Lstat("/proc/self/ns/time"); !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		timeNS = "-"
	}
	return b + "." + pidNS + "." + timeNS + "." + strconv.Itoa(os.Getuid()), true
})

// nsInode is the inode of one of this process's namespaces, from the
// "kind:[inode]" link /proc/self/ns/<kind> reads as.
func nsInode(kind string) (string, bool) {
	l, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		return "", false
	}
	v := strings.TrimSuffix(strings.TrimPrefix(l, kind+":["), "]")
	if v == "" || v == l {
		return "", false
	}
	return v, true
}
