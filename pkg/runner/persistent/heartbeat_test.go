package persistent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// enableHeartbeat enables the Assistant with its heartbeat on and its
// roster agreed to.
func enableHeartbeat(t *testing.T, home string, on bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TZ", "UTC")
	c := Config{Enabled: true, Switches: map[string]bool{EntryHeartbeat: on}}
	RecordConsent(BuildRoster(assistantAgent(t), c, nil), &c)
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
}

func startTicking(t *testing.T, home string, st *starter, c *clock) *Supervisor {
	t.Helper()
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start, Now: c.now, Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

func callTask(t *testing.T, sv *Supervisor, tool, args string) applink.Answer {
	t.Helper()
	return sv.answerAppLink(context.Background(), applink.Request{Tool: tool, Arguments: json.RawMessage(args)})
}

// beats reads the segment's heartbeat receipt.
func beats(t *testing.T, sv *Supervisor) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(sv.Status().SegmentDir, "receipts", heartbeatLog))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		out = append(out, m)
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEveryTurnIsToldTheTime: the Assistant's turns open with the time; the
// built-ins that do not keep time are told none.
func TestEveryTurnIsToldTheTime(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	sent := st.lastSegment().rawTurns()
	if len(sent) != 1 || sent[0] != "(The time now: Wednesday 21 October 2026, 14:03, UTC.)\n\n"+memoryFrame+"hello" {
		t.Errorf("the turn sent: %q", sent)
	}
	for _, a := range Agents() {
		if keeps := a.Builtin.KeepsTime(); keeps != (a.Name == "assistant") {
			t.Errorf("%s keeps time: %v", a.Name, keeps)
		}
		if _, ok := BuildRoster(a, Config{}, nil).Entry(EntryHeartbeat); ok != (a.Name == "assistant") {
			t.Errorf("%s has a heartbeat entry: %v", a.Name, ok)
		}
	}
}

