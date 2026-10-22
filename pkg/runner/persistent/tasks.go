package persistent

// An agent that keeps time registers its own tasks: an instruction it is to
// act on at the times a cron expression names, in a zone. Its heartbeat
// (heartbeat.go) fires a turn for each task when that time comes, while the
// agent runs. The tasks live in the agent's state folder, written by its
// supervisor alone while it runs and under its owner lock while it does not;
// a task registers turns and never a grant.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// The task limits: how many an agent keeps, how often one may fire, how
// many fires a day all of them may make, and the sizes of a name and an
// instruction.
const (
	MaxTasks           = 25
	MinTaskInterval    = 15 * time.Minute
	MaxTaskFiresPerDay = 96
	MaxTaskNameBytes   = 80
	MaxInstruction     = 4096
	maxResultLine      = 300
)

// tasksFile is the agent's tasks, in its state folder.
const (
	tasksFile    = "tasks.yaml"
	tasksVersion = 1
)

// Task states.
const (
	TaskOn     = "on"
	TaskPaused = "paused"
)

// TaskMadeByAgent says the agent registered the task.
const TaskMadeByAgent = "agent"

// Task is one registered task.
type Task struct {
	ID          string    `yaml:"id" json:"id"`
	Name        string    `yaml:"name" json:"name"`
	Cron        string    `yaml:"cron" json:"cron"`
	Zone        string    `yaml:"zone" json:"zone"`
	Instruction string    `yaml:"instruction" json:"instruction"`
	State       string    `yaml:"state" json:"state"`
	MadeBy      string    `yaml:"madeBy" json:"madeBy"`
	Created     time.Time `yaml:"created" json:"created"`
	// LastFire is when the task last fired, and LastResult the first line
	// of that turn's answer, or why it did not answer.
	LastFire   time.Time `yaml:"lastFire,omitempty" json:"lastFire,omitzero"`
	LastResult string    `yaml:"lastResult,omitempty" json:"lastResult,omitempty"`
	// Next is when it fires next; set when the task is listed, not kept.
	Next time.Time `yaml:"-" json:"next,omitzero"`
}

// Tasks is an agent's tasks file.
type Tasks struct {
	Version int `yaml:"version"`
	// Checked is the last time the heartbeat looked for tasks that came
	// due: the next start names the ones that came due after it.
	Checked time.Time `yaml:"checked,omitempty"`
	// Fires are the times of the fires of the last day, for the day's cap.
	Fires  []time.Time `yaml:"fires,omitempty"`
	NextID int         `yaml:"nextId"`
	Tasks  []Task      `yaml:"tasks"`
}

// LoadTasks reads the agent's tasks; none when it has no file.
func LoadTasks(home, name string) (Tasks, error) {
	b, err := os.ReadFile(filepath.Join(Dir(home, name), tasksFile)) //nolint:gosec // the agent's own state
	if errors.Is(err, os.ErrNotExist) {
		return Tasks{Version: tasksVersion}, nil
	}
	if err != nil {
		return Tasks{}, err
	}
	var t Tasks
	if err := yaml.Unmarshal(b, &t); err != nil {
		return Tasks{}, fmt.Errorf("the %s agent's %s: %w", name, tasksFile, err)
	}
	if t.Version > tasksVersion {
		return Tasks{}, fmt.Errorf("the %s agent's %s is version %d, which this build does not know", name, tasksFile, t.Version)
	}
	t.Version = tasksVersion
	return t, nil
}

// saveTasks writes the agent's tasks, 0600, through a temporary file.
func saveTasks(home, name string, t Tasks) error {
	dir := Dir(home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, tasksFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, tasksFile))
}

// TaskSpec is what the agent gives to add or update a task; an update sets
// only what it names.
type TaskSpec struct {
	Name        *string `json:"name,omitempty"`
	Cron        *string `json:"cron,omitempty"`
	Zone        *string `json:"zone,omitempty"`
	Instruction *string `json:"instruction,omitempty"`
	Paused      *bool   `json:"paused,omitempty"`
}

