package persistent

// The heartbeat of an agent that keeps time: a ticker in its supervisor,
// once a minute while the agent runs, that fires a turn for each of the
// agent's tasks whose time came since it last looked. It is the agent's own:
// nothing outside it schedules anything, and it fires nothing while the agent
// is stopped. A task that came due while the agent was stopped is named to
// it at its next turn and not run; a task due while a turn is in flight waits
// for it; a task whose time came again before its turn could be sent skips
// that time. Each fire, skip and miss is a line of the segment's
// receipts/heartbeat.ndjson.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/schedule"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// EntryHeartbeat is the roster's switch for an agent's heartbeat.
const EntryHeartbeat = "heartbeat"

// KindTasks is the kind of the heartbeat's entry.
const KindTasks EntryKind = "tasks"

// heartbeatLine is the heartbeat's consent sentence.
const heartbeatLine = "Runs turns on its own at the times in its tasks, using your model plan: it registers the tasks itself, and you can pause or remove each one"

// heartbeatLog is the segment's record of the heartbeat.
const heartbeatLog = "heartbeat.ndjson"

// The outcomes a heartbeat line records.
const (
	beatFired   = "fired"
	beatSkipped = "skipped"
	beatMissed  = "missed"
)

// defaultTick is how often the heartbeat looks.
const defaultTick = time.Minute

// maxMissedNamed is how many missed tasks a turn names.
const maxMissedNamed = 10

// now is the supervisor's clock.
func (s *Supervisor) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// heartbeatOn says the agent's roster has its heartbeat on.
func heartbeatOn(a Agent, c Config) bool {
	if !a.Builtin.KeepsTime() {
		return false
	}
	e, ok := BuildRoster(a, c, nil).Entry(EntryHeartbeat)
	return ok && e.On
}

// startHeartbeat names to the agent the tasks that came due while it was
// stopped, then ticks until Stop.
func (s *Supervisor) startHeartbeat() {
	if !s.opts.Agent.Builtin.KeepsTime() {
		return
	}
	s.noteMissed()
	ctx, cancel := context.WithCancel(context.Background())
	tick := s.opts.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.hbCancel, s.hbDone = cancel, done
	s.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.beat(ctx)
			}
		}
	}()
}

// cancelHeartbeat ends the ticker: it fires nothing more.
func (s *Supervisor) cancelHeartbeat() {
	s.mu.Lock()
	cancel := s.hbCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stopHeartbeat ends the ticker and waits for a fire in flight to give up.
func (s *Supervisor) stopHeartbeat() {
	s.cancelHeartbeat()
	s.mu.Lock()
	done := s.hbDone
	s.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

// noteMissed records the tasks that came due between the heartbeat's last
// look and now, while no supervisor ran the agent, and keeps their names
// for the next turn.
func (s *Supervisor) noteMissed() {
	home, name := s.opts.Home, s.opts.Agent.Name
	now := s.now()
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return
	}
	var missed []string
	_ = s.updateTasks(func(ts *Tasks) error {
		if !ts.Checked.IsZero() && heartbeatOn(s.opts.Agent, cfg) {
			due := ts.due(ts.Checked, now)
			for _, t := range ts.Tasks {
				times := due[t.ID]
				if len(times) == 0 {
					continue
				}
				for _, at := range times {
					s.logBeat(t, at, beatMissed, "the agent was not running")
				}
				missed = append(missed, strconv.Quote(t.Name)+" (due "+times[len(times)-1].In(zoneOf(t)).Format("Mon 2 Jan 15:04")+")")
			}
		}
		ts.Checked = now
		return nil
	})
	if len(missed) == 0 {
		return
	}
	more := ""
	if len(missed) > maxMissedNamed {
		more = fmt.Sprintf(", and %d more", len(missed)-maxMissedNamed)
		missed = missed[:maxMissedNamed]
	}
	s.mu.Lock()
	s.missed = "(Agentry's heartbeat: these tasks of yours came due while you were not running, and did not run: " +
		strings.Join(missed, ", ") + more + ". Mention it to the person if it matters; task_list shows your tasks.)\n\n"
	s.mu.Unlock()
}

// beat fires the tasks that came due since the last look.
func (s *Supervisor) beat(ctx context.Context) {
	home, name := s.opts.Home, s.opts.Agent.Name
	now := s.now()
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return
	}
	on := heartbeatOn(s.opts.Agent, cfg)
	var due map[string][]time.Time
	var tasks []Task
	_ = s.updateTasks(func(ts *Tasks) error {
		from := ts.Checked
		if from.IsZero() || from.After(now) {
			from = now.Add(-time.Minute)
		}
		if on {
			due, tasks = ts.due(from, now), slices.Clone(ts.Tasks)
		}
		ts.Checked = now
		return nil
	})
	type fire struct {
		t  Task
		at time.Time
	}
	var fires []fire
	for _, t := range tasks {
		times := due[t.ID]
		if len(times) == 0 {
			continue
		}
		// One turn for a task, at its latest time: the earlier ones came
		// while a turn before it held the heartbeat.
		for _, at := range times[:len(times)-1] {
			s.logBeat(t, at, beatSkipped, "its time came again before its last turn could be sent")
		}
		fires = append(fires, fire{t, times[len(times)-1]})
	}
	slices.SortFunc(fires, func(a, b fire) int { return a.at.Compare(b.at) })
	for _, f := range fires {
		if ctx.Err() != nil {
			s.logBeat(f.t, f.at, beatSkipped, "the agent stopped")
			continue
		}
		s.fireTask(ctx, f.t, f.at)
	}
}

