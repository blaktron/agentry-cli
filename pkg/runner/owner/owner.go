// Package owner labels the docker objects a run creates with the process that
// created them, and tells a live owner from a dead one, so a later run on the
// same host can remove what a crashed run left behind without touching a
// sibling run that is still going.
//
// Every run-scoped object is keyed by a random run id and nothing else, and
// several `agentry run` processes can share one docker daemon at once (a
// person's two terminals, a sandbox host taking several hosted runs). So "not
// this process's run id" is no test of abandonment; "its owner is dead" is,
// provided the owner can be identified across pid reuse — a pid and its start
// time, judged only by a process in the same boot, pid namespace and uid
// (Identity). Where that cannot be established (off Linux, an unreadable
// /proc), nothing is labelled and nothing is swept: a leftover is litter, a
// sibling's container removed mid-turn is a broken run.
package owner

import (
	"maps"
	"os"
	"strconv"
	"sync"
)

// The labels an owned object carries.
const (
	LabelHost  = "agentry.owner.host"  // Identity() of the creating process
	LabelPID   = "agentry.owner.pid"   // its pid
	LabelStart = "agentry.owner.start" // its StartTime
)

// Labels are this process's owner labels, or nil where Identity or the
// process's own start time cannot be read. Read once: none of it changes
// for the life of the process.
var Labels = func() func() map[string]string {
	once := sync.OnceValue(func() map[string]string {
		id, ok := Identity()
		if !ok {
			return nil
		}
		pid := os.Getpid()
		start, ok := StartTime(pid)
		if !ok {
			return nil
		}
		return map[string]string{
			LabelHost:  id,
			LabelPID:   strconv.Itoa(pid),
			LabelStart: strconv.FormatUint(start, 10),
		}
	})
	return func() map[string]string { return maps.Clone(once()) }
}()

// With returns labels plus this process's owner labels, as a new map.
func With(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, Labels())
	return out
}

// Dead reports whether the owner a set of labels names is provably gone:
// labelled from this process's Identity, with a pid and start time that
// parse, and either no process with that pid or one with another start time.
// Anything it cannot prove — another identity, a missing or malformed label,
// a process record that exists but cannot be read, a pid that is still the
// same process — is not dead.
func Dead(labels map[string]string) bool {
	id, ok := Identity()
	if !ok || labels[LabelHost] != id {
		return false
	}
	pid, err := strconv.Atoi(labels[LabelPID])
	if err != nil || pid <= 0 {
		return false
	}
	start, err := strconv.ParseUint(labels[LabelStart], 10, 64)
	if err != nil {
		return false
	}
	return ownerDead(pid, start)
}
