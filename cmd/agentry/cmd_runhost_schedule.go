package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// A runhost's schedules are the service's own: a device sends one with the
// job every occurrence runs a copy of, which the store keeps encrypted with
// everything the job carries (its secrets, the model key, its destinations'
// credentials, its target hosts' sign-ins, its input sources' tokens), after
// a consent on the device naming each of them and the server. The release's
// OAuth connections are handed to the runhost, which refreshes them itself,
// as it refreshes its model sign-ins. Each occurrence verifies the release
// from the store again and runs as a device's job does, its output sealed to
// every paired device, waiting for the next collection. A schedule needs the
// store's key sealed by systemd (runhostStoreSeals).

var runhostSchedulesDir = filepath.Join(runhost.StateDir, "schedules")

func runhostScheduleStore() schedule.Store { return schedule.Store{Dir: runhostSchedulesDir} }

// runhostScheduleMetaFile is what the service keeps of a schedule beside
// the schedule's own record, no secret in it.
const runhostScheduleMetaFile = "runhost.json"

type runhostScheduleMeta struct {
	Device      string                  `json:"device"`
	Entries     []runhost.ScheduleEntry `json:"entries"`
	Connections []connection.Key        `json:"connections,omitempty"`
}

// runhostScheduleTemplate is a schedule's entry in the store: the job each
// occurrence runs a copy of, and its files.
type runhostScheduleTemplate struct {
	Job   job.Job `json:"job"`
	Files []byte  `json:"files,omitempty"`
}

func scheduleEntryName(id string) string { return "sch-" + id }

// runhostScheduleMaxFiles bounds the files a schedule keeps: its entry is
// read whole at each occurrence.
const runhostScheduleMaxFiles = 256 << 20

// runhostScheduleNow is the scheduler's clock; tests replace it.
var runhostScheduleNow = time.Now

// runhostFiring holds the schedules with an occurrence running, so a slow
// run is never started twice.
var runhostFiring = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func runhostScheduleVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	switch {
	case len(req.Args) == 1 && req.Args[0] == "put":
		return runhostSchedulePut(ctx, req, in, fw)
	case len(req.Args) == 1 && req.Args[0] == "list":
		list, err := runhostScheduleList()
		if err != nil {
			return engine.ExitCLI, err
		}
		return 0, fw.Result(list)
	case len(req.Args) == 2 && req.Args[0] == "remove":
		info, err := runhostScheduleRemove(ctx, req.Args[1])
		if err != nil {
			return engine.ExitUsage, err
		}
		return 0, fw.Result(info)
	case len(req.Args) == 2 && req.Args[0] == "run":
		fire, err := runhostScheduleFire(ctx, req.Args[1], runhostScheduleNow(), true)
		if err != nil {
			return engine.ExitCLI, err
		}
		return 0, fw.Result(fire)
	}
	return engine.ExitUsage, errors.New("schedule takes put, list, remove <id> or run <id>")
}