// fireTask sends a task's turn once no other turn is in flight.
func (s *Supervisor) fireTask(ctx context.Context, t Task, at time.Time) {
	if !s.waitTurn(ctx) {
		s.logBeat(t, at, beatSkipped, "the agent stopped")
		return
	}
	defer s.turn.Unlock()
	// The task as it is now: it may have been paused, changed or removed
	// while the turn before it ran.
	var cur Task
	refused := ""
	_ = s.updateTasks(func(ts *Tasks) error {
		i := ts.index(t.ID)
		switch {
		case i < 0:
			refused = "it was removed"
		case ts.Tasks[i].State != TaskOn:
			refused = "it was paused"
		case ts.firesToday(s.now()) >= MaxTaskFiresPerDay:
			refused = fmt.Sprintf("the tasks fired %d times in the last day, the most they may", MaxTaskFiresPerDay)
		default:
			cur = ts.Tasks[i]
			ts.Fires = append(ts.Fires, s.now().UTC())
		}
		return nil
	})
	if refused != "" {
		s.logBeat(t, at, beatSkipped, refused)
		return
	}
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil || !heartbeatOn(s.opts.Agent, cfg) {
		s.logBeat(cur, at, beatSkipped, "the heartbeat was switched off")
		return
	}
	events, unsubscribe := s.Subscribe()
	defer unsubscribe()
	s.mu.Lock()
	s.taskTurn = cur.ID
	s.mu.Unlock()
	frame := "(Agentry's heartbeat: your task " + cur.ID + ", " + strconv.Quote(cur.Name) + ", came due at " +
		at.In(zoneOf(cur)).Format("15:04 Mon 2 Jan") + " " + cur.Zone + ". You registered it; its instruction is the message below. " +
		"Act on it as you would a request from the person, within your instructions. The person may not be reading: say what you did in your answer.)\n\n"
	err = s.sendLocked(ctx, frame, "Task: "+cur.Name+"\n\n"+cur.Instruction)
	if err != nil {
		// No user turn came to take the mark: the person's next turn is not
		// the task's.
		s.mu.Lock()
		s.taskTurn = ""
		s.mu.Unlock()
	}
	answer := lastAnswer(events)
	result := firstLine(answer)
	if err != nil {
		result = "not answered: " + err.Error()
	}
	_ = s.updateTasks(func(ts *Tasks) error {
		if i := ts.index(cur.ID); i >= 0 {
			ts.Tasks[i].LastFire, ts.Tasks[i].LastResult = s.now().UTC(), result
		}
		return nil
	})
	if err != nil {
		s.logBeat(cur, at, beatSkipped, "the turn failed: "+err.Error())
		return
	}
	s.logBeat(cur, at, beatFired, "")
	s.notice("Task " + strconv.Quote(cur.Name) + " ran: " + result)
}

