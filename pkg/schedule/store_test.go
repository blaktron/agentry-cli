package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func sample(t *testing.T) Schedule {
	t.Helper()
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return Schedule{
		Name: "Morning digest", Ref: "acme/digest", Version: "1.2.0", Digest: "sha256:ab",
		Run:     RunSpec{OutDir: "/home/p/reports", Intake: []string{"topic=go"}},
		When:    Recurrence{Kind: Daily, Time: "09:00", Zone: "UTC", Start: start},
		Created: start,
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	sc := sample(t)
	if err := s.Create(&sc, []byte("summarise the news")); err != nil {
		t.Fatal(err)
	}
	if !ValidID(sc.ID) || !sc.Run.Task {
		t.Fatalf("created %+v", sc)
	}
	got, err := s.Get(sc.ID)
	if err != nil || got.Name != sc.Name || got.Digest != sc.Digest || !got.When.Start.Equal(sc.When.Start) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	tp, err := s.TaskPath(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(tp); string(b) != "summarise the news" {
		t.Fatalf("task = %q", b)
	}
	for _, f := range []string{"schedule.yaml", "task.txt"} {
		fi, err := os.Stat(filepath.Join(s.Dir, sc.ID, f))
		// Windows keeps no POSIX modes; the runner home's own ACL holds there.
		if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
			t.Fatalf("%s mode %v, %v", f, fi.Mode(), err)
		}
	}
	list, bad, err := s.List()
	if err != nil || len(bad) != 0 || len(list) != 1 {
		t.Fatalf("List = %v %v %v", list, bad, err)
	}
	if err := s.Remove(sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestStoreRefusesPathsThatAreNotIDs(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, id := range []string{"../etc", "ABCDEFGHIJ", "short", "abcdefghij/..", ""} {
		if _, err := s.Get(id); err == nil {
			t.Errorf("Get(%q) succeeded", id)
		}
		if err := s.Remove(id); err == nil {
			t.Errorf("Remove(%q) succeeded", id)
		}
	}
}

func TestClaimIsOncePerOccurrence(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	sc := sample(t)
	if err := s.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	occ := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Claim(sc.ID, occ)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d claims won, want 1", wins)
	}
	s.Prune(sc.ID, occ.Add(time.Second))
	if ok, _ := s.Claim(sc.ID, occ); !ok {
		t.Fatal("a pruned claim was not free again")
	}
}

func TestFiresAndState(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	sc := sample(t)
	if err := s.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		occ := time.Date(2026, 10, 9+i, 9, 0, 0, 0, time.UTC)
		if err := s.AppendFire(sc.ID, Fire{Occurrence: occ, Outcome: Ran, RunID: "r" + string(rune('0'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	fires, err := s.Fires(sc.ID, 2)
	if err != nil || len(fires) != 2 || fires[1].RunID != "r2" {
		t.Fatalf("Fires = %+v %v", fires, err)
	}
	st := State{Last: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	if err := s.SaveState(sc.ID, st); err != nil {
		t.Fatal(err)
	}
	if got, err := s.State(sc.ID); err != nil || !got.Last.Equal(st.Last) {
		t.Fatalf("State = %+v %v", got, err)
	}
}

func TestDue(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := Recurrence{Kind: Daily, Time: "09:00", Zone: "UTC", Start: start}
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }

	// On time.
	run, missed := Due(r, State{}, day(9, 9))
	if !run.Equal(day(9, 9)) || len(missed) != 0 {
		t.Fatalf("on time: %v %v", run, missed)
	}
	// Late but before the following occurrence: runs, nothing missed.
	run, missed = Due(r, State{Last: day(9, 9)}, day(10, 20))
	if !run.Equal(day(10, 9)) || len(missed) != 0 {
		t.Fatalf("late: %v %v", run, missed)
	}
	// Off for three days: the latest runs, the two before are missed.
	run, missed = Due(r, State{Last: day(9, 9)}, day(13, 10))
	if !run.Equal(day(13, 9)) || len(missed) != 3 || !missed[0].Equal(day(10, 9)) {
		t.Fatalf("away: %v %v", run, missed)
	}
	// A ring with nothing due (early, or a superset calendar day).
	run, missed = Due(r, State{Last: day(13, 9)}, day(13, 23))
	if !run.IsZero() || missed != nil {
		t.Fatalf("nothing due: %v %v", run, missed)
	}
	// An occurrence exactly at the start counts.
	r2 := Recurrence{Kind: Daily, Time: "12:00", Zone: "UTC", Start: start}
	if run, _ := Due(r2, State{}, start); !run.Equal(start) {
		t.Fatalf("at start: %v", run)
	}
}
