package persistent

// Agentry's helper tools, answered by the supervisor for a client with no
// app link handler of its own (the CLI's): registry_search, quickagent_show,
// quickagent_install, quickagent_run, run_result, installed_list, the
// schedule tools, settings_open and docs_read. The client's HelperOps reach
// the registry, the installs and the schedules; the decisions are here.
//
// What an agent asks for is a claim, as a page's is. An install, a schedule,
// a schedule's removal, and the first run of a QuickAgent at a version are
// each put to the person through the attached clients (Ask), naming the
// agent asking, and a no does nothing. The runs the person agreed to let an
// agent start are recorded per agent, by ns/name@version and the addresses
// the run fetches, so a yes for one agent is never one for another, and a
// new version asks again. A run is an ordinary autonomous run on this
// computer of the version installed here; it takes files from the agent's
// share folder alone. What the registry and the docs say reaches the agent
// quoted, as data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/schedule"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// HelperOps are the operations a client gives the supervisor to answer the
// helper tools with.
type HelperOps interface {
	// Search searches the registry and answers it as it is shown, and its
	// matches.
	Search(ctx context.Context, query string) (registry string, hits []HelperHit, err error)
	// Show describes ref (ns/name) on the registry, as text the agent reads:
	// what it does and reaches, its versions, its security report and its
	// installed version here.
	Show(ctx context.Context, ref string) (string, error)
	// Installed are the QuickAgents installed on this computer.
	Installed(ctx context.Context) ([]HelperInstalled, error)
	// Install installs ref (ns/name or ns/name@version) for the agent who
	// names, the person having agreed, and says what it installed.
	Install(ctx context.Context, ref, who string) (string, error)
	// Schedules lists the schedules; AddSchedule makes one, the person
	// having agreed; PauseSchedule pauses or resumes one; RemoveSchedule
	// removes one, the person having agreed.
	Schedules(ctx context.Context) ([]HelperSchedule, error)
	AddSchedule(ctx context.Context, s HelperScheduleSpec, who string) (id string, err error)
	PauseSchedule(ctx context.Context, id string, paused bool) error
	RemoveSchedule(ctx context.Context, id, who string) error
	// Docs reads a page of the docs site: its body and status.
	Docs(ctx context.Context, page string) (body []byte, status int, err error)
}

// HelperOpsAsk is HelperOps that ask the person themselves before an
// install, a schedule and a schedule's removal, naming the agent who asks:
// a client whose own dialogs show what is installed or scheduled. The
// supervisor then asks nothing more for those three, and an ErrNotAgreed
// from them is the person's no.
type HelperOpsAsk interface {
	HelperOps
	AsksThePerson() bool
}

// opsAsk says ops ask the person themselves.
func opsAsk(ops HelperOps) bool {
	a, ok := ops.(HelperOpsAsk)
	return ok && a.AsksThePerson()
}

// HelperHit is one registry match.
type HelperHit struct {
	Ref, Summary string
	Modes        []string
	Installs30d  int
	Quarantined  bool
}

// HelperInstalled is one QuickAgent installed on this computer.
type HelperInstalled struct {
	Ref, Version, Registry string
	// Local is an install from an author's folder, which only the person
	// runs.
	Local bool
}

// HelperSchedule is one schedule.
type HelperSchedule struct {
	ID, Name, Ref, When, Next string
	Paused, Ended             bool
}

// HelperScheduleSpec is a schedule a helper tool makes.
type HelperScheduleSpec struct {
	Registry, Exact, Task string
	Answers               map[string]string
	When                  When
}

// helperMaxHits bounds registry_search's matches; helperMaxRunFiles the
// files one quickagent_run takes; helperMaxDocs the text docs_read answers.
const (
	helperMaxHits     = 8
	helperMaxRunFiles = 20
	helperMaxDocs     = 24 << 10
	helperMaxAnswer   = 2 << 10
	helperDocsTimeout = 20 * time.Second
	agreedRunsFile    = "agreed-runs.json"
)