func runhostSchedulePut(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if !runhostStoreSeals() {
		return engine.ExitPolicy, fmt.Errorf("this runhost's systemd %d cannot seal its store's key to its account, and a schedule keeps its secrets there: schedules need systemd %d or newer", runhostSystemdVersion(), schedule.MinSystemd)
	}
	if _, sealed, err := runhostStoreKeyBytes(); err != nil || !sealed {
		return engine.ExitPolicy, fmt.Errorf("the runhost's store key is not sealed (%v): a schedule keeps its secrets there", err)
	}
	br := bufio.NewReaderSize(in, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return engine.ExitUsage, fmt.Errorf("the schedule's header: %w", err)
	}
	var h runhost.ScheduleHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return engine.ExitUsage, fmt.Errorf("the schedule's header: %w", err)
	}
	var when schedule.Recurrence
	if err := json.Unmarshal(h.When, &when); err != nil {
		return engine.ExitUsage, fmt.Errorf("the schedule's recurrence: %w", err)
	}
	if err := when.Validate(); err != nil {
		return engine.ExitUsage, err
	}
	now := runhostScheduleNow()
	if _, ok := when.Next(now); !ok {
		return engine.ExitUsage, errors.New("the schedule has no occurrence left")
	}
	j, err := job.Read(br)
	if err != nil {
		return engine.ExitUsage, err
	}
	if j.Run.Session {
		return engine.ExitUsage, errors.New("a schedule runs autonomous: nobody is there to answer a session")
	}
	if !runhostDevicePaired(j.Device) {
		return engine.ExitUsage, fmt.Errorf("device %s is not paired with this runhost", j.Device)
	}
	if req.Device != "" && j.Device != req.Device {
		return engine.ExitUsage, fmt.Errorf("the schedule names device %s, but device %s's run key sent it", j.Device, req.Device)
	}
	if j.Run.Model.PaysWithCredits() {
		if _, err := runhostJobCredits(j.Run.Model.Policy()); err != nil {
			return engine.ExitPolicy, err
		}
	}
	if j.Run.Model.SignIn() {
		if err := runhostJobSignIn(j.Run.Model.APIKeyKind); err != nil {
			return engine.ExitPolicy, err
		}
	}
	st, err := openRunhostStore()
	if err != nil {
		return engine.ExitCLI, fmt.Errorf("the runhost's store: %w", err)
	}
	archive, err := runhostTakeRelease(st, j, br, fw)
	if err != nil {
		return engine.ExitVerify, err
	}
	meta, err := runhostVerifyRelease(j.Release, archive)
	if err != nil {
		return engine.ExitVerify, err
	}
	if !st.Has(j.Release.Digest) {
		if err := st.Put(j.Release, archive); err != nil {
			return engine.ExitCLI, fmt.Errorf("keep the release in the runhost's store: %w", err)
		}
	}
	tmpl := runhostScheduleTemplate{Job: *j}
	if j.Run.Payload != nil {
		if j.Run.Payload.Size > runhostScheduleMaxFiles {
			return engine.ExitUsage, fmt.Errorf("the schedule's files are %d bytes, more than the %d a runhost keeps for a schedule", j.Run.Payload.Size, runhostScheduleMaxFiles)
		}
		tmpl.Files = make([]byte, j.Run.Payload.Size)
		if _, err := io.ReadFull(br, tmpl.Files); err != nil {
			return engine.ExitUsage, fmt.Errorf("the schedule's files: %w", err)
		}
	}
	home, err := runnerHome()
	if err != nil {
		return engine.ExitCLI, err
	}
	conns := connection.Open(home, runhostKeychain{})
	sm := runhostScheduleMeta{Device: j.Device, Entries: h.Entries}
	// Each connection the job uses is one the runhost made for itself
	// (signin.Server.Connect) before the schedule was sent.
	for server := range j.Run.Connections {
		k := connection.Key{Ref: meta.Ref, Digest: j.Release.Digest, Server: server}
		if c, err := conns.Get(k); err != nil || c.Broken != "" {
			return engine.ExitPolicy, fmt.Errorf("the runhost keeps no working connection to %s for %s: make the schedule again from a device, which signs the runhost in", server, meta.Ref)
		}
		sm.Connections = append(sm.Connections, k)
	}
	sc := schedule.Schedule{Name: h.Name, Ref: meta.Namespace + "/" + meta.Name, Version: meta.Version, Digest: j.Release.Digest, When: when, Created: now.UTC()}
	ss := runhostScheduleStore()
	if err := ss.Create(&sc, nil); err != nil {
		return engine.ExitCLI, err
	}
	b, err := json.Marshal(tmpl)
	if err != nil {
		return engine.ExitCLI, err
	}
	if err := st.PutEntry(scheduleEntryName(sc.ID), b); err != nil {
		_ = ss.Remove(sc.ID)
		return engine.ExitCLI, fmt.Errorf("keep the schedule in the runhost's store: %w", err)
	}
	if err := writeScheduleMeta(sc.ID, sm); err != nil {
		_ = st.RemoveEntry(scheduleEntryName(sc.ID))
		_ = ss.Remove(sc.ID)
		return engine.ExitCLI, err
	}
	_ = fw.Out("schedule %s (%s) is kept on this runhost, its secrets in its store, encrypted", sc.ID, sc.Name)
	info, err := runhostScheduleInfo(ss, sc)
	if err != nil {
		return engine.ExitCLI, err
	}
	return 0, fw.Result(info)
}

