package engine

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

func volumeNames(f *dockerx.Fake) []string {
	var out []string
	for _, v := range f.Volumes {
		out = append(out, v.Name)
	}
	slices.Sort(out)
	return out
}

// TestAPersistentAgentsVolumesBelongToItsRunnerHome: two homes running the
// same agent on one engine make volumes of their own, each carrying its
// home's identity and the agent's name, and a second start of either finds
// its own and makes none.
func TestAPersistentAgentsVolumesBelongToItsRunnerHome(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	for _, h := range []string{"aaaa", "bbbb"} {
		p.Persistent.Home = h
		p.Persistent.SessionVolume = "agentry-persistent-keeper-" + h + "-session"
		p.Persistent.DataVolume = "agentry-persistent-keeper-" + h + "-data"
		if err := ensurePersistentVolumes(context.Background(), f, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := volumeNames(f); len(got) != 4 {
		t.Fatalf("volumes %v, want each home's two", got)
	}
	for _, v := range f.Volumes {
		h := strings.Split(v.Name, "-")[3]
		if v.Labels[PersistentLabel] != "keeper" || v.Labels[PersistentHomeLabel] != h {
			t.Errorf("%s carries %v", v.Name, v.Labels)
		}
	}
	creates := 0
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "VolumeCreate:") {
			creates++
		}
	}
	if err := ensurePersistentVolumes(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "VolumeCreate:") {
			creates--
		}
	}
	if creates != 0 {
		t.Errorf("the second start made %d volume(s) that exist", -creates)
	}
}

func legacyVolumes(f *dockerx.Fake) {
	for _, n := range []string{"agentry-persistent-keeper-session", "agentry-persistent-keeper-data"} {
		f.Volumes = append(f.Volumes, dockerx.Labeled{Name: n, Labels: map[string]string{PersistentLabel: "keeper"}})
	}
}

// TestAHomeThatRanTheAgentCarriesTheSharedVolumesOver: the volumes an earlier
// build named by the agent alone are copied, read-only, into the home's own
// new volumes, and left as they are for the other homes that used them.
func TestAHomeThatRanTheAgentCarriesTheSharedVolumesOver(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	legacyVolumes(f)
	p.Persistent.Home = "aaaa"
	p.Persistent.SessionVolume, p.Persistent.DataVolume = "agentry-persistent-keeper-aaaa-session", "agentry-persistent-keeper-aaaa-data"
	p.Persistent.LegacySessionVolume, p.Persistent.LegacyDataVolume = "agentry-persistent-keeper-session", "agentry-persistent-keeper-data"
	if err := ensurePersistentVolumes(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, c := range f.Calls {
		if c.Op != "RunForeground" || !strings.HasSuffix(c.Run.Name, "-volume-copy") {
			continue
		}
		copies++
		var from, to dockerx.Mount
		for _, m := range c.Run.Mounts {
			switch m.Target {
			case "/from":
				from = m
			case "/to":
				to = m
			}
		}
		if !from.ReadOnly || to.ReadOnly || c.Run.Network != "none" || !strings.HasPrefix(to.Source, "agentry-persistent-keeper-aaaa-") ||
			from.Source != strings.Replace(to.Source, "-aaaa", "", 1) {
			t.Errorf("copy %+v from %+v to %+v", c.Run, from, to)
		}
	}
	if copies != 2 {
		t.Errorf("%d copies, want the session volume and the data volume", copies)
	}
	if got := volumeNames(f); len(got) != 4 {
		t.Errorf("volumes %v: the legacy two stay, the new two exist", got)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "VolumeRemove:") {
			t.Errorf("a volume was removed: %s", op)
		}
	}
}

// TestAHomeThatNeverRanTheAgentGetsNoneOfAnothersVolumes: with no legacy
// volume named, the new volumes start empty though the shared ones exist.
func TestAHomeThatNeverRanTheAgentGetsNoneOfAnothersVolumes(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	legacyVolumes(f)
	p.Persistent.Home = "bbbb"
	p.Persistent.SessionVolume, p.Persistent.DataVolume = "agentry-persistent-keeper-bbbb-session", "agentry-persistent-keeper-bbbb-data"
	if err := ensurePersistentVolumes(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:") {
			t.Errorf("a copy ran: %s", op)
		}
	}
}

// TestAFailedCopyLeavesNoHalfVolume: a copy that fails stops the start, and
// the volume it was filling is removed so the next start copies again.
func TestAFailedCopyLeavesNoHalfVolume(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	legacyVolumes(f)
	f.Foreground["-volume-copy"] = &dockerx.FakeForeground{ExitCode: 1, Stderr: "cp: no space left on device"}
	p.Persistent.Home = "aaaa"
	p.Persistent.SessionVolume, p.Persistent.DataVolume = "agentry-persistent-keeper-aaaa-session", "agentry-persistent-keeper-aaaa-data"
	p.Persistent.LegacySessionVolume, p.Persistent.LegacyDataVolume = "agentry-persistent-keeper-session", "agentry-persistent-keeper-data"
	err := ensurePersistentVolumes(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v", err)
	}
	if got := volumeNames(f); len(got) != 2 {
		t.Errorf("volumes %v, want only the shared two", got)
	}
}

// TestAnImageServersVolumeIsMadeOnceWithTheAgentsLabels: a segment whose
// plan has an image server makes that server's volume with the agent's
// labels when it does not exist, and leaves an existing one as it is: it
// holds the server's sign-in.
func TestAnImageServersVolumeIsMadeOnceWithTheAgentsLabels(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	p.Persistent.Home = "aaaa"
	p.Persistent.SessionVolume = "agentry-persistent-keeper-aaaa-session"
	p.Persistent.DataVolume = "agentry-persistent-keeper-aaaa-data"
	p.Persistent.ServerVolumes = []string{"agentry-persistent-keeper-aaaa-gog"}
	for range 2 {
		if err := ensurePersistentVolumes(context.Background(), f, p); err != nil {
			t.Fatal(err)
		}
	}
	made := 0
	for _, op := range f.Ops() {
		if op == "VolumeCreate:agentry-persistent-keeper-aaaa-gog" {
			made++
		}
	}
	if made != 1 {
		t.Fatalf("the server's volume was made %d times", made)
	}
	for _, v := range f.Volumes {
		if v.Name == "agentry-persistent-keeper-aaaa-gog" && (v.Labels[PersistentLabel] != "keeper" || v.Labels[PersistentHomeLabel] != "aaaa") {
			t.Errorf("the server's volume carries %v", v.Labels)
		}
	}
}