// TestATaskFiresATurnAtItsTime: a task the agent registers fires a turn of
// its own when its time comes, recorded in the receipt, the transcript and
// the task itself.
func TestATaskFiresATurnAtItsTime(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	a := callTask(t, sv, applink.TaskAdd, `{"name":"Stretch","cron":"*/15 * * * *","instruction":"Remind the person to stand up."}`)
	if a.IsError || !strings.Contains(a.Text, "t1") || !strings.Contains(a.Text, "next at Wed 21 Oct 2026 14:15") {
		t.Fatalf("task_add: %+v", a)
	}
	events, unsubscribe := sv.Subscribe()
	defer unsubscribe()
	c.set(time.Date(2026, 10, 21, 14, 15, 10, 0, time.UTC))
	seg := st.lastSegment()
	eventually(t, "the task's turn", func() bool { return len(seg.sentTurns()) == 1 })
	// A chat watching is handed the turn marked as the task's.
	select {
	case ev := <-events:
		if ev.Kind != session.KindUserTurn || ev.Task != "t1" {
			t.Errorf("the watcher's first event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the watcher")
	}
	turn := seg.rawTurns()[0]
	for _, want := range []string{"(The time now:", `your task t1, "Stretch", came due at 14:15`, "Task: Stretch\n\nRemind the person to stand up."} {
		if !strings.Contains(turn, want) {
			t.Errorf("the task's turn lacks %q: %q", want, turn)
		}
	}
	eventually(t, "the result kept", func() bool {
		ts, _ := sv.Tasks()
		return len(ts) == 1 && ts[0].LastResult == "echo: Task: Stretch"
	})
	ts, _ := sv.Tasks()
	if !ts[0].Next.Equal(time.Date(2026, 10, 21, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("next: %s", ts[0].Next)
	}
	eventually(t, "the receipt", func() bool { return len(beats(t, sv)) == 1 })
	if b := beats(t, sv)[0]; b["outcome"] != beatFired || b["task"] != "t1" {
		t.Errorf("the receipt: %v", b)
	}
	entries, _ := Transcript(home, "assistant", 50)
	marked := false
	for _, e := range entries {
		marked = marked || e.Task == "t1"
	}
	if !marked {
		t.Error("the transcript does not mark the task's turn")
	}
	history := HistoryEvents(entries)
	if len(history) == 0 || history[0].Kind != session.KindUserTurn || history[0].Task != "t1" {
		t.Errorf("the history does not mark the task's turn: %+v", history)
	}
	// The next tick in the same minute fires nothing again.
	time.Sleep(50 * time.Millisecond)
	if n := len(seg.sentTurns()); n != 1 {
		t.Errorf("%d turns", n)
	}
	// The person's own turn after it carries no mark.
	if err := sv.Send(context.Background(), "thanks"); err != nil {
		t.Fatal(err)
	}
	entries, _ = Transcript(home, "assistant", 50)
	for _, ev := range HistoryEvents(entries) {
		if ev.Kind == session.KindUserTurn && strings.Contains(ev.Text, "thanks") && ev.Task != "" {
			t.Errorf("the person's turn is marked as task %q", ev.Task)
		}
	}
}

// TestADueTaskWaitsForTheTurnInFlight: a task that comes due while the
// person's turn runs is sent once it is answered, and the person's next turn
// meanwhile is answered busy.
func TestADueTaskWaitsForTheTurnInFlight(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	hold := make(chan struct{})
	st := &starter{hold: hold}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Check","cron":"15 14 * * *","instruction":"Check in."}`); a.IsError {
		t.Fatal(a.Text)
	}
	done := make(chan error, 1)
	go func() { done <- sv.Send(context.Background(), "a long question") }()
	seg := st.lastSegment()
	eventually(t, "the person's turn in flight", func() bool { return len(seg.sentTurns()) == 1 })
	c.set(time.Date(2026, 10, 21, 14, 15, 30, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if n := len(seg.sentTurns()); n != 1 {
		t.Fatalf("the task did not wait: %d turns", n)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task's turn after it", func() bool { return len(seg.sentTurns()) == 2 })
	if !strings.Contains(seg.sentTurns()[1], "Task: Check") {
		t.Errorf("the second turn: %q", seg.sentTurns()[1])
	}
}

// TestTasksDueWhileStoppedAreNamedNotRun: a start names to the agent, at its
// first turn, the tasks that came due while it was stopped, and runs none of
// them.
func TestTasksDueWhileStoppedAreNamedNotRun(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	ts := Tasks{Version: tasksVersion, Checked: time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC), NextID: 1,
		Tasks: []Task{{ID: "t1", Name: "Morning brief", Cron: "0 8 * * *", Zone: "UTC", Instruction: "Summarise the share.", State: TaskOn, MadeBy: TaskMadeByAgent}}}
	if err := saveTasks(home, "assistant", ts); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	time.Sleep(50 * time.Millisecond)
	seg := st.lastSegment()
	if n := len(seg.sentTurns()); n != 0 {
		t.Fatalf("a missed task ran: %q", seg.sentTurns())
	}
	if b := beats(t, sv); len(b) != 1 || b[0]["outcome"] != beatMissed {
		t.Errorf("the receipt: %v", b)
	}
	if err := sv.Send(context.Background(), "morning"); err != nil {
		t.Fatal(err)
	}
	if turn := seg.sentTurns()[0]; !strings.Contains(turn, `came due while you were not running, and did not run: "Morning brief" (due Wed 21 Oct 08:00)`) {
		t.Errorf("the first turn: %q", turn)
	}
	if err := sv.Send(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seg.sentTurns()[1], "did not run") {
		t.Error("the missed tasks were named twice")
	}
}

// TestAHeartbeatSwitchedOffFiresNothing: with the heartbeat off, no task
// fires, and the tools say why; switching it restarts no segment.
func TestAHeartbeatSwitchedOffFiresNothing(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Check","cron":"15 14 * * *","instruction":"Check in."}`); a.IsError {
		t.Fatal(a.Text)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryHeartbeat, On: false}}); err != nil {
		t.Fatal(err)
	}
	if e, _ := sv.Roster().Entry(EntryHeartbeat); e.State != StateOff {
		t.Errorf("the switched entry is %s", e.State)
	}
	c.set(time.Date(2026, 10, 21, 14, 16, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if n := len(st.lastSegment().sentTurns()); n != 0 {
		t.Errorf("%d turns fired", n)
	}
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"x","cron":"0 9 * * *","instruction":"y"}`); !a.IsError || !strings.Contains(a.Text, "heartbeat is off") {
		t.Errorf("task_add with the heartbeat off: %+v", a)
	}
	if err := sv.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	segments := len(st.segments)
	st.mu.Unlock()
	if segments != 1 {
		t.Errorf("switching the heartbeat started %d segments", segments)
	}
}

// TestATaskTurnHasTheRosterOfItsTime: a change pending when a task fires is
// applied first, so the task's turn reaches the segment the roster says.
func TestATaskTurnHasTheRosterOfItsTime(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Check","cron":"15 14 * * *","instruction":"Check in."}`); a.IsError {
		t.Fatal(a.Text)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryShare, On: false}}); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	eventually(t, "the task's turn", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.segments) == 2 && len(st.last.sentTurns()) == 1
	})
	st.mu.Lock()
	last := st.segments[1]
	st.mu.Unlock()
	if last.Grants.Share != "" {
		t.Errorf("the task's segment has the share: %q", last.Grants.Share)
	}
}