// TaskOp is a person's change to a task: pause, resume or remove.
type TaskOp struct {
	ID string `json:"id"`
	Op string `json:"op"`
}

// The person's task operations.
const (
	TaskOpPause  = "pause"
	TaskOpResume = "resume"
	TaskOpRemove = "remove"
)

// location is a task's zone; the host's when it names none.
func location(zone string) (*time.Location, string, error) {
	if zone == "" {
		zone = schedule.LocalZone()
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return nil, "", fmt.Errorf("%q is not a time zone this computer knows (an IANA name, such as Europe/Paris)", zone)
	}
	return loc, zone, nil
}

// cleanTaskText is a name or an instruction as a task keeps it: trimmed,
// valid text of at most max bytes, with no control character but a newline
// and a tab in an instruction.
func cleanTaskText(what, s string, max int, multiline bool) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", fmt.Errorf("a task's %s is empty", what)
	case len(s) > max || !utf8.ValidString(s):
		return "", fmt.Errorf("a task's %s is at most %d bytes of text", what, max)
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("a task's %s holds a control or formatting character", what)
		}
	}
	return s, nil
}

// apply sets on t what spec names, checked.
func (spec TaskSpec) apply(t *Task) error {
	if spec.Name != nil {
		n, err := cleanTaskText("name", *spec.Name, MaxTaskNameBytes, false)
		if err != nil {
			return err
		}
		t.Name = n
	}
	if spec.Instruction != nil {
		in, err := cleanTaskText("instruction", *spec.Instruction, MaxInstruction, true)
		if err != nil {
			return err
		}
		t.Instruction = in
	}
	if spec.Zone != nil {
		_, z, err := location(*spec.Zone)
		if err != nil {
			return err
		}
		t.Zone = z
	}
	if spec.Cron != nil {
		t.Cron = strings.Join(strings.Fields(*spec.Cron), " ")
	}
	if spec.Paused != nil {
		t.State = TaskOn
		if *spec.Paused {
			t.State = TaskPaused
		}
	}
	return nil
}

// checkTimes holds a task's time to the limits: a time that comes, no more
// often than every MinTaskInterval, and all the tasks that are on making no
// more than MaxTaskFiresPerDay fires in the day after now.
func checkTimes(ts []Task, t Task, now time.Time) error {
	cr, err := ParseCron(t.Cron)
	if err != nil {
		return err
	}
	loc, _, err := location(t.Zone)
	if err != nil {
		return err
	}
	if _, err := cr.Next(now, loc); err != nil {
		return fmt.Errorf("the time %q never comes", t.Cron)
	}
	if closer, _ := cr.CloserThan(now, loc, MinTaskInterval); closer {
		return fmt.Errorf("a task fires at most every %d minutes; %q fires more often", int(MinTaskInterval.Minutes()), t.Cron)
	}
	total := 0
	for _, o := range append(slices.Clone(ts), t) {
		if o.State != TaskOn {
			continue
		}
		c, err := ParseCron(o.Cron)
		if err != nil {
			continue
		}
		l, _, err := location(o.Zone)
		if err != nil {
			continue
		}
		total += c.Count(now, now.Add(24*time.Hour), l, MaxTaskFiresPerDay)
	}
	if total > MaxTaskFiresPerDay {
		return fmt.Errorf("the tasks that are on would fire %d times in the next day, and they may fire at most %d", total, MaxTaskFiresPerDay)
	}
	return nil
}