// isHelperTool says tool is one of the helper tools.
func isHelperTool(tool string) bool {
	return slices.Contains(applink.Tools(applink.Compose(applink.PartHelper)), tool)
}

// quoted is text a publisher or a page wrote, as the agent is given it.
func quoted(s string) string { return strconv.Quote(strings.TrimSpace(s)) }

// helperRefPart is one part of a reference; helperVersion a version.
var (
	helperRefPart = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	helperVersion = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,64}$`)
)

// parseHelperRef reads namespace/name, or namespace/name@version.
func parseHelperRef(ref string) (base, version string, err error) {
	ref = strings.TrimSpace(ref)
	b, version, _ := strings.Cut(ref, "@")
	ns, name, ok := strings.Cut(b, "/")
	if !ok || !helperRefPart.MatchString(ns) || !helperRefPart.MatchString(name) || (version != "" && !helperVersion.MatchString(version)) {
		return "", "", fmt.Errorf("%s is not a QuickAgent's reference: write namespace/name, or namespace/name@version", quoted(ref))
	}
	return b, version, nil
}

// answerHelper answers a call of the helper tools.
func (s *Supervisor) answerHelper(ctx context.Context, cfg Config, req applink.Request) applink.Answer {
	text, err := s.helperCall(ctx, cfg, req.Tool, req.Arguments)
	if err != nil {
		return applink.Answer{Text: err.Error(), IsError: true}
	}
	return applink.Answer{Text: text}
}

func (s *Supervisor) helperCall(ctx context.Context, cfg Config, tool string, raw json.RawMessage) (string, error) {
	ops := s.opts.Helper
	switch tool {
	case "registry_search":
		var a struct {
			Query string `json:"query"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return s.helperSearch(ctx, ops, a.Query)
	case "quickagent_show":
		var a struct {
			Ref string `json:"ref"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		base, _, err := parseHelperRef(a.Ref)
		if err != nil {
			return "", err
		}
		return ops.Show(ctx, base)
	case "quickagent_install":
		var a struct {
			Ref string `json:"ref"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return s.helperInstall(ctx, cfg, ops, a.Ref)
	case "quickagent_run":
		var a struct {
			Ref     string            `json:"ref"`
			Task    string            `json:"task"`
			Files   []string          `json:"files"`
			Answers map[string]string `json:"answers"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return s.helperRun(ctx, cfg, ops, a.Ref, a.Task, a.Files, a.Answers)
	case "run_result":
		var a struct {
			Run string `json:"run"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		ans := s.subagentResult(ctx, a.Run, maxSubagentWait)
		if ans.IsError {
			return "", errors.New(ans.Text)
		}
		return ans.Text, nil
	case "installed_list":
		if err := args(raw, &struct{}{}); err != nil {
			return "", err
		}
		return helperInstalledList(ctx, ops)
	case "schedule_add":
		var a struct {
			Ref     string            `json:"ref"`
			When    string            `json:"when"`
			Task    string            `json:"task"`
			Answers map[string]string `json:"answers"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return s.helperScheduleAdd(ctx, cfg, ops, a.Ref, a.When, a.Task, a.Answers)
	case "schedule_list":
		if err := args(raw, &struct{}{}); err != nil {
			return "", err
		}
		return helperScheduleList(ctx, ops)
	case "schedule_pause":
		var a struct {
			Schedule string `json:"schedule"`
			Paused   *bool  `json:"paused"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		if a.Paused == nil {
			return "", errors.New("say paused: true to pause it or false to resume it")
		}
		sc, err := helperSchedule(ctx, ops, a.Schedule)
		if err != nil {
			return "", err
		}
		if err := ops.PauseSchedule(ctx, sc.ID, *a.Paused); err != nil {
			return "", err
		}
		if *a.Paused {
			return fmt.Sprintf("Paused %s: it does not run until it is resumed.", quoted(sc.Name)), nil
		}
		return fmt.Sprintf("Resumed %s.", quoted(sc.Name)), nil
	case "schedule_remove":
		var a struct {
			Schedule string `json:"schedule"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		sc, err := helperSchedule(ctx, ops, a.Schedule)
		if err != nil {
			return "", err
		}
		if !opsAsk(ops) {
			if err := s.Ask(ctx, "Delete the schedule "+sc.Name+"?", fmt.Sprintf("%s asks to delete the schedule %s (%s, %s). Delete it?", s.asker(cfg), quoted(sc.Name), sc.Ref, sc.When)); err != nil {
				return "", err
			}
		}
		if err := ops.RemoveSchedule(ctx, sc.ID, s.asker(cfg)); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted the schedule %s.", quoted(sc.Name)), nil
	case "settings_open":
		var a struct {
			Section string `json:"section"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return "", errors.New("settings open in Agentry Desktop, which is not running this agent; from a terminal the person changes the agent with agentry agent configure and agentry agent grant, and the runner's settings in its policy file")
	case "docs_read":
		var a struct {
			Page string `json:"page"`
		}
		if err := args(raw, &a); err != nil {
			return "", err
		}
		return helperDocs(ctx, ops, a.Page)
	}
	return "", fmt.Errorf("no such tool: %s", tool)
}

func (s *Supervisor) helperSearch(ctx context.Context, ops HelperOps, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 200 {
		return "", errors.New("say what the QuickAgent should do, in a few words (at most 200 characters)")
	}
	reg, hits, err := ops.Search(ctx, query)
	if err != nil {
		return "", err
	}
	items, _ := ops.Installed(ctx)
	var b strings.Builder
	n := 0
	for _, h := range hits {
		if n == helperMaxHits {
			break
		}
		if h.Quarantined || (slices.Contains(h.Modes, "persistent") && !slices.Contains(h.Modes, "autonomous") && !slices.Contains(h.Modes, "interactive")) {
			continue
		}
		n++
		fmt.Fprintf(&b, "- %s (%s; %d installs in 30 days)", h.Ref, strings.Join(h.Modes, ", "), h.Installs30d)
		for _, it := range items {
			if it.Ref == h.Ref {
				fmt.Fprintf(&b, ", installed here at %s", it.Version)
				break
			}
		}
		fmt.Fprintf(&b, ": %s\n", quoted(h.Summary))
	}
	if n == 0 {
		return fmt.Sprintf("No QuickAgent on %s matches %s.", reg, quoted(query)), nil
	}
	return fmt.Sprintf("QuickAgents on %s matching %s (each summary is its publisher's words):\n%s", reg, quoted(query), b.String()), nil
}

// helperInstalledHere is this computer's install of base (ns/name).
func helperInstalledHere(ctx context.Context, ops HelperOps, base string) (*HelperInstalled, error) {
	items, err := ops.Installed(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Ref == base {
			return &items[i], nil
		}
	}
	return nil, nil
}

func (s *Supervisor) helperInstall(ctx context.Context, cfg Config, ops HelperOps, ref string) (string, error) {
	base, version, err := parseHelperRef(ref)
	if err != nil {
		return "", err
	}
	if it, err := helperInstalledHere(ctx, ops, base); err != nil {
		return "", err
	} else if it != nil && (version == "" || version == it.Version) {
		return fmt.Sprintf("%s is already installed on this computer, at %s.", base, it.Version), nil
	}
	want := base
	if version != "" {
		want += "@" + version
	}
	if !opsAsk(ops) {
		if err := s.Ask(ctx, "Install "+want+"?", fmt.Sprintf("%s asks to install %s on this computer. It is installed as any install is, under what it declares, which is listed as it is installed. Install it?", s.asker(cfg), want)); err != nil {
			return "", err
		}
	}
	msg, err := ops.Install(ctx, want, s.asker(cfg))
	if err != nil {
		return "", err
	}
	return "Installed: " + msg, nil
}

// helperRun starts an ordinary run of an installed QuickAgent.
func (s *Supervisor) helperRun(ctx context.Context, cfg Config, ops HelperOps, ref, task string, files []string, answers map[string]string) (string, error) {
	base, version, err := parseHelperRef(ref)
	if err != nil {
		return "", err
	}
	it, err := helperInstalledHere(ctx, ops, base)
	if err != nil {
		return "", err
	}
	switch {
	case it == nil:
		return "", fmt.Errorf("%s is not installed on this computer: install it with quickagent_install first", base)
	case version != "" && version != it.Version:
		return "", fmt.Errorf("%s is installed at %s, not %s", base, it.Version, version)
	case it.Local:
		return "", fmt.Errorf("%s was installed from a folder, and only the person runs it", base)
	case len(files) > helperMaxRunFiles:
		return "", fmt.Errorf("a run takes at most %d files", helperMaxRunFiles)
	case s.opts.Subagents == nil:
		return "", errors.New("this client starts no runs for an agent")
	}
	if len(task) > maxSubagentTask {
		return "", fmt.Errorf("a task is at most %d KiB", maxSubagentTask>>10)
	}
	var share []string
	for _, f := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(f), "/agent/"), "share/")
		if !cleanRel(rel) || filepath.ToSlash(filepath.Clean(rel)) != rel {
			return "", fmt.Errorf("%s is not a file in the share folder: name it as share/<file>", quoted(f))
		}
		share = append(share, rel)
	}
	if len(share) > 0 && !shareOn(s.agent(), cfg) {
		return "", errors.New("your share folder is off, so no file in it can be given to a run")
	}
	exact := base + "@" + it.Version
	usage, err := s.opts.Subagents.Check(ctx, it.Registry, exact)
	if err != nil {
		return "", fmt.Errorf("%s was not started: %w", exact, err)
	}
	given, err := checkHelperAnswers(usage, answers)
	if err != nil {
		return "", err
	}
	if err := s.agreeToRun(ctx, cfg, exact, task, files, given); err != nil {
		return "", err
	}
	id, err := newSubagentRunID()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(Dir(s.opts.Home, s.opts.Agent.Name), subagentsDir, id)
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	if err := os.MkdirAll(in, 0o700); err != nil {
		return "", err
	}
	staged, err := s.stageSubagentFiles(ctx, cfg, in, share, nil)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	run := &subagentRun{id: id, exact: exact, files: files, started: time.Now().UTC(), done: make(chan struct{}), helper: true}
	s.subagents.add(run)
	s.logSubagent(subagentLogLine{Event: "started", Run: id, Subagent: exact, GrantedBy: "helper", Files: files})
	s.mu.Lock()
	baseCtx := s.appCtx
	s.mu.Unlock()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	go s.runSubagent(baseCtx, run, SubagentRunSpec{Registry: it.Registry, Exact: exact, Task: task, Answers: given.intake, Files: staged, OutDir: out})
	return fmt.Sprintf("Started run %s of %s. It is an ordinary run on this computer; call run_result with this id to wait for its outcome.", id, exact), nil
}