// waitTurn takes the turn lock once the turn in flight ends; false when ctx
// ends first.
func (s *Supervisor) waitTurn(ctx context.Context) bool {
	for {
		if s.turn.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// answerWait is how long a task's result waits for the answer's event, which
// reaches the watchers after the turn returns.
const answerWait = 2 * time.Second

// lastAnswer is the text of the assistant turn that answered the task: the
// first among the events once the task's own user turn has come.
func lastAnswer(events <-chan session.Event) string {
	seen := false
	timeout := time.After(answerWait)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return ""
			}
			switch {
			case ev.Kind == session.KindUserTurn:
				seen = true
			case seen && ev.Kind == session.KindAssistantTurn:
				return ev.Text
			case seen && ev.Kind == session.KindError:
				return "error: " + ev.Text
			}
		case <-timeout:
			return ""
		}
	}
}

// zoneOf is a task's zone, UTC when this computer does not know it.
func zoneOf(t Task) *time.Location {
	loc, _, err := location(t.Zone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// timeFrame tells an agent that keeps time the time now.
func timeFrame(now time.Time) string {
	zone := schedule.LocalZone()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	return "(The time now: " + now.In(loc).Format("Monday 2 January 2006, 15:04") + ", " + zone + ".)\n\n"
}

// updateTasks reads the agent's tasks, changes them and writes them, one
// change at a time.
func (s *Supervisor) updateTasks(change func(*Tasks) error) error {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	ts, err := LoadTasks(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return err
	}
	if err := change(&ts); err != nil {
		return err
	}
	return saveTasks(s.opts.Home, s.opts.Agent.Name, ts)
}

// Tasks are the running agent's tasks, each with its next fire.
func (s *Supervisor) Tasks() ([]Task, error) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	ts, err := LoadTasks(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return nil, err
	}
	return ts.Listed(s.now()), nil
}

// ChangeTask applies a person's pause, resume or remove to the running
// agent's task.
func (s *Supervisor) ChangeTask(op TaskOp) ([]Task, error) {
	if !s.opts.Agent.Builtin.KeepsTime() {
		return nil, fmt.Errorf("the %s agent has no tasks", s.opts.Agent.Name)
	}
	if err := s.updateTasks(func(ts *Tasks) error { return ts.ApplyOp(op, s.now()) }); err != nil {
		return nil, err
	}
	return s.Tasks()
}

// logBeat appends a heartbeat line to the running segment's receipts.
func (s *Supervisor) logBeat(t Task, due time.Time, outcome, reason string) {
	s.mu.Lock()
	dir, segment, run := filepath.Join(s.outDir, "receipts"), s.segment, s.runID
	s.mu.Unlock()
	line, err := json.Marshal(struct {
		Time    time.Time `json:"time"`
		Task    string    `json:"task"`
		Name    string    `json:"name"`
		Due     time.Time `json:"due"`
		Outcome string    `json:"outcome"`
		Reason  string    `json:"reason,omitempty"`
		Segment int       `json:"segment"`
		Run     string    `json:"run"`
	}{s.now().UTC(), t.ID, t.Name, due.UTC(), outcome, reason, segment, run})
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, heartbeatLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the segment's own receipt
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	_ = errors.Join(werr, f.Close())
}

// TaskList is the agent's tasks while it is not running, each with its next
// fire.
func TaskList(home string, a Agent) ([]Task, error) {
	ts, err := LoadTasks(home, a.Name)
	if err != nil {
		return nil, err
	}
	return ts.Listed(time.Now()), nil
}

// ChangeTaskOf applies a person's pause, resume or remove to an agent's
// task: through its supervisor while it runs, under its owner lock while it
// does not.
func ChangeTaskOf(ctx context.Context, home string, a Agent, client string, op TaskOp) ([]Task, bool, error) {
	if !a.Builtin.KeepsTime() {
		return nil, false, fmt.Errorf("the %s agent has no tasks", a.Name)
	}
	owner, err := Acquire(home, a.Name, client)
	var held *HeldError
	if errors.As(err, &held) {
		ts, err := RequestTaskChange(ctx, home, a.Name, op)
		return ts, true, err
	}
	if err != nil {
		return nil, false, err
	}
	defer owner.Release()
	ts, err := LoadTasks(home, a.Name)
	if err != nil {
		return nil, false, err
	}
	if err := ts.ApplyOp(op, time.Now()); err != nil {
		return nil, false, err
	}
	if err := saveTasks(home, a.Name, ts); err != nil {
		return nil, false, err
	}
	return ts.Listed(time.Now()), false, nil
}

// TasksOf are an agent's tasks: its supervisor's while it runs, its file's
// while it does not.
func TasksOf(ctx context.Context, home string, a Agent) ([]Task, bool, error) {
	if !a.Builtin.KeepsTime() {
		return nil, false, nil
	}
	ts, err := RequestTasks(ctx, home, a.Name)
	if err == nil {
		return ts, true, nil
	}
	if !errors.Is(err, ErrNotRunning) {
		return nil, false, err
	}
	ts, err = TaskList(home, a)
	return ts, false, err
}

// isTaskTool says the app link tool is one of the task tools.
func isTaskTool(tool string) bool {
	return slices.Contains([]string{applink.TaskAdd, applink.TaskList, applink.TaskUpdate, applink.TaskRemove}, tool)
}

// heartbeatOff is the task tools' answer while the heartbeat is off.
const heartbeatOff = "Your heartbeat is off, so no task fires and none can be registered or changed. The person switches it on in Agentry (\"Its heartbeat and tasks\"); tell them so if they asked for something at a time."

// answerTask answers a task tool: the agent's own tasks, and nothing else.
// An argument the tool does not take refuses the call.
func (s *Supervisor) answerTask(raw json.RawMessage, tool string) applink.Answer {
	var args struct {
		Task string `json:"task"`
		TaskSpec
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&args) != nil {
			return applink.Answer{Text: tool + " was given an argument it does not take", IsError: true}
		}
	}
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return applink.Answer{Text: err.Error(), IsError: true}
	}
	on := heartbeatOn(s.opts.Agent, cfg)
	if tool == applink.TaskList {
		ts, err := s.Tasks()
		if err != nil {
			return applink.Answer{Text: err.Error(), IsError: true}
		}
		head := ""
		if !on {
			head = heartbeatOff + "\n\n"
		}
		return applink.Answer{Text: head + describeTasks(ts)}
	}
	if !on {
		return applink.Answer{Text: heartbeatOff, IsError: true}
	}
	if tool == applink.TaskAdd && args.Task != "" {
		return applink.Answer{Text: "task_add names no task: it makes one", IsError: true}
	}
	if tool == applink.TaskRemove && args.TaskSpec != (TaskSpec{}) {
		return applink.Answer{Text: "task_remove takes a task's id alone", IsError: true}
	}
	var t Task
	err = s.updateTasks(func(ts *Tasks) error {
		var err error
		switch tool {
		case applink.TaskAdd:
			t, err = ts.AddTask(args.TaskSpec, s.now())
		case applink.TaskUpdate:
			t, err = ts.UpdateTask(args.Task, args.TaskSpec, s.now())
		case applink.TaskRemove:
			t, err = ts.RemoveTask(args.Task)
		}
		return err
	})
	if err != nil {
		return applink.Answer{Text: "not saved: " + err.Error(), IsError: true}
	}
	switch tool {
	case applink.TaskRemove:
		return applink.Answer{Text: "Removed " + t.ID + ", " + strconv.Quote(t.Name) + "."}
	case applink.TaskAdd:
		return applink.Answer{Text: "Registered " + describeTask(Tasks{Tasks: []Task{t}}.Listed(s.now())[0])}
	}
	return applink.Answer{Text: "Saved " + describeTask(Tasks{Tasks: []Task{t}}.Listed(s.now())[0])}
}

// describeTask is one task as the agent is told it.
func describeTask(t Task) string {
	loc := zoneOf(t)
	b := new(strings.Builder)
	fmt.Fprintf(b, "%s, %s: %q in %s, %s", t.ID, strconv.Quote(t.Name), t.Cron, t.Zone, t.State)
	if !t.Next.IsZero() {
		fmt.Fprintf(b, ", next at %s", t.Next.In(loc).Format("Mon 2 Jan 2006 15:04"))
	}
	if !t.LastFire.IsZero() {
		fmt.Fprintf(b, "; last fired %s, answered %s", t.LastFire.In(loc).Format("Mon 2 Jan 15:04"), strconv.Quote(t.LastResult))
	}
	fmt.Fprintf(b, ". Instruction: %s", strconv.Quote(t.Instruction))
	return b.String()
}

// describeTasks are the tasks, one a line.
func describeTasks(ts []Task) string {
	if len(ts) == 0 {
		return "You have no tasks."
	}
	lines := make([]string, 0, len(ts))
	for _, t := range ts {
		lines = append(lines, "- "+describeTask(t))
	}
	return strings.Join(lines, "\n")
}