// TestTheTaskLimits: how often, how many and how much.
func TestTheTaskLimits(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	t.Setenv("TZ", "UTC")
	s := func(v string) *string { return &v }
	var ts Tasks
	if _, err := ts.AddTask(TaskSpec{Name: s("x"), Cron: s("*/10 * * * *"), Instruction: s("y")}, now); err == nil {
		t.Error("a task every 10 minutes was taken")
	}
	if _, err := ts.AddTask(TaskSpec{Name: s("x"), Cron: s("*/15 * * * *"), Instruction: s("y")}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AddTask(TaskSpec{Name: s("x"), Cron: s("7 * * * *"), Instruction: s("y")}, now); err == nil {
		t.Error("a 97th fire a day was taken")
	}
	ts = Tasks{}
	for i := range MaxTasks {
		if _, err := ts.AddTask(TaskSpec{Name: s("x"), Cron: s("0 9 * * *"), Instruction: s("y")}, now); err != nil {
			t.Fatalf("task %d: %v", i+1, err)
		}
	}
	if _, err := ts.AddTask(TaskSpec{Name: s("x"), Cron: s("0 9 * * *"), Instruction: s("y")}, now); err == nil {
		t.Error("a 26th task was taken")
	}
	ts = Tasks{}
	for _, bad := range []TaskSpec{
		{Name: s("x"), Cron: s("0 9 * * *"), Instruction: s(strings.Repeat("a", MaxInstruction+1))},
		{Name: s("a\u202eb"), Cron: s("0 9 * * *"), Instruction: s("y")},
		{Name: s("x"), Cron: s("0 9 * * *"), Instruction: s("y"), Zone: s("Mars/Olympus")},
		{Name: s("x"), Cron: s("0 0 31 2 *"), Instruction: s("y")},
		{Name: s(""), Cron: s("0 9 * * *"), Instruction: s("y")},
	} {
		if _, err := ts.AddTask(bad, now); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
}

// TestThePersonPausesAndRemovesTasks: over the owner socket while the agent
// runs, and under its lock while it does not.
func TestThePersonPausesAndRemovesTasks(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Check","cron":"15 14 * * *","instruction":"Check in."}`); a.IsError {
		t.Fatal(a.Text)
	}
	ctx := context.Background()
	a := assistantAgent(t)
	ts, running, err := ChangeTaskOf(ctx, home, a, ClientCLI, TaskOp{ID: "t1", Op: TaskOpPause})
	if err != nil || !running || len(ts) != 1 || ts[0].State != TaskPaused || !ts[0].Next.IsZero() {
		t.Fatalf("pause through the socket: %+v %v %v", ts, running, err)
	}
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if n := len(st.lastSegment().sentTurns()); n != 0 {
		t.Errorf("a paused task fired %d times", n)
	}
	if _, err := sv.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, running, err = ChangeTaskOf(ctx, home, a, ClientCLI, TaskOp{ID: "t1", Op: TaskOpRemove}); err != nil || running {
		t.Fatalf("remove while stopped: %v %v", running, err)
	}
	if ts, _, _ := TasksOf(ctx, home, a); len(ts) != 0 {
		t.Errorf("left: %+v", ts)
	}
	if _, _, err := ChangeTaskOf(ctx, home, a, ClientCLI, TaskOp{ID: "t9", Op: TaskOpRemove}); err == nil {
		t.Error("removing an unknown task was taken")
	}
}

// TestTaskToolsRefuseWhatTheyDoNotTake: a task tool's call with an argument
// it does not take changes nothing.
func TestTaskToolsRefuseWhatTheyDoNotTake(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	sv := startTicking(t, home, &starter{}, c)
	for _, args := range []string{
		`{"name":"x","cron":"0 9 * * *","instruction":"y","switches":[{"id":"share","on":true}]}`,
		`{"name":"x","cron":"0 9 * * *","instruction":"y","task":"t1"}`,
	} {
		if a := callTask(t, sv, applink.TaskAdd, args); !a.IsError {
			t.Errorf("task_add %s: %+v", args, a)
		}
	}
	if ts, _ := sv.Tasks(); len(ts) != 0 {
		t.Errorf("saved: %+v", ts)
	}
}

// TestAHeartbeatNotAgreedToFiresNothing: a heartbeat switched on in
// config.yaml without the person's agreement fires nothing, though no segment
// restart would catch it.
func TestAHeartbeatNotAgreedToFiresNothing(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if err := saveTasks(home, "assistant", Tasks{Version: tasksVersion, NextID: 1,
		Tasks: []Task{{ID: "t1", Name: "Check", Cron: "15 14 * * *", Zone: "UTC", Instruction: "Check in.", State: TaskOn, MadeBy: TaskMadeByAgent}}}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(home, "assistant")
	cfg.Switches[EntryHeartbeat] = true
	if err := SaveConfig(home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if n := len(st.lastSegment().sentTurns()); n != 0 {
		t.Errorf("a heartbeat nobody agreed to fired %d turns", n)
	}
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"x","cron":"0 9 * * *","instruction":"y"}`); !a.IsError {
		t.Errorf("task_add with a heartbeat nobody agreed to: %+v", a)
	}
}

// TestATaskStillRunningSkipsItsNextTime: a time of a task that comes while
// its own turn before still runs is skipped and recorded, and the task fires
// again at its time after.
func TestATaskStillRunningSkipsItsNextTime(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	hold := make(chan struct{})
	st := &starter{hold: hold}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Sweep","cron":"*/15 * * * *","instruction":"Sweep the share."}`); a.IsError {
		t.Fatal(a.Text)
	}
	seg := st.lastSegment()
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	eventually(t, "the first turn in flight", func() bool { return len(seg.sentTurns()) == 1 })
	// Its turn runs past 14:30.
	c.set(time.Date(2026, 10, 21, 14, 31, 0, 0, time.UTC))
	time.Sleep(30 * time.Millisecond)
	close(hold)
	eventually(t, "the skip recorded", func() bool {
		for _, b := range beats(t, sv) {
			if b["outcome"] == beatSkipped && strings.HasPrefix(b["due"].(string), "2026-10-21T14:30") {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	if n := len(seg.sentTurns()); n != 1 {
		t.Fatalf("the overrun time fired: %d turns", n)
	}
	c.set(time.Date(2026, 10, 21, 14, 45, 0, 0, time.UTC))
	eventually(t, "the next time fires", func() bool { return len(seg.sentTurns()) == 2 })
}

// TestOneHeartbeatPerAgent: a second supervisor of the agent is refused, so
// a task fires once whoever else asks to run it.
func TestOneHeartbeatPerAgent(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	if _, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: (&starter{}).start, Now: c.now, Tick: 5 * time.Millisecond,
	}); err == nil {
		t.Fatal("a second supervisor started")
	}
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Once","cron":"15 14 * * *","instruction":"Once."}`); a.IsError {
		t.Fatal(a.Text)
	}
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	seg := st.lastSegment()
	eventually(t, "the fire", func() bool { return len(seg.sentTurns()) == 1 })
	time.Sleep(50 * time.Millisecond)
	fired := 0
	for _, b := range beats(t, sv) {
		if b["outcome"] == beatFired {
			fired++
		}
	}
	if fired != 1 || len(seg.sentTurns()) != 1 {
		t.Errorf("fired %d times, %d turns", fired, len(seg.sentTurns()))
	}
}

// TestAFailedTaskTurnIsRecordedAsFailed: a task's turn the harness fails is
// a failed line, and the task's result says so.
func TestAFailedTaskTurnIsRecordedAsFailed(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{failSends: 1}
	sv := startTicking(t, home, st, c)
	if a := callTask(t, sv, applink.TaskAdd, `{"name":"Check","cron":"15 14 * * *","instruction":"Check in."}`); a.IsError {
		t.Fatal(a.Text)
	}
	c.set(time.Date(2026, 10, 21, 14, 15, 0, 0, time.UTC))
	eventually(t, "the failed line", func() bool {
		b := beats(t, sv)
		return len(b) == 1 && b[0]["outcome"] == beatFailed
	})
	ts, _ := sv.Tasks()
	if !strings.HasPrefix(ts[0].LastResult, "the turn failed: ") {
		t.Errorf("the result: %q", ts[0].LastResult)
	}
}