func scheduleMetaPath(id string) (string, error) {
	if !schedule.ValidID(id) {
		return "", fmt.Errorf("%q is not a schedule id", id)
	}
	return filepath.Join(runhostSchedulesDir, id, runhostScheduleMetaFile), nil
}

func writeScheduleMeta(id string, sm runhostScheduleMeta) error {
	p, err := scheduleMetaPath(id)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		return err
	}
	return writeRunhostFile(p, b)
}

func readScheduleMeta(id string) (runhostScheduleMeta, error) {
	var sm runhostScheduleMeta
	p, err := scheduleMetaPath(id)
	if err != nil {
		return sm, err
	}
	return sm, readJSONFile(p, &sm)
}

func runhostScheduleInfo(ss schedule.Store, sc schedule.Schedule) (runhost.ScheduleInfo, error) {
	sm, err := readScheduleMeta(sc.ID)
	if err != nil {
		return runhost.ScheduleInfo{}, err
	}
	when, err := json.Marshal(sc.When)
	if err != nil {
		return runhost.ScheduleInfo{}, err
	}
	info := runhost.ScheduleInfo{ID: sc.ID, Name: sc.Name, Ref: sc.Ref + "@" + sc.Version, Device: sm.Device, When: when,
		Paused: sc.Paused, Created: sc.Created, Entries: sm.Entries}
	st, _ := ss.State(sc.ID)
	info.Ended = st.Ended
	after := runhostScheduleNow()
	if st.Last.After(after) {
		after = st.Last
	}
	if next, ok := sc.When.Next(after); ok && !st.Ended && sc.When.Remaining(st.Runs) != 0 {
		info.Next = &next
	}
	fires, _ := ss.Fires(sc.ID, 10)
	for i := len(fires) - 1; i >= 0; i-- {
		f := fires[i]
		info.Fires = append(info.Fires, runhost.ScheduleFire{Occurrence: f.Occurrence, Outcome: string(f.Outcome), Job: f.RunID, ExitCode: f.ExitCode, Message: f.Message})
	}
	return info, nil
}

