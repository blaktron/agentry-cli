//go:build linux

package owner

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// TestLabelsNameThisProcess: the labels carry this process's identity, pid
// and start time, and this process is not dead by them.
func TestLabelsNameThisProcess(t *testing.T) {
	l := Labels()
	if l == nil {
		t.Skip("no /proc identity here")
	}
	id, _ := Identity()
	if l[LabelHost] != id || l[LabelPID] != strconv.Itoa(os.Getpid()) || l[LabelStart] == "" {
		t.Fatalf("labels = %v, want this process (%s, pid %d)", l, id, os.Getpid())
	}
	if Dead(l) {
		t.Fatal("this process reads as dead by its own labels")
	}
}

// TestDeadOnlyWhatItCanProve: an exited process is dead; a live pid with
// another start time (the pid reused) is dead; another identity, a missing
// or malformed label is never dead.
func TestDeadOnlyWhatItCanProve(t *testing.T) {
	self := Labels()
	if self == nil {
		t.Skip("no /proc identity here")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start, ok := StartTime(pid)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("the child exited before its start time was read")
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{LabelHost: self[LabelHost], LabelPID: strconv.Itoa(pid), LabelStart: strconv.FormatUint(start, 10)}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name   string
		labels map[string]string
		dead   bool
	}{
		{"an exited owner", with(LabelHost, self[LabelHost]), true},
		{"a reused pid", map[string]string{LabelHost: self[LabelHost], LabelPID: self[LabelPID], LabelStart: self[LabelStart] + "1"}, true},
		{"another host or namespace", with(LabelHost, "another.host.0"), false},
		{"no host label", with(LabelHost, ""), false},
		{"no pid label", with(LabelPID, ""), false},
		{"a malformed pid", with(LabelPID, "twelve"), false},
		{"a malformed start", with(LabelStart, "soon"), false},
		{"no labels at all", nil, false},
	}
	for _, c := range cases {
		if got := Dead(c.labels); got != c.dead {
			t.Errorf("%s: Dead = %v, want %v (%v)", c.name, got, c.dead, c.labels)
		}
	}
}

// TestUnreadableIsNotDead: a process record that exists but cannot be read
// (hidepid with a non-dumpable owner, an LSM denial) proves nothing, so its
// owner is not dead; only an absent record is.
func TestUnreadableIsNotDead(t *testing.T) {
	root := t.TempDir()
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })
	// A directory where the stat file should be: the read fails with
	// something other than "does not exist", as a denied read would, even
	// for root.
	if err := os.MkdirAll(root+"/4242/stat", 0o755); err != nil {
		t.Fatal(err)
	}
	if ownerDead(4242, 1) {
		t.Error("an unreadable record read as dead")
	}
	if !ownerDead(4243, 1) {
		t.Error("an absent record did not read as dead")
	}
	if err := os.MkdirAll(root+"/4244", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/4244/stat", []byte("4244 (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ownerDead(4244, 777) || !ownerDead(4244, 778) {
		t.Error("a readable record: same start time must be live, another start time dead")
	}
}