// checkedAnswers are a run's answers as checked: the intake a run takes,
// the lines a question shows the person, and the addresses it fetches.
type checkedAnswers struct {
	intake  map[string]string
	lines   []string
	sources []string
}

// checkHelperAnswers checks an agent's answers against the questions: each
// names a question, fits its type, and is never one only the person
// answers. A file is never an answer here: a run takes files as files.
func checkHelperAnswers(u SubagentUsage, answers map[string]string) (*checkedAnswers, error) {
	byID := map[string]SubagentQuestion{}
	var ids []string
	for _, q := range u.Questions {
		byID[q.ID] = q
		ids = append(ids, q.ID)
	}
	given := make([]string, 0, len(answers))
	for id := range answers {
		given = append(given, id)
	}
	sort.Strings(given)
	out := &checkedAnswers{intake: map[string]string{}}
	for _, id := range given {
		q, ok := byID[id]
		if !ok {
			if len(ids) == 0 {
				return nil, fmt.Errorf("it asks no questions, so it takes no answer to %s", quoted(id))
			}
			return nil, fmt.Errorf("it asks no question %s: its questions are %s", quoted(id), strings.Join(ids, ", "))
		}
		v := strings.TrimSpace(answers[id])
		if v == "" {
			continue
		}
		if len(v) > helperMaxAnswer {
			return nil, fmt.Errorf("the answer to %s is longer than %d bytes", id, helperMaxAnswer)
		}
		switch q.Type {
		case "secret", "host":
			return nil, fmt.Errorf("only the person can answer %s (%s): a run of it is theirs to start", id, q.Type)
		case "file":
			return nil, fmt.Errorf("%s takes a file: give the file in files instead", id)
		case "choice":
			if !slices.Contains(q.Choices, v) {
				return nil, fmt.Errorf("%s is not one of %s's choices: %s", quoted(v), id, strings.Join(q.Choices, ", "))
			}
		case "repository", "url":
			if !strings.HasPrefix(v, "https://") {
				return nil, fmt.Errorf("the answer to %s is an https address", id)
			}
			out.sources = append(out.sources, id+"="+v)
		}
		out.intake[id] = v
		out.lines = append(out.lines, id+": "+quoted(cutText(v, 300)))
	}
	return out, nil
}