// AddTask registers a task the agent made.
func (ts *Tasks) AddTask(spec TaskSpec, now time.Time) (Task, error) {
	if len(ts.Tasks) >= MaxTasks {
		return Task{}, fmt.Errorf("an agent keeps at most %d tasks; remove one first", MaxTasks)
	}
	if spec.Name == nil || spec.Cron == nil || spec.Instruction == nil {
		return Task{}, errors.New("a task needs a name, a cron time and an instruction")
	}
	t := Task{State: TaskOn, MadeBy: TaskMadeByAgent, Created: now.UTC()}
	if spec.Zone == nil {
		spec.Zone = new(string)
	}
	if err := spec.apply(&t); err != nil {
		return Task{}, err
	}
	if err := checkTimes(ts.Tasks, t, now); err != nil {
		return Task{}, err
	}
	ts.NextID++
	t.ID = "t" + strconv.Itoa(ts.NextID)
	ts.Tasks = append(ts.Tasks, t)
	return t, nil
}

// UpdateTask changes the task id as spec says.
func (ts *Tasks) UpdateTask(id string, spec TaskSpec, now time.Time) (Task, error) {
	i := ts.index(id)
	if i < 0 {
		return Task{}, fmt.Errorf("there is no task %s (task_list lists them)", id)
	}
	t := ts.Tasks[i]
	if err := spec.apply(&t); err != nil {
		return Task{}, err
	}
	others := slices.Delete(slices.Clone(ts.Tasks), i, i+1)
	if err := checkTimes(others, t, now); err != nil {
		return Task{}, err
	}
	ts.Tasks[i] = t
	return t, nil
}

// RemoveTask removes the task id.
func (ts *Tasks) RemoveTask(id string) (Task, error) {
	i := ts.index(id)
	if i < 0 {
		return Task{}, fmt.Errorf("there is no task %s", id)
	}
	t := ts.Tasks[i]
	ts.Tasks = slices.Delete(ts.Tasks, i, i+1)
	return t, nil
}

// ApplyOp applies a person's pause, resume or remove.
func (ts *Tasks) ApplyOp(op TaskOp, now time.Time) error {
	switch op.Op {
	case TaskOpRemove:
		_, err := ts.RemoveTask(op.ID)
		return err
	case TaskOpPause, TaskOpResume:
		paused := op.Op == TaskOpPause
		_, err := ts.UpdateTask(op.ID, TaskSpec{Paused: &paused}, now)
		return err
	}
	return fmt.Errorf("a task is paused, resumed or removed; %q is none of them", op.Op)
}

func (ts *Tasks) index(id string) int {
	return slices.IndexFunc(ts.Tasks, func(t Task) bool { return t.ID == id })
}

// Listed are the tasks with each one's next fire set, from now.
func (ts Tasks) Listed(now time.Time) []Task {
	out := slices.Clone(ts.Tasks)
	for i, t := range out {
		if t.State != TaskOn {
			continue
		}
		cr, err := ParseCron(t.Cron)
		if err != nil {
			continue
		}
		loc, _, err := location(t.Zone)
		if err != nil {
			continue
		}
		out[i].Next, _ = cr.Next(now, loc)
	}
	return out
}

// due are the times each task that is on came due after from and up to
// to, by task id, in order.
func (ts Tasks) due(from, to time.Time) map[string][]time.Time {
	out := map[string][]time.Time{}
	for _, t := range ts.Tasks {
		if t.State != TaskOn {
			continue
		}
		cr, err := ParseCron(t.Cron)
		if err != nil {
			continue
		}
		loc, _, err := location(t.Zone)
		if err != nil {
			continue
		}
		for at := from; ; {
			next, err := cr.Next(at, loc)
			if err != nil || next.After(to) {
				break
			}
			out[t.ID] = append(out[t.ID], next)
			at = next
			if len(out[t.ID]) > MaxTaskFiresPerDay {
				break
			}
		}
	}
	return out
}

// firesToday are the fires in the day before now, the rest dropped.
func (ts *Tasks) firesToday(now time.Time) int {
	ts.Fires = slices.DeleteFunc(ts.Fires, func(f time.Time) bool { return now.Sub(f) >= 24*time.Hour })
	return len(ts.Fires)
}

// firstLine is the first line of an answer, cut for a task's result.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return cutText(strings.TrimSpace(s), maxResultLine)
}
