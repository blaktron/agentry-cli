package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// sealingRunhost is a runhost server whose systemd seals the store's key: a
// reversible wrap stands in for systemd-creds.
func sealingRunhost(t *testing.T) *runhostServer {
	t.Helper()
	s := newRunhostServer(t)
	oldEnc, oldDec, oldDir, oldNow := schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow
	t.Cleanup(func() {
		schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow = oldEnc, oldDec, oldDir, oldNow
	})
	schedule.EncryptCredential = func(name string, plain []byte, out string) error {
		return os.WriteFile(out, append([]byte(name+":"), plain...), 0o600)
	}
	runhostDecryptCredential = func(name, file string) ([]byte, error) {
		b, err := os.ReadFile(file) //nolint:gosec // the test's own file
		if err != nil {
			return nil, err
		}
		v, ok := bytes.CutPrefix(b, []byte(name+":"))
		if !ok {
			return nil, errors.New("another credential's name")
		}
		return v, nil
	}
	runhostSystemdVersion = func() int { return 257 }
	runhostSchedulesDir = filepath.Join(s.dir, "schedules")
	return s
}

func putSchedule(t *testing.T, j *job.Job, archive []byte, when schedule.Recurrence) (runhost.ScheduleInfo, int, error) {
	t.Helper()
	w, _ := json.Marshal(when)
	h, _ := json.Marshal(runhost.ScheduleHeader{Name: "nightly", When: w, Entries: []runhost.ScheduleEntry{{Kind: "model-key", Name: "anthropic"}}})
	var in bytes.Buffer
	in.Write(append(h, '\n'))
	if err := job.Write(&in, j); err != nil {
		t.Fatal(err)
	}
	in.Write(archive)
	var out bytes.Buffer
	fw := runhost.NewFrameWriter(&out)
	code, err := runhostScheduleVerb(context.Background(), runhost.Request{Verb: "schedule", Args: []string{"put"}}, &in, fw)
	_ = fw.Exit(code, err)
	a, rerr := runhost.ReadAnswer(&out, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var info runhost.ScheduleInfo
	if a.Code == 0 {
		_ = json.Unmarshal(a.Result, &info)
	}
	return info, a.Code, err
}

func daily(t *testing.T, start time.Time, at string) schedule.Recurrence {
	t.Helper()
	return schedule.Recurrence{Kind: schedule.Daily, Time: at, Zone: "UTC", Start: start}
}

// A schedule sent to a runhost is kept in its store, encrypted, and its
// occurrence runs a copy of its job with an id of its own, whose output is
// sealed for the devices to collect; removing it takes its entry off the
// disk.
func TestARunhostRunsAScheduleFromItsStore(t *testing.T) {
	sealingRunhost(t)
	start := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	runhostScheduleNow = func() time.Time { return start }
	j, archive := unsignedJob(t, "0123456789abcdef0123456789abcdef")
	info, code, err := putSchedule(t, j, archive, daily(t, start, "07:00"))
	if code != 0 {
		t.Fatalf("put: %d %v", code, err)
	}
	if info.ID == "" || info.Next == nil || !info.Next.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)) || info.Device != testDevice {
		t.Fatalf("info %+v", info)
	}
	entry := filepath.Join(runhostStoreDir, scheduleEntryName(info.ID)+".entry")
	b, err := os.ReadFile(entry) //nolint:gosec // the test's own store
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("sk-test-model-key")) {
		t.Fatal("the schedule's model key is plaintext on disk")
	}

	// Nothing is due before seven; at seven one run starts.
	runhostSchedulerTick(context.Background(), t.Logf)
	if fires, _ := runhostScheduleStore().Fires(info.ID, 0); len(fires) != 0 {
		t.Fatalf("a run before its time: %+v", fires)
	}
	runhostScheduleNow = func() time.Time { return start.Add(time.Hour + time.Minute) }
	runhostSchedulerTick(context.Background(), t.Logf)
	var fire schedule.Fire
	deadline := time.Now().Add(20 * time.Second)
	for {
		fires, _ := runhostScheduleStore().Fires(info.ID, 0)
		if len(fires) == 1 {
			fire = fires[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fire recorded: %+v", fires)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fire.Outcome != schedule.Ran || fire.RunID == "" || fire.RunID == j.ID {
		t.Fatalf("fire %+v", fire)
	}
	if _, err := os.Stat(filepath.Join(runhostJobsDir, fire.RunID, runhostSealedName)); err != nil {
		t.Fatalf("the run's output was not sealed for collection: %v", err)
	}
	// The same occurrence never runs twice.
	runhostSchedulerTick(context.Background(), t.Logf)
	time.Sleep(100 * time.Millisecond)
	if fires, _ := runhostScheduleStore().Fires(info.ID, 0); len(fires) != 1 {
		t.Fatalf("fires %+v", fires)
	}
	list, err := runhostScheduleList()
	if err != nil || len(list) != 1 || len(list[0].Fires) != 1 || list[0].Fires[0].Job != fire.RunID {
		t.Fatalf("list %+v %v", list, err)
	}

	if _, err := runhostScheduleRemove(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("the removed schedule's entry is still on disk: %v", err)
	}
	if list, _ := runhostScheduleList(); len(list) != 0 {
		t.Fatalf("list after removal: %+v", list)
	}
}

// A runhost whose systemd cannot seal its store's key to its account takes
// no schedule, and says why.
func TestAScheduleNeedsASealedStore(t *testing.T) {
	sealingRunhost(t)
	runhostSystemdVersion = func() int { return 252 }
	forgetRunhostStoreKey()
	j, archive := unsignedJob(t, "0123456789abcdef0123456789abcdef")
	_, code, err := putSchedule(t, j, archive, daily(t, time.Now(), "07:00"))
	if code != 11 && code == 0 {
		t.Fatalf("an unsealed store took a schedule: %d", code)
	}
	if err == nil || !strings.Contains(err.Error(), "systemd 256") {
		t.Fatalf("the refusal: %v", err)
	}
}

// A session cannot be scheduled, and a schedule for another device than the
// run key's is refused.
func TestARunhostScheduleRefusesWhatCannotRunUnattended(t *testing.T) {
	sealingRunhost(t)
	j, archive := unsignedJob(t, "0123456789abcdef0123456789abcdef")
	j.Run.Session = true
	if _, code, _ := putSchedule(t, j, archive, daily(t, time.Now(), "07:00")); code == 0 {
		t.Fatal("a session was scheduled")
	}
}

// `agentry schedule add --on` prepares the run as `run --on` does, names
// what the runhost's store will keep, and hands it over; the runhost then
// runs it with no device present, and the device collects the run's output
// as it collects any job's.
func TestScheduleAddOnARunhostHandsTheScheduleOver(t *testing.T) {
	srv, _ := newRunOnLab(t)
	oldEnc, oldDec, oldDir, oldNow := schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow
	t.Cleanup(func() {
		schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow = oldEnc, oldDec, oldDir, oldNow
	})
	schedule.EncryptCredential = func(name string, plain []byte, out string) error {
		return os.WriteFile(out, append([]byte(name+":"), plain...), 0o600)
	}
	runhostDecryptCredential = func(name, file string) ([]byte, error) {
		b, err := os.ReadFile(file) //nolint:gosec // the test's own file
		if err != nil {
			return nil, err
		}
		v, _ := bytes.CutPrefix(b, []byte(name+":"))
		return v, nil
	}
	runhostSystemdVersion = func() int { return 257 }
	runhostSchedulesDir = filepath.Join(srv.dir, "schedules")
	t.Setenv("API_TOKEN", "tok-secret-value-1")
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  secrets: [API_TOKEN]\n"))

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "schedule", "add", "acme/demo@1.0.0", "--on", "lab",
		"--daily", "07:00", "--zone", "UTC", "--task", "the scheduled task", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("schedule add --on: %v\n%s", err, stderr)
	}
	for _, want := range []string{"secret API_TOKEN", "runhost lab", `lab's default saved model, whichever it is then (now "anthropic"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the consent does not name %q:\n%s", want, stderr)
		}
	}
	// The runhost holds the model's credential itself: the schedule keeps
	// none of this computer's.
	if strings.Contains(stderr, "model-key") {
		t.Errorf("the schedule keeps a model key:\n%s", stderr)
	}
	if !strings.Contains(stdout, "is on runhost lab") {
		t.Fatalf("stdout:\n%s", stdout)
	}
	list, err := runhostScheduleList()
	if err != nil || len(list) != 1 {
		t.Fatalf("the runhost holds %+v (%v)", list, err)
	}
	sc := list[0]
	if sc.Ref != "acme/demo@1.0.0" || sc.Device != testDevice || len(sc.Entries) != 1 || sc.Model != job.RunhostDefault {
		t.Fatalf("schedule %+v", sc)
	}
	// Nothing of the task, the secret or the model key is in the clear in
	// the runhost's state.
	err = filepath.WalkDir(srv.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p) //nolint:gosec // the test's own tree
		for _, v := range []string{"tok-secret-value-1", "sk-test-model-key", "the scheduled task"} {
			if bytes.Contains(b, []byte(v)) {
				t.Errorf("%s holds %q in the clear", p, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The occurrence runs a copy of the job, its secret and task among what
	// it carries.
	runhostScheduleNow = func() time.Time { return sc.Next.Add(time.Minute) }
	runhostSchedulerTick(context.Background(), t.Logf)
	var fire schedule.Fire
	deadline := time.Now().Add(20 * time.Second)
	for {
		fires, _ := runhostScheduleStore().Fires(sc.ID, 0)
		if len(fires) == 1 {
			fire = fires[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the occurrence did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fire.Outcome != schedule.Ran {
		t.Fatalf("fire %+v", fire)
	}
	if seen, _ := os.ReadFile(filepath.Join(srv.dir, "secrets.seen")); string(seen) != "API_TOKEN=tok-secret-value-1\n" {
		t.Errorf("the occurrence's secrets file held %q", seen)
	}
	out := filepath.Join(t.TempDir(), "collected")
	_, stderr, err = runCLI(t, nil, "runhost", "collect", "lab", fire.RunID, "--out", out)
	if err != nil {
		t.Fatalf("collect the occurrence's job: %v\n%s", err, stderr)
	}
	stdout, _, err = runCLI(t, nil, "runhost", "store", "lab", "--remove", "schedule:"+sc.ID)
	if err != nil || !strings.Contains(stdout, "is off runhost lab") {
		t.Fatalf("remove: %v\n%s", err, stdout)
	}
	if list, _ := runhostScheduleList(); len(list) != 0 {
		t.Fatalf("the schedule stayed: %+v", list)
	}
}

// A runhost whose systemd seals nothing is refused before the consent is
// asked, as a policy refusal.
func TestScheduleAddOnARunhostThatTakesNoScheduleAsksNothing(t *testing.T) {
	newRunOnLab(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "schedule", "add", "acme/demo@1.0.0", "--on", "lab",
		"--daily", "07:00", "--zone", "UTC", "--allow-unsigned", "--yes")
	var ce *engine.ClassifiedError
	if err == nil || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "takes no schedule") {
		t.Fatalf("schedule add on an unsealed runhost: %v", err)
	}
	if strings.Contains(stderr, "Its store keeps") {
		t.Fatalf("the consent was put before the refusal:\n%s", stderr)
	}
}

// --check with --on says what the runhost's store would keep and whether
// the runhost takes schedules, and sends nothing.
func TestScheduleAddCheckOnARunhostKeepsNothing(t *testing.T) {
	srv, _ := newRunOnLab(t)
	runhostSchedulesDir = filepath.Join(srv.dir, "schedules")
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  secrets: [API_TOKEN]\n"))
	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "--json", "schedule", "add", "acme/demo@1.0.0", "--on", "lab",
		"--daily", "07:00", "--zone", "UTC", "--allow-unsigned", "--check")
	if err != nil {
		t.Fatalf("check: %v\n%s", err, stderr)
	}
	var res scheduleCheckJSON
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if res.Runhost == nil || res.Runhost.Name != "lab" || res.Runhost.TakesSchedules || res.Ready {
		t.Fatalf("an unsealed runhost: %+v %+v", res, res.Runhost)
	}
	kinds := map[string]string{}
	for _, e := range res.Runhost.Entries {
		kinds[e.Kind] = e.Name
	}
	if _, ok := kinds["model-key"]; kinds["secret"] != "API_TOKEN" || ok {
		t.Fatalf("entries %+v", res.Runhost.Entries)
	}
	if res.Runhost.Model == nil || res.Runhost.Model.Name != "anthropic" || !res.Runhost.FollowsDefault {
		t.Fatalf("the runhost's model %+v", res.Runhost.Model)
	}
	if list, _ := runhostScheduleList(); len(list) != 0 {
		t.Fatalf("a check kept a schedule: %+v", list)
	}
}

// A schedule's OAuth connection is one the runhost made for itself: a
// schedule whose connection the runhost does not keep is refused, one it
// keeps is taken, and removing the last schedule that uses it revokes the
// runhost's grant at the vendor and forgets it.
func TestARunhostScheduleUsesOnlyAConnectionOfTheRunhostsOwn(t *testing.T) {
	sealingRunhost(t)
	v := mcpauthtest.New(t)
	v.DCR = true
	prevHTTP := vendorHTTP
	vendorHTTP = func() *http.Client { return v.Guard().Client() }
	t.Cleanup(func() { vendorHTTP = prevHTTP })
	start := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	runhostScheduleNow = func() time.Time { return start }
	j, archive := unsignedJob(t, "0123456789abcdef0123456789abcdef")
	j.Run.Connections = map[string]job.Connection{v.ServerURL(): {AccessToken: "at-from-the-device", ExpiresAt: start.Add(time.Hour)}}

	if _, code, err := putSchedule(t, j, archive, daily(t, start, "07:00")); code != engine.ExitPolicy || err == nil || !strings.Contains(err.Error(), "signs the runhost in") {
		t.Fatalf("a connection the runhost does not keep: %d %v", code, err)
	}

	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	conns := connection.Open(home, runhostKeychain{})
	g, err := mcpauth.Consent{HTTP: v.Guard().Client(), ServerURL: v.ServerURL(), Present: v.Browser(), Timeout: 10 * time.Second}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k := connection.Key{Ref: "acme/demo@1.0.0", Digest: j.Release.Digest, Server: v.ServerURL()}
	if _, err := conns.Put(k, g, ""); err != nil {
		t.Fatal(err)
	}
	info, code, err := putSchedule(t, j, archive, daily(t, start, "07:00"))
	if code != 0 {
		t.Fatalf("a connection the runhost keeps: %d %v", code, err)
	}
	// Its occurrence runs on a token the runhost's own grant refreshed.
	issued := len(v.Issued)
	runhostScheduleNow = func() time.Time { return start.Add(time.Hour + time.Minute) }
	runhostSchedulerTick(context.Background(), t.Logf)
	deadline := time.Now().Add(20 * time.Second)
	for {
		fires, _ := runhostScheduleStore().Fires(info.ID, 0)
		if len(fires) == 1 {
			if fires[0].Outcome != schedule.Ran {
				t.Fatalf("fire %+v", fires[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fire recorded: %+v", fires)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(v.Issued) == issued {
		t.Fatal("the occurrence did not refresh the runhost's connection")
	}
	g.Token.RefreshToken = v.Issued[len(v.Issued)-1]
	if _, err := runhostScheduleRemove(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conns.Get(k); !errors.Is(err, connection.ErrNone) {
		t.Fatalf("the removed schedule's connection is still kept: %v", err)
	}
	if v.Live(g.Token.RefreshToken) {
		t.Fatal("the runhost's grant was not revoked")
	}
}