// agreedRuns is the record of the runs the person agreed to let the agent
// start.
type agreedRuns struct {
	Version int      `json:"version"`
	Runs    []string `json:"runs"`
}

// agreeToRun asks the person before the agent's first run of a QuickAgent
// at a version, and records a yes; a later run of the same version on the
// same addresses asks nothing.
func (s *Supervisor) agreeToRun(ctx context.Context, cfg Config, exact, task string, files []string, given *checkedAnswers) error {
	path := filepath.Join(Dir(s.opts.Home, s.opts.Agent.Name), agreedRunsFile)
	var rec agreedRuns
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the agent's own record
		_ = json.Unmarshal(b, &rec)
	}
	key := exact
	if len(given.sources) > 0 {
		src := slices.Clone(given.sources)
		slices.Sort(src)
		key += " " + strings.Join(src, " ")
	}
	if slices.Contains(rec.Runs, key) {
		return nil
	}
	who := s.asker(cfg)
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s asks to run %s on this computer", who, exact)
	if len(files) > 0 {
		fmt.Fprintf(&msg, " on %s from its share folder", strings.Join(files, ", "))
	}
	msg.WriteString(".")
	if len(given.lines) > 0 {
		fmt.Fprintf(&msg, " The answers %s gave its questions: %s.", who, strings.Join(given.lines, "; "))
	}
	if t := strings.TrimSpace(task); t != "" {
		fmt.Fprintf(&msg, " The task %s wrote for it: %s.", who, quoted(cutText(t, 500)))
	}
	msg.WriteString(" It runs as any run of it does, under what it was installed with. " + who + " may run this version again without asking. Run it?")
	if err := s.Ask(ctx, "Run "+exact+" for "+who+"?", msg.String()); err != nil {
		return err
	}
	rec.Version = 1
	rec.Runs = append(rec.Runs, key)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func helperInstalledList(ctx context.Context, ops HelperOps) (string, error) {
	items, err := ops.Installed(ctx)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "No QuickAgent is installed on this computer.", nil
	}
	var b strings.Builder
	b.WriteString("Installed on this computer:\n")
	for _, it := range items {
		fmt.Fprintf(&b, "- %s@%s", it.Ref, it.Version)
		if it.Local {
			b.WriteString(" (from a folder: only the person runs it)")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (s *Supervisor) helperScheduleAdd(ctx context.Context, cfg Config, ops HelperOps, ref, when, task string, answers map[string]string) (string, error) {
	base, _, err := parseHelperRef(ref)
	if err != nil {
		return "", err
	}
	it, err := helperInstalledHere(ctx, ops, base)
	switch {
	case err != nil:
		return "", err
	case it == nil:
		return "", fmt.Errorf("%s is not installed on this computer: install it with quickagent_install first", base)
	case it.Local:
		return "", fmt.Errorf("%s was installed from a folder: the person schedules it", base)
	case s.opts.Subagents == nil:
		return "", errors.New("this client makes no schedules for an agent")
	}
	w, err := ParseWhen(when, time.Now())
	if err != nil {
		return "", err
	}
	exact := base + "@" + it.Version
	usage, err := s.opts.Subagents.Check(ctx, it.Registry, exact)
	if err != nil {
		return "", fmt.Errorf("%s was not scheduled: %w", exact, err)
	}
	given, err := checkHelperAnswers(usage, answers)
	if err != nil {
		return "", err
	}
	who := s.asker(cfg)
	text := fmt.Sprintf("%s asks to schedule %s to run %s", who, exact, w.Describe())
	if t := strings.TrimSpace(task); t != "" {
		text += ", with the task " + quoted(cutText(t, 500))
	}
	if !opsAsk(ops) {
		if err := s.Ask(ctx, "Schedule "+exact+"?", text+". Each run is an ordinary run of it on this computer. Schedule it?"); err != nil {
			return "", err
		}
	}
	id, err := ops.AddSchedule(ctx, HelperScheduleSpec{Registry: it.Registry, Exact: exact, Task: strings.TrimSpace(task), Answers: given.intake, When: w}, who)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Scheduled %s: %s. The schedule's id is %s.", exact, w.Describe(), id), nil
}

func helperScheduleList(ctx context.Context, ops HelperOps) (string, error) {
	all, err := ops.Schedules(ctx)
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "No run is scheduled on this computer.", nil
	}
	var b strings.Builder
	b.WriteString("Scheduled on this computer:\n")
	for _, sc := range all {
		fmt.Fprintf(&b, "- %s: %s (%s), %s", sc.ID, quoted(sc.Name), sc.Ref, sc.When)
		switch {
		case sc.Ended:
			b.WriteString(", ended")
		case sc.Paused:
			b.WriteString(", paused")
		case sc.Next != "":
			fmt.Fprintf(&b, ", next %s", sc.Next)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// helperSchedule is the schedule with id.
func helperSchedule(ctx context.Context, ops HelperOps, id string) (HelperSchedule, error) {
	id = strings.TrimSpace(id)
	if !schedule.ValidID(id) {
		return HelperSchedule{}, fmt.Errorf("%s is not a schedule's id: schedule_list names them", quoted(id))
	}
	all, err := ops.Schedules(ctx)
	if err != nil {
		return HelperSchedule{}, err
	}
	for _, sc := range all {
		if sc.ID == id {
			return sc, nil
		}
	}
	return HelperSchedule{}, fmt.Errorf("no schedule %s on this computer", id)
}

// helperDocsPage is a path on the docs site.
var helperDocsPage = regexp.MustCompile(`^/[A-Za-z0-9/_.\-]*$`)

func helperDocs(ctx context.Context, ops HelperOps, page string) (string, error) {
	page = strings.TrimSpace(page)
	if page == "" {
		page = "/"
	}
	if !strings.HasPrefix(page, "/") {
		page = "/" + page
	}
	if len(page) > 200 || !helperDocsPage.MatchString(page) || strings.Contains(page, "..") || strings.Contains(page, "//") {
		return "", fmt.Errorf("%s is not a page of the docs: give its path, such as /agentry/desktop/", quoted(page))
	}
	ctx, cancel := context.WithTimeout(ctx, helperDocsTimeout)
	defer cancel()
	body, status, err := ops.Docs(ctx, page)
	if err != nil {
		return "", fmt.Errorf("the docs did not answer: %w", err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Sprintf("The docs have no page %s. docs_read of / lists the pages.", page), nil
	default:
		return "", fmt.Errorf("the docs answered %d for %s", status, page)
	}
	return fmt.Sprintf("%s%s (the page's text):\n\n%s", DocsBase, page, cutText(DocsText(body, page), helperMaxDocs)), nil
}