func runhostScheduleList() ([]runhost.ScheduleInfo, error) {
	ss := runhostScheduleStore()
	list, _, err := ss.List()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := []runhost.ScheduleInfo{}
	for _, sc := range list {
		info, err := runhostScheduleInfo(ss, sc)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// runhostScheduleRemove takes a schedule off the runhost: its entry in the
// store and every connection no other schedule uses, revoked at the vendor
// where it offers that, the grant being the runhost's own.
func runhostScheduleRemove(ctx context.Context, id string) (runhost.ScheduleInfo, error) {
	ss := runhostScheduleStore()
	sc, err := ss.Get(id)
	if err != nil {
		return runhost.ScheduleInfo{}, err
	}
	info, _ := runhostScheduleInfo(ss, sc)
	sm, _ := readScheduleMeta(id)
	st, err := openRunhostStore()
	if err != nil {
		return info, err
	}
	if err := st.RemoveEntry(scheduleEntryName(id)); err != nil {
		return info, err
	}
	if len(sm.Connections) > 0 {
		inUse := map[connection.Key]bool{}
		others, _, _ := ss.List()
		for _, o := range others {
			if o.ID == id {
				continue
			}
			if om, err := readScheduleMeta(o.ID); err == nil {
				for _, k := range om.Connections {
					inUse[k] = true
				}
			}
		}
		if home, err := runnerHome(); err == nil {
			conns := connection.Open(home, runhostKeychain{})
			clients := connection.OpenClients(home, runhostKeychain{})
			for _, k := range sm.Connections {
				if !inUse[k] {
					_, _ = conns.Remove(ctx, k, vendorHTTP(), clients, "its last schedule on the runhost was removed")
				}
			}
		}
	}
	return info, ss.Remove(id)
}

// runhostScheduler fires the runhost's schedules while the service runs:
// at its start and every interval, each occurrence that has fallen due runs
// once; occurrences passed while the service was not running are recorded
// as missed, as a computer's are.
func runhostScheduler(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		runhostSchedulerTick(ctx, logf)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func runhostSchedulerTick(ctx context.Context, logf func(string, ...any)) {
	if runhostRestarting() != "" || !runhostStoreSeals() {
		return
	}
	ss := runhostScheduleStore()
	list, _, err := ss.List()
	if err != nil {
		return
	}
	now := runhostScheduleNow()
	for _, sc := range list {
		if sc.Paused {
			continue
		}
		st, err := ss.State(sc.ID)
		if err != nil || st.Ended {
			continue
		}
		run, missed := schedule.Due(sc.When, st, now)
		for _, m := range missed {
			if ok, err := ss.Claim(sc.ID, m); err != nil || !ok {
				continue
			}
			_ = ss.AppendFire(sc.ID, schedule.Fire{Occurrence: m, Outcome: schedule.Missed,
				Message: "the runhost's service was not running when it fell due, and a later occurrence fell due first"})
			st.Last = m
		}
		if len(missed) > 0 {
			_ = ss.SaveState(sc.ID, st)
		}
		if run.IsZero() {
			continue
		}
		if _, err := runhostScheduleFire(ctx, sc.ID, run, false); err != nil {
			logf("schedule %s: %v", sc.ID, err)
		}
	}
}

// runhostScheduleFire runs one occurrence of schedule id: a copy of its job
// with an id of its own, its connections refreshed here. The fire is
// recorded when the run ends; what it returns is the fire as it started.
func runhostScheduleFire(ctx context.Context, id string, occurrence time.Time, manual bool) (runhost.ScheduleFire, error) {
	ss := runhostScheduleStore()
	sc, err := ss.Get(id)
	if err != nil {
		return runhost.ScheduleFire{}, err
	}
	runhostFiring.Lock()
	if runhostFiring.m[id] {
		runhostFiring.Unlock()
		return runhost.ScheduleFire{}, fmt.Errorf("schedule %s is running an occurrence already", id)
	}
	runhostFiring.m[id] = true
	runhostFiring.Unlock()
	release := func() {
		runhostFiring.Lock()
		delete(runhostFiring.m, id)
		runhostFiring.Unlock()
	}
	st, err := ss.State(id)
	if err != nil {
		release()
		return runhost.ScheduleFire{}, err
	}
	if !manual {
		won, err := ss.Claim(id, occurrence)
		if err != nil || !won {
			release()
			return runhost.ScheduleFire{}, err
		}
		st.Last = occurrence
		st.Runs++
		if err := ss.SaveState(id, st); err != nil {
			release()
			return runhost.ScheduleFire{}, err
		}
		ss.Prune(id, occurrence.AddDate(0, 0, -30))
	}
	fire := schedule.Fire{Occurrence: occurrence, Manual: manual, Started: runhostScheduleNow()}
	failed := func(code int, err error) (runhost.ScheduleFire, error) {
		fire.Outcome, fire.ExitCode, fire.Message, fire.Ended = schedule.Failed, code, err.Error(), runhostScheduleNow()
		_ = ss.AppendFire(id, fire)
		endRunhostScheduleIfDone(ss, sc, occurrence)
		release()
		return runhost.ScheduleFire{Occurrence: occurrence, Outcome: string(fire.Outcome), ExitCode: code, Message: fire.Message}, nil
	}
	j, files, code, err := runhostScheduledJob(ctx, sc)
	if err != nil {
		return failed(code, err)
	}
	devices, err := pairedSealKeys()
	if err != nil {
		return failed(engine.ExitCLI, err)
	}
	st2, err := openRunhostStore()
	if err != nil {
		return failed(engine.ExitCLI, err)
	}
	_, archive, err := st2.Get(j.Release.Digest)
	if err != nil {
		return failed(engine.ExitVerify, fmt.Errorf("the schedule's release in the store: %w", err))
	}
	meta, err := runhostVerifyRelease(j.Release, archive)
	if err != nil {
		return failed(engine.ExitVerify, err)
	}
	fire.RunID = j.ID
	_, code, err = beginRunhostJob(context.WithoutCancel(ctx), j, meta, archive, bytes.NewReader(files), devices, func(code int) {
		fire.Ended, fire.ExitCode, fire.Outcome = runhostScheduleNow(), code, schedule.Ran
		if code != 0 {
			fire.Outcome = schedule.Failed
		}
		_ = ss.AppendFire(id, fire)
		endRunhostScheduleIfDone(ss, sc, occurrence)
		release()
	})
	if err != nil {
		fire.RunID = ""
		return failed(code, err)
	}
	return runhost.ScheduleFire{Occurrence: occurrence, Outcome: "running", Job: j.ID}, nil
}

// runhostScheduledJob is the job one occurrence of sc runs: the schedule's
// template under a new id, each OAuth connection refreshed for it.
func runhostScheduledJob(ctx context.Context, sc schedule.Schedule) (*job.Job, []byte, int, error) {
	st, err := openRunhostStore()
	if err != nil {
		return nil, nil, engine.ExitCLI, err
	}
	b, err := st.Entry(scheduleEntryName(sc.ID))
	if err != nil {
		return nil, nil, engine.ExitCLI, fmt.Errorf("the schedule's entry in the store: %w", err)
	}
	var tmpl runhostScheduleTemplate
	if err := json.Unmarshal(b, &tmpl); err != nil {
		return nil, nil, engine.ExitCLI, fmt.Errorf("the schedule's entry in the store: %w", err)
	}
	j := tmpl.Job
	id, err := remote.NewJobID()
	if err != nil {
		return nil, nil, engine.ExitCLI, err
	}
	j.ID = id
	if len(j.Run.Connections) > 0 {
		home, err := runnerHome()
		if err != nil {
			return nil, nil, engine.ExitCLI, err
		}
		sm, err := readScheduleMeta(sc.ID)
		if err != nil {
			return nil, nil, engine.ExitCLI, err
		}
		conns := connection.Open(home, runhostKeychain{})
		clients := connection.OpenClients(home, runhostKeychain{})
		fresh := map[string]job.Connection{}
		for server, c := range j.Run.Connections {
			i := slices.IndexFunc(sm.Connections, func(k connection.Key) bool { return k.Server == server })
			if i < 0 {
				return nil, nil, engine.ExitPolicy, fmt.Errorf("the runhost keeps no connection to %s: make the schedule again from a device", server)
			}
			a, _, err := conns.Use(ctx, sm.Connections[i], vendorHTTP(), clients)
			if err != nil {
				var broken *connection.BrokenError
				if errors.As(err, &broken) {
					return nil, nil, engine.ExitPolicy, fmt.Errorf("the runhost's connection to %s no longer works (%s): make the schedule again from a device, which signs the runhost in again", server, broken.Reason)
				}
				return nil, nil, engine.ExitGateway, fmt.Errorf("refresh the connection to %s: %w", server, err)
			}
			c.AccessToken, c.ExpiresAt = a.Token, a.ExpiresAt
			fresh[server] = c
		}
		j.Run.Connections = fresh
	}
	if err := j.Validate(); err != nil {
		return nil, nil, engine.ExitUsage, err
	}
	return &j, tmpl.Files, 0, nil
}

func endRunhostScheduleIfDone(ss schedule.Store, sc schedule.Schedule, t time.Time) {
	st, err := ss.State(sc.ID)
	if err != nil {
		return
	}
	if _, ok := sc.When.Next(t); ok && sc.When.Remaining(st.Runs) != 0 {
		return
	}
	st.Ended = true
	_ = ss.SaveState(sc.ID, st)
}

// scheduleEntriesSaid is a schedule's entries as a person reads them.
func scheduleEntriesSaid(es []runhost.ScheduleEntry) string {
	var b []string
	for _, e := range es {
		b = append(b, e.Kind+" "+e.Name)
	}
	if len(b) == 0 {
		return "nothing but its release"
	}
	return strings.Join(b, ", ")
}
