// agentry agent: the persistent agents, built-in QuickAgents the runner
// keeps between conversations (pkg/runner/persistent).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/procx"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
	"github.com/blaktron/agentry-cli/pkg/tui"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Install, start, chat with and stop the persistent agents, which keep their conversation between starts",
		Long: "A persistent agent is a QuickAgent that the runner keeps between your\n" +
			"conversations: you start it, chat with it, stop it, and when you start it\n" +
			"again the conversation continues. Three are built into Agentry; `install`\n" +
			"adds a persistent QuickAgent from the registry, or your own folder, and\n" +
			"`uninstall` removes one. An installed agent is named by its reference\n" +
			"(ns/name), or by its name alone when no other agent has it. Its conversation lives on a\n" +
			"volume of its own, beside a second one made for its files; stopping it\n" +
			"keeps both and only `agentry agent reset` removes them. Each start is a\n" +
			"segment, with its own receipts and envelope in the agent's folder under\n" +
			"the runner home.\n\n" +
			"An agent runs in the background once started: `agentry agent start` returns\n" +
			"when it is up, `chat` attaches this terminal to it, and leaving the chat\n" +
			"leaves it running until `agentry agent stop`. It runs on the harness and\n" +
			"model your runner policy names, in a container, without the session caps\n" +
			"an interactive QuickAgent has. One process runs an agent at a time: this\n" +
			"CLI or Agentry Desktop, and the other is refused, naming the holder.\n\n" +
			"Each agent shares a folder with you, ~/agentry/<agent>share by default, read\n" +
			"and write, and keeps its own files on its data volume. What it has is its\n" +
			"roster: `show` lists it, `grant` switches an entry, `configure` adds a server\n" +
			"or a knowledge pack, names it and gives it a personality. A change reaches a\n" +
			"running agent at its next turn.\n\n" +
			"The Coding agent (coder) runs any program in its toolchain image, reaches\n" +
			"the internet and a browser, and deploys to a dev server you give it\n" +
			"(`configure coder --dev-host`); each is a roster entry you agree to at its\n" +
			"first start. It builds and runs software, which is riskier than an agent\n" +
			"that only reads and writes.\n\n" +
			"The Assistant knows the time at every turn, and registers its own tasks: an\n" +
			"instruction it acts on at the times a cron expression names. Its heartbeat\n" +
			"fires them while it runs, once you switch its heartbeat entry on; `tasks`\n" +
			"lists them and pauses or removes one.\n\n" +
			"Each agent reads skills, folders of instructions for kinds of work, at\n" +
			"skills/: its built-in ones, and your own from a folder or a .zip on this\n" +
			"computer (`agentry agent skill add`).\n\n" +
			"A persistent agent from the registry may have subagents: QuickAgents it\n" +
			"starts as runs of their own, each in its own sandbox under its own\n" +
			"contract, given only the task and the files it names. Those its release\n" +
			"declares are pinned when you install it; you add one installed here with\n" +
			"`configure --subagent`. Each is a roster entry, on until you switch it off,\n" +
			"with a second switch that asks you before each run. An agent that\n" +
			"declares the helper tools may also find, install, run and schedule\n" +
			"QuickAgents, asking you first. Agentry asks in the window attached to the\n" +
			"agent (`chat` takes /yes and /no); with none attached, nothing is done.\n\n" +
			"Agentry Helper (main) runs only in Agentry Desktop.\n\n" +
			"--on <runhost> runs list, show, start, stop, chat, attach, enable,\n" +
			"disable, reset, grant, configure, tasks and secret on a remote runhost,\n" +
			"where the agent then runs under the runhost's service, on the model the\n" +
			"runhost keeps (agentry runhost model <name> --provider <provider>). What it\n" +
			"is given for the first time is agreed to here. A stored secret's value, a\n" +
			"git host account's token, the Fastmail token and a calendar feed's address\n" +
			"(secret set, configure --account, --fastmail-token, --calendar-feed) are read\n" +
			"here and kept in the runhost's store, encrypted, once you agree; Microsoft\n" +
			"365 signs in by its device code. A flag that reads a file from this computer\n" +
			"or a sign-in whose browser answer goes into a container (Google Workspace's,\n" +
			"a calendar's) is refused with --on.\n\n" +
			"  agentry agent install acme/helper\n" +
			"  agentry agent enable assistant\n" +
			"  agentry agent start assistant\n" +
			"  agentry agent chat assistant\n" +
			"  agentry agent stop assistant",
	}
	cmd.AddCommand(
		newAgentListCmd(),
		newAgentShowCmd(),
		newAgentInstallCmd(),
		newAgentUninstallCmd(),
		newAgentStartCmd(),
		newAgentStopCmd(),
		newAgentChatCmd(),
		newAgentAttachCmd(),
		newAgentEnableCmd(true),
		newAgentEnableCmd(false),
		newAgentResetCmd(),
		newAgentGrantCmd(),
		newAgentConfigureCmd(),
		newAgentTasksCmd(),
		newAgentCompactCmd(),
		newAgentSkillCmd(),
		newAgentSecretCmd(),
		newAgentMessengerCmd(),
		newAgentMemoryCmd(),
		newAgentSuperviseCmd(),
	)
	wireAgentOn(cmd)
	return cmd
}

// agentNamed is the agent an argument names, a built-in or one installed in
// the runner home, refused with the usage class.
func agentNamed(name string) (persistent.Agent, error) {
	home, err := runnerHome()
	if err != nil {
		return persistent.Agent{}, err
	}
	a, err := persistent.LookupIn(home, name)
	if err != nil {
		return a, engine.Classify(engine.ExitUsage, err)
	}
	return a, nil
}

// cliAgent is an agent this CLI runs: a desktop-only one is refused.
func cliAgent(name string) (persistent.Agent, error) {
	a, err := agentNamed(name)
	if err != nil {
		return a, err
	}
	if a.DesktopOnly {
		return a, engine.Classify(engine.ExitUsage, fmt.Errorf("%s %w: open Agentry Desktop to use it", a.Title, persistent.ErrDesktopOnly))
	}
	return a, nil
}

// agentState is how an agent stands now, as list and show print it.
type agentState struct {
	Agent string `json:"agent"`
	// Ref is the agent's ns/name, and Version, Source, Persona, Packs and
	// Subagents what an installed agent runs: its version and where it came
	// from, the persona and the packs its release ships, and its subagents
	// as they were pinned at its install.
	Ref         string                   `json:"ref"`
	Version     string                   `json:"version,omitempty"`
	Source      string                   `json:"source,omitempty"`
	Persona     *persistent.Persona      `json:"persona,omitempty"`
	Packs       []string                 `json:"packs,omitempty"`
	Subagents   []persistent.SubagentPin `json:"subagents,omitempty"`
	DisplayName string                   `json:"displayName"`
	State       string                   `json:"state"`
	DesktopOnly bool                     `json:"desktopOnly,omitempty"`
	Holder      *persistent.Holder       `json:"holder,omitempty"`
	Status      *persistent.Status       `json:"status,omitempty"`
	Config      *persistent.Config       `json:"config,omitempty"`
	Usage       *persistent.Usage        `json:"usage,omitempty"`
	StateDir    string                   `json:"stateDir,omitempty"`
	Volumes     *agentVolumes            `json:"volumes,omitempty"`
	Transcript  []persistent.Entry       `json:"transcript,omitempty"`
	Share       string                   `json:"share,omitempty"`
	Roster      persistent.Roster        `json:"roster,omitempty"`
	// Tasks counts an agent's tasks, for one that keeps time.
	Tasks *int `json:"tasks,omitempty"`
	// Suggested are the servers the agent suggests and does not have.
	Suggested []suggestedServer `json:"suggested,omitempty"`
	// MailProviders are the services whose mail the agent can reach, for
	// one that reaches any.
	MailProviders []persistent.MailProviderState `json:"mailProviders,omitempty"`
	// CalendarFeeds are the calendars the agent reads, by name, never with
	// their addresses.
	CalendarFeeds []persistent.CalendarFeedState `json:"calendarFeeds,omitempty"`
	// Messengers are the messengers the agent is reached on, never with
	// their credentials, each with its connection while the agent runs.
	Messengers []persistent.MessengerState `json:"messengers,omitempty"`
	// LastCompaction is the newest record of the agent's compactions, and
	// LastConsolidation of its memory's, for one that keeps a memory.
	LastCompaction    *persistent.UpkeepRecord `json:"lastCompaction,omitempty"`
	LastConsolidation *persistent.UpkeepRecord `json:"lastConsolidation,omitempty"`
	// SubagentRuns are the runs it started of its subagents, newest first,
	// at most persistent.MaxSubagentRunsShown.
	SubagentRuns []persistent.SubagentRunRecord `json:"subagentRuns,omitempty"`
}

// suggestedServer is a server an agent suggests, as show lists it.
type suggestedServer struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	ID   string `json:"id"`
	Line string `json:"line"`
}

type agentVolumes struct {
	Session string `json:"session"`
	Data    string `json:"data"`
	Memory  string `json:"memory,omitempty"`
}

// The states list prints beside "running".
const (
	agentDesktopOnly = "desktop only"
	agentDisabled    = "disabled"
	agentStopped     = "stopped"
)

func readAgentState(ctx context.Context, home string, a persistent.Agent) (agentState, error) {
	st := agentState{Agent: a.Name, Ref: a.Ref, Version: a.Version, Source: a.Source, Persona: a.Persona, Packs: a.Packs, Subagents: a.Subagents, DisplayName: a.Title}
	if a.DesktopOnly {
		st.State, st.DesktopOnly = agentDesktopOnly, true
		return st, nil
	}
	cfg, err := persistent.LoadConfig(home, a.Name)
	if err != nil {
		return st, err
	}
	st.DisplayName = a.DisplayName(cfg)
	st.Config = &cfg
	st.Share, _ = persistent.ShareFolder(a, cfg)
	st.Roster = persistent.BuildRosterUnder(a, cfg, nil, persistent.HomeMalwareCheck(home))
	st.MailProviders = persistent.MailProviders(a, cfg)
	st.CalendarFeeds = persistent.CalendarFeeds(a, cfg)
	st.Messengers = persistent.Messengers(a, cfg, homeMessengerAllowed(home))
	for _, s := range persistent.Suggestions(a, cfg) {
		st.Suggested = append(st.Suggested, suggestedServer{Key: s.Key, Name: s.Name, ID: persistent.ServerEntryID(s.Server), Line: s.Note})
	}
	held, holder := persistent.Held(home, a.Name)
	switch {
	case held:
		st.State, st.Holder = persistent.StateRunning, holder
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if s, err := persistent.QueryStatus(qctx, home, a.Name); err == nil {
			st.Status = &s
			if len(s.Messengers) > 0 {
				st.Messengers = s.Messengers
			}
		}
		if r, err := persistent.QueryRoster(qctx, home, a.Name); err == nil {
			st.Roster = r
		}
	case cfg.Enabled:
		st.State = agentStopped
	default:
		st.State = agentDisabled
	}
	if a.KeepsTime() {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if ts, _, err := persistent.TasksOf(tctx, home, a); err == nil {
			n := len(ts)
			st.Tasks = &n
		}
	}
	return st, nil
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the persistent agents and whether each is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			var all []agentState
			for _, a := range persistent.AgentsIn(home) {
				st, err := readAgentState(cmd.Context(), home, a)
				if err != nil {
					return err
				}
				st.Config, st.Roster, st.Persona = nil, nil, nil
				all = append(all, st)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"agents": all})
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tSINCE\tSEGMENT")
			for _, st := range all {
				since, segment := "-", "-"
				state := st.State
				if st.Holder != nil {
					since = st.Holder.Since.Local().Format("2006-01-02 15:04")
					state += " (" + st.Holder.Where() + ")"
				}
				if st.Status != nil {
					segment = fmt.Sprint(st.Status.Segment)
				}
				name := st.name()
				if st.DisplayName != "" && !strings.EqualFold(st.DisplayName, name) && !strings.HasSuffix(name, "/"+st.DisplayName) {
					name += " (" + safeTerminalText(st.DisplayName, 64) + ")"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, state, since, segment)
			}
			return tw.Flush()
		},
	}
}

// agentShowLong is `agentry agent show`'s help: what it prints, and what each
// state of a roster entry means.
const agentShowLong = "Shows a persistent agent: whether it runs and which client runs it, its\n" +
	"segment, its share folder, its roster, its usage, its volumes and its folder.\n\n" +
	"Each roster entry has a state:\n\n" +
	"  on             the agent has it\n" +
	"  off            switched off\n" +
	"  pending        switched since the running segment started, or a server that\n" +
	"                 has finished installing: the next turn restarts the agent with\n" +
	"                 it and continues the conversation\n" +
	"  needs-sign-in  a server that signs in by OAuth, which an agent cannot be\n" +
	"                 given in this version\n" +
	"  installing     a server you added, being installed in the background; the\n" +
	"                 agent runs without it until it is ready\n" +
	"  failed         a server whose install failed; the line below it says why.\n" +
	"                 The agent runs without it. Switch it off and on to install it\n" +
	"                 again:\n" +
	"                   agentry agent grant <name> server:<id> off\n" +
	"                   agentry agent grant <name> server:<id> on\n\n" +
	"A package server with no network also has a line that says whether it sees\n" +
	"your share folder, with the id of that switch, <entry>:share (see\n" +
	"`agentry agent grant --help`).\n\n" +
	"\"compacted\" names the agent's last compaction (when, why, and whether a\n" +
	"fresh conversation started), and for an agent that keeps a memory \"tidied\"\n" +
	"names its last daily consolidation of the memory.\n\n" +
	"\"subagent runs\" lists the newest runs the agent started of its subagents, and\n" +
	"--json lists up to 50 as subagentRuns, each with the subagent's exact\n" +
	"version, when it started and ended, how it ended and the run's own id.\n\n" +
	"  agentry agent show assistant\n" +
	"  agentry agent show assistant --transcript 20"

func newAgentShowCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a persistent agent: its state, its segment, its usage and the end of its transcript",
		Long:  agentShowLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := agentNamed(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			st, err := readAgentState(cmd.Context(), home, a)
			if err != nil {
				return err
			}
			if !a.DesktopOnly {
				u, err := persistent.LoadUsage(home, a.Name)
				if err != nil {
					return err
				}
				st.Usage = &u
				st.StateDir = persistent.Dir(home, a.Name)
				st.Volumes = &agentVolumes{Session: persistent.SessionVolumeIn(home, a.Name), Data: persistent.DataVolumeIn(home, a.Name)}
				compaction, consolidation := persistent.LastUpkeep(home, a.Name)
				st.LastCompaction = compaction
				st.SubagentRuns = persistent.SubagentRuns(home, a.Name, persistent.MaxSubagentRunsShown)
				if a.KeepsMemory() {
					st.Volumes.Memory = persistent.MemoryVolumeIn(home, a.Name)
					st.LastConsolidation = consolidation
				}
				if lines > 0 {
					if st.Transcript, err = persistent.Transcript(home, a.Name, lines); err != nil {
						return err
					}
				}
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), st)
			}
			printAgentState(cmd.OutOrStdout(), st)
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "transcript", 0, "also print the last N lines of the agent's transcript")
	return cmd
}

// name is how list and show name the agent: a built-in by its short name,
// an installed agent by its reference.
func (st agentState) name() string {
	if st.Source != "" {
		return st.Ref
	}
	return st.Agent
}

func printAgentState(w io.Writer, st agentState) {
	_, _ = fmt.Fprintf(w, "%s (%s): %s\n", safeTerminalText(st.DisplayName, 64), st.name(), st.State)
	if st.DesktopOnly {
		_, _ = fmt.Fprintln(w, "It runs only in Agentry Desktop.")
		return
	}
	switch st.Source {
	case persistent.SourceRegistry:
		_, _ = fmt.Fprintf(w, "  release:    %s@%s, from the registry\n", st.Ref, st.Version)
	case persistent.SourceLocal:
		_, _ = fmt.Fprintf(w, "  release:    %s@%s, from a folder on this computer (unsigned)\n", st.Ref, st.Version)
	}
	if p := st.Persona; p != nil {
		lock := "you may replace its personality"
		if !p.Editable {
			lock = "its personality is locked by its publisher"
		}
		_, _ = fmt.Fprintf(w, "  persona:    %s, %d bytes; %s\n", safeTerminalText(p.Name, 64), len(p.Text), lock)
	}
	if len(st.Packs) > 0 {
		_, _ = fmt.Fprintf(w, "  ships:      %s\n", strings.Join(st.Packs, ", "))
	}
	for _, sa := range st.Subagents {
		if sa.Version != "" {
			_, _ = fmt.Fprintf(w, "  subagent:   %s@%s (%s): %s\n", sa.Ref, sa.Version, sa.Constraint, safeTerminalText(sa.Why, 200))
		} else {
			_, _ = fmt.Fprintf(w, "  subagent:   %s (%s), resolved to no version: %s\n", sa.Ref, sa.Constraint, safeTerminalText(sa.Unresolved, 300))
		}
	}
	if h := st.Holder; h != nil {
		_, _ = fmt.Fprintf(w, "  run by:     %s (pid %d), since %s\n", h.Where(), h.PID, h.Since.Local().Format("2006-01-02 15:04"))
	}
	if s := st.Status; s != nil {
		_, _ = fmt.Fprintf(w, "  segment:    %d (run %s), on %s with %s\n", s.Segment, s.Run, s.Adapter, s.Model)
		if s.Resumed {
			_, _ = fmt.Fprintln(w, "  continued:  the conversation of the segment before")
		} else {
			_, _ = fmt.Fprintf(w, "  continued:  no: %s\n", s.ResumeReason)
		}
		_, _ = fmt.Fprintf(w, "  this segment: %d turn(s), %d tool call(s), %d token(s)\n", s.Usage.UserTurns, s.Usage.Turns, s.Usage.InputTokens+s.Usage.OutputTokens+s.Usage.TotalTokens)
	}
	if c := st.Config; c != nil {
		_, _ = fmt.Fprintf(w, "  enabled:    %t; segments started: %d\n", c.Enabled, c.Segments)
		if c.Personality != "" {
			_, _ = fmt.Fprintf(w, "  personality: %d bytes, given at its next turn when changed\n", len(c.Personality))
		}
	}
	if st.Share != "" {
		_, _ = fmt.Fprintf(w, "  share:      %s\n", st.Share)
	}
	if st.Tasks != nil {
		_, _ = fmt.Fprintf(w, "  tasks:      %d (agentry agent tasks %s)\n", *st.Tasks, st.Agent)
	}
	if r := st.LastCompaction; r != nil {
		_, _ = fmt.Fprintf(w, "  compacted:  %s\n", upkeepLine(*r))
	}
	if r := st.LastConsolidation; r != nil {
		_, _ = fmt.Fprintf(w, "  tidied:     %s\n", upkeepLine(*r))
	}
	if s := st.Status; s != nil && s.Pending {
		_, _ = fmt.Fprintln(w, "  pending:    a change restarts it at its next turn")
	}
	printSubagentRuns(w, st.SubagentRuns)
	if len(st.Roster) > 0 {
		_, _ = fmt.Fprintln(w, "  roster:")
		printRoster(w, st.Roster)
	}
	if len(st.MailProviders) > 0 {
		_, _ = fmt.Fprintf(w, "  mail providers (agentry agent configure %s --mail-provider <key>):\n", st.Agent)
		printMailProviders(w, st.MailProviders)
	}
	if len(st.CalendarFeeds) > 0 {
		_, _ = fmt.Fprintf(w, "  calendar feeds (agentry agent configure %s --calendar-feed <name>):\n", st.Agent)
		for _, f := range st.CalendarFeeds {
			from := f.Host
			if f.Service != "" {
				from = f.Service + ", " + f.Host
			}
			state := "off"
			if f.On {
				state = "on"
			}
			_, _ = fmt.Fprintf(w, "    %s\t%s (%s): %s, address set %s\n", safeTerminalText(f.Name, 80), f.Entry, from, state, f.Set.Local().Format("2 Jan 2006 15:04"))
		}
	}
	if len(st.Messengers) > 0 {
		_, _ = fmt.Fprintf(w, "  messengers (agentry agent messenger list %s):\n", st.Agent)
		a, _ := agentNamed(st.Agent)
		printMessengers(w, a, st.Messengers)
	}
	if len(st.Suggested) > 0 {
		_, _ = fmt.Fprintf(w, "  suggested servers (agentry agent configure %s --add-suggested <key>):\n", st.Agent)
		for _, s := range st.Suggested {
			_, _ = fmt.Fprintf(w, "    %s\t%s: %s\n", s.Key, s.Name, safeTerminalText(s.Line, 300))
		}
	}
	if u := st.Usage; u != nil {
		_, _ = fmt.Fprintf(w, "  lifetime:   %d segment(s) ended, %d turn(s), %d token(s)\n", u.Segments, u.UserTurns, u.InputTokens+u.OutputTokens+u.TotalTokens)
	}
	if v := st.Volumes; v != nil {
		if v.Memory != "" {
			_, _ = fmt.Fprintf(w, "  volumes:    %s, %s, %s\n", v.Session, v.Data, v.Memory)
		} else {
			_, _ = fmt.Fprintf(w, "  volumes:    %s, %s\n", v.Session, v.Data)
		}
	}
	if st.StateDir != "" {
		_, _ = fmt.Fprintf(w, "  folder:     %s\n", st.StateDir)
	}
	for _, e := range st.Transcript {
		text := strings.TrimSpace(e.Text)
		_, _ = fmt.Fprintf(w, "  [%d] %s: %s\n", e.Segment, e.Kind, safeTerminalText(text, 400))
	}
}

// subagentRunsPrinted is how many of an agent's subagent runs show prints;
// --json lists them all.
const subagentRunsPrinted = 5

// printSubagentRuns prints the newest of an agent's subagent runs.
func printSubagentRuns(w io.Writer, runs []persistent.SubagentRunRecord) {
	if len(runs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  subagent runs: %d, the newest first (all of them with --json):\n", len(runs))
	for _, r := range runs[:min(len(runs), subagentRunsPrinted)] {
		outcome := r.Outcome
		switch {
		case outcome == "" && r.Ended.IsZero():
			outcome = "not ended"
		case r.Error != "":
			outcome += ": " + safeTerminalText(r.Error, 200)
		}
		_, _ = fmt.Fprintf(w, "    %s\t%s, started %s: %s\n", r.ID, r.Subagent, r.Started.Local().Format("2006-01-02 15:04"), outcome)
	}
}

// agentPolicy is the runner policy an agent runs under.
func agentPolicy(path string) (*policy.Policy, string, policy.File, error) {
	file, err := policy.Resolve(path)
	if err != nil {
		return nil, "", file, err
	}
	pol, sha, err := policy.EnsureDefault(file.Path)
	if err != nil {
		return nil, "", file, engine.Classify(engine.ExitPolicy, err)
	}
	return pol, sha, file, nil
}

// agentStartTimeout bounds how long start waits for the agent to come up:
// a first start may build the runner's images.
var agentStartTimeout = 20 * time.Minute

func newAgentStartCmd() *cobra.Command {
	var (
		policyPath string
		yes        bool
	)
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a persistent agent in the background; its conversation continues from where it stopped",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			cfg, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return err
			}
			if !cfg.Enabled {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not enabled: agentry agent enable %s", a.Name, a.Name))
			}
			if held, h := persistent.Held(home, a.Name); held {
				return engine.Classify(engine.ExitUsage, &persistent.HeldError{Agent: a.Name, Holder: h})
			}
			pol, _, _, err := agentPolicy(policyPath)
			if err != nil {
				return err
			}
			// What the agent is given that the person has not agreed to on
			// this computer is asked for here, on the terminal: the
			// supervisor has none, and refuses to start without it.
			if pending := persistent.Unconsented(persistent.BuildRosterUnder(a, cfg, nil, pol.MalwareCheckOn()), cfg); len(pending) > 0 {
				if err := agentConsent(cmd, a, &persistent.ConsentError{Agent: a.Name, Entries: pending}, yes); err != nil {
					return err
				}
				if err := persistent.AgreeToRoster(home, a, persistent.ClientCLI); err != nil {
					return engine.Classify(engine.ExitCLI, err)
				}
			}
			if err := dockerx.New().Available(cmd.Context()); err != nil {
				return engine.Classify(engine.ExitCLI, fmt.Errorf("a persistent agent runs in containers, and the container engine is not available: %w", err))
			}
			return startSupervisor(cmd, home, a, policyPath)
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what the agent is given for the first time without being asked")
	return cmd
}

// startSupervisor launches `agentry agent supervise` detached, its output in
// the agent's log, and waits until its socket answers, printing the log as
// it grows.
func startSupervisor(cmd *cobra.Command, home string, a persistent.Agent, policyPath string) error {
	errOut := cmd.ErrOrStderr()
	exe, err := os.Executable()
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := os.MkdirAll(persistent.Dir(home, a.Name), 0o700); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	logPath := persistent.LogPath(home, a.Name)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the runner home's own file
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	_ = logf.Chmod(0o600) // a log an earlier start left keeps its mode otherwise
	argv := []string{"agent", "supervise", a.Name}
	if policyPath != "" {
		abs, err := filepath.Abs(policyPath)
		if err != nil {
			_ = logf.Close()
			return engine.Classify(engine.ExitUsage, err)
		}
		argv = append(argv, "--policy", abs)
	}
	child := exec.Command(exe, argv...) //nolint:gosec // this binary, re-run
	child.Stdout, child.Stderr = logf, logf
	procx.Detach(child)
	procx.NoWindow(child)
	if err := child.Start(); err != nil {
		_ = logf.Close()
		return engine.Classify(engine.ExitCLI, fmt.Errorf("start the %s agent's supervisor: %w", a.Name, err))
	}
	_ = logf.Close()
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	_, _ = fmt.Fprintf(errOut, "Starting the %s agent (its log: %s)\n", a.Name, logPath)
	follow := &logFollower{path: logPath, w: errOut}
	deadline := time.Now().Add(agentStartTimeout)
	for {
		follow.flush()
		qctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		st, qerr := persistent.QueryStatus(qctx, home, a.Name)
		cancel()
		if qerr == nil {
			follow.flush()
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), st)
			}
			printStarted(cmd.OutOrStdout(), a, st)
			return nil
		}
		select {
		case err := <-exited:
			follow.flush()
			why := "it exited"
			if err != nil {
				why = err.Error()
			}
			return engine.Classify(engine.ExitCLI, fmt.Errorf("the %s agent did not start (%s); its log is %s", a.Name, why, logPath))
		case <-cmd.Context().Done():
			return engine.Classify(engine.ExitCLI, fmt.Errorf("interrupted; the %s agent may still be starting: agentry agent show %s", a.Name, a.Name))
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return engine.Classify(engine.ExitTimeout, fmt.Errorf("the %s agent did not answer within %s; its log is %s", a.Name, agentStartTimeout, logPath))
		}
	}
}

func printStarted(w io.Writer, a persistent.Agent, st persistent.Status) {
	_, _ = fmt.Fprintf(w, "The %s agent is running: segment %d, on %s with %s.\n", a.Name, st.Segment, st.Adapter, st.Model)
	if st.Resumed {
		_, _ = fmt.Fprintln(w, "Its conversation continues from the segment before.")
	} else {
		_, _ = fmt.Fprintf(w, "A new conversation: %s.\n", st.ResumeReason)
	}
	_, _ = fmt.Fprintf(w, "Chat with it: agentry agent chat %s%s\nStop it:      agentry agent stop %s%s\n", a.Name, onThisRunhost(), a.Name, onThisRunhost())
}

// logFollower prints what a file gained since the last flush.
type logFollower struct {
	path string
	w    io.Writer
	off  int64
}

func (f *logFollower) flush() {
	fh, err := os.Open(f.path)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return
	}
	n, _ := io.Copy(f.w, fh)
	f.off += n
}

// stageLines writes each stage a segment's start reaches as a "stage:" line
// of the supervisor's log, in the words a person is shown, which agentry
// agent start prints as the log grows.
func stageLines(w io.Writer) func(persistent.Stage) {
	return func(st persistent.Stage) {
		_, _ = fmt.Fprintf(w, "stage: %s\n", st.Words())
	}
}

func newAgentSuperviseCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:    "supervise <name>",
		Short:  "Run a persistent agent's supervisor in the foreground (what agentry agent start launches)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			pol, sha, file, err := agentPolicy(policyPath)
			if err != nil {
				return err
			}
			errOut := cmd.ErrOrStderr()
			// A signal while the segment starts (a first start builds images)
			// cancels the start, which tears down what it made; once it runs,
			// a signal stops it as a stop command does. The segment lives
			// until then, not until a context of this command's ends.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer signal.Stop(sig)
			startCtx, cancelStart := context.WithCancel(context.Background())
			starting := make(chan struct{})
			go func() {
				select {
				case s := <-sig:
					_, _ = fmt.Fprintf(errOut, "%s: the %s agent's start is cancelled\n", s, a.Name)
					cancelStart()
				case <-starting:
				}
			}()
			sv, err := persistent.Start(startCtx, persistent.Options{
				Home: home, Agent: a, Client: persistent.ClientCLI,
				Policy: pol, PolicySHA: sha, PolicyFile: &file, Build: version.Build(),
				Docker: dockerx.New(), Stderr: errOut, Packs: agentPacks(cmd), Voice: agentVoice(cmd),
				OnStage: stageLines(errOut), Credits: creditsClient(pol),
				Subagents: cliSubagents{policyPath: policyPath}, Helper: cliHelper{policyPath: policyPath},
			})
			close(starting)
			if err != nil {
				cancelStart()
				return err
			}
			st := sv.Status()
			_, _ = fmt.Fprintf(errOut, "supervising the %s agent: segment %d, run %s, pid %d\n", a.Name, st.Segment, st.Run, st.PID)
			select {
			case <-sv.Done():
			case s := <-sig:
				_, _ = fmt.Fprintf(errOut, "%s: stopping the %s agent\n", s, a.Name)
			}
			env, err := sv.Stop(context.Background())
			if env != nil {
				_, _ = fmt.Fprintf(errOut, "the segment ended (%s); its receipts: %s\n", env.Status, env.OutDir)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	return cmd
}

func newAgentStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a persistent agent; its conversation and files are kept for the next start",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if held, _ := persistent.Held(home, a.Name); !held {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running", a.Name))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			st, err := persistent.RequestStop(ctx, home, a.Name)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), st)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The %s agent stopped. The segment's envelope: %s\n", a.Name, st.ResultFile)
			if st.Error != "" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the segment ended with an error: %s\n", st.Error)
			}
			return nil
		},
	}
}

func newAgentChatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat <name>",
		Short: "Chat with a running persistent agent; leaving the chat leaves the agent running",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			qctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			st, err := persistent.QueryStatus(qctx, home, a.Name)
			cancel()
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running: agentry agent start %s", a.Name, a.Name))
			}
			entries, err := persistent.Transcript(home, a.Name, 500)
			if err != nil {
				return err
			}
			history := persistent.HistoryEvents(entries)
			s, err := persistent.Attach(cmd.Context(), home, a.Name, history)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			cfg, _ := persistent.LoadConfig(home, a.Name)
			_, uiErr := runChat(cmd.Context(), s, tui.Options{
				Ref:     a.PackageRef() + " · " + safeTerminalText(a.DisplayName(cfg), 64),
				Model:   st.Model,
				HasTask: true,
			}, cmd.InOrStdin(), cmd.OutOrStdout())
			_, _ = s.End(context.Background(), false)
			if ended, reason := s.Ended(); ended && reason != "" && reason != "cancelled" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The %s agent's segment ended (%s).\n", a.Name, reason)
			} else {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The %s agent keeps running. Stop it: agentry agent stop %s\n", a.Name, a.Name)
			}
			if uiErr != nil {
				return engine.Classify(engine.ExitCLI, uiErr)
			}
			return nil
		},
	}
}

func newAgentAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <name> --json",
		Short: "Drive a running persistent agent from a program: JSON-line commands on stdin, its events on descriptor 3",
		Long: "attach is agent chat for a program. It speaks the session protocol of\n" +
			"`agentry run --session-protocol`: one JSON command a line on stdin\n" +
			"({\"send\":{\"text\":\"…\"}} or {\"end\":{\"finish\":false}}), and the agent's\n" +
			"events as JSON lines on descriptor 3, which the driver opens. A send while\n" +
			"another chat's turn is in flight is refused as busy. Ending, or the end of\n" +
			"input, detaches: the agent keeps running. A persistent agent takes no files\n" +
			"through its chat; put them in its share folder. Descriptor 3 does not exist\n" +
			"on Windows, so attach is refused there.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !flags.jsonOut {
				return engine.Classify(engine.ExitUsage, errors.New("attach is for a program, and speaks JSON only: pass --json (a person chats with agentry agent chat)"))
			}
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			qctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			_, err = persistent.QueryStatus(qctx, home, a.Name)
			cancel()
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running: agentry agent start %s", a.Name, a.Name))
			}
			w, err := protocolEventsOut()
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			ignoreBrokenPipes()
			s, err := persistent.Attach(cmd.Context(), home, a.Name, nil)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			errOut := cmd.ErrOrStderr()
			_, err = serveProtocol(cmd.Context(), s, cmd.InOrStdin(), w, protocol.Options{
				OnDropped: func() {
					_, _ = fmt.Fprintln(errOut, "agent attach: the events reader stopped taking events; the rest are in the agent's transcript")
				},
			})
			if ended, reason := s.Ended(); ended && reason != "" && reason != session.EndCancelled {
				_, _ = fmt.Fprintf(errOut, "The %s agent's segment ended (%s).\n", a.Name, reason)
			}
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			return nil
		},
	}
}

func newAgentEnableCmd(on bool) *cobra.Command {
	use, short := "enable <name>", "Enable a persistent agent, so it can be started"
	if !on {
		use, short = "disable <name>", "Disable a persistent agent; its conversation and files are kept"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if held, _ := persistent.Held(home, a.Name); held && !on {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running: stop it first (agentry agent stop %s)", a.Name, a.Name))
			}
			cfg, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return err
			}
			cfg.Enabled = on
			if err := persistent.SaveConfig(home, a.Name, cfg); err != nil {
				return err
			}
			if on {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The %s agent is enabled. Start it: agentry agent start %s%s\n", a.Name, a.Name, onThisRunhost())
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The %s agent is disabled. Its conversation and files are kept.\n", a.Name)
			}
			return nil
		},
	}
}

// agentDocker is the container engine reset removes the volumes from.
var agentDocker = func() dockerx.Client { return dockerx.New() }

// agentReset is one of the resets agentry agent reset runs: what its prompt
// says (the agent's name, then what it wipes), what it says when done (the
// agent's name), and the reset.
type agentReset struct {
	prompt, done string
	reset        func(context.Context, string, string, dockerx.Client) error
}

const (
	resetAll = iota
	resetHistory
	resetKeepCredentials
)

var agentResets = [...]agentReset{
	resetAll: {
		prompt: "This puts the %s agent back to its defaults: it wipes %s and everything you chose for it, with the secrets and sign-ins you gave it.",
		done:   "The %s agent was reset to its defaults: its next start begins a new conversation and asks you again to agree to what it does.",
		reset:  persistent.Reset,
	},
	resetHistory: {
		prompt: "This wipes the %s agent's history: %s, its transcript and tasks. It keeps everything you chose and agreed to, its secrets and its sign-ins.",
		done:   "The %s agent's history was wiped: its next start begins a new conversation, with everything you chose and agreed to.",
		reset:  persistent.ResetHistory,
	},
	resetKeepCredentials: {
		prompt: "This puts the %s agent back to its defaults: it wipes %s and everything you chose for it. It keeps the secrets you stored and the sign-ins of its servers, switched off.",
		done:   "The %s agent was reset to its defaults, its secrets and sign-ins kept and switched off: its next start begins a new conversation and asks you again to agree to what it does.",
		reset:  persistent.ResetAgent,
	},
}

func newAgentResetCmd() *cobra.Command {
	var confirm string
	var history, keepCredentials bool
	cmd := &cobra.Command{
		Use:   "reset <name>",
		Short: "Put a persistent agent back to its defaults; the files in its share folder are kept",
		Long: "Reset removes the agent's volumes (its conversation, its files and, for the\n" +
			"Assistant, its memory: export it first with agentry agent memory export) and\n" +
			"its transcript, and everything you chose for it: its name and personality,\n" +
			"the servers you added and their installed copies, its knowledge packs and\n" +
			"switches, what you agreed to, its share folder's location, its dev server and\n" +
			"ports, the secrets you stored for it (removed from your keychain too), its\n" +
			"sign-ins, and whether it starts with Agentry. Its next start begins a new\n" +
			"conversation and asks you again to agree to what it does.\n\n" +
			"The Assistant has two smaller resets:\n" +
			"  --history           removes its conversation, transcript, tasks, memory and\n" +
			"                      files, and what it read from its share folder; it keeps\n" +
			"                      everything you chose and agreed to, its secrets and its\n" +
			"                      sign-ins.\n" +
			"  --keep-credentials  removes all that and puts everything you chose back to\n" +
			"                      its default, but keeps the secrets you stored and the\n" +
			"                      sign-ins of its servers (Google Workspace's included),\n" +
			"                      switched off: turning one on asks you again and needs no\n" +
			"                      new sign-in. Its share folder's location stays.\n" +
			"Both keep its usage. A kept secret goes with agentry agent secret remove, and\n" +
			"every kept credential with a reset without either flag.\n\n" +
			"Kept by every reset: whether the agent is on, the receipts of its segments,\n" +
			"every file in its share folder, your model sign-ins, and a dev server's\n" +
			"sign-in in your keychain. Type the agent's name when asked, or pass it as\n" +
			"--confirm when there is no terminal to ask on.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			if history && keepCredentials {
				return engine.Classify(engine.ExitUsage, errors.New("--history and --keep-credentials are two different resets: choose one"))
			}
			if (history || keepCredentials) && !a.Builtin.ResetsInParts() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has one reset: --history and --keep-credentials are the Assistant's", a.Name))
			}
			r := agentResets[resetAll]
			switch {
			case history:
				r = agentResets[resetHistory]
			case keepCredentials:
				r = agentResets[resetKeepCredentials]
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if held, _ := persistent.Held(home, a.Name); held {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running: stop it first (agentry agent stop %s)", a.Name, a.Name))
			}
			if confirm == "" {
				if !stdinIsTTY(cmd.InOrStdin()) {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("reset asks you to type the agent's name, and there is no terminal: pass --confirm %s", a.Name))
				}
				wipes := "its conversation, its files"
				if a.KeepsMemory() {
					wipes += ", its memory"
				}
				export := ""
				if a.KeepsMemory() {
					export = fmt.Sprintf(" To keep its memory, export it first: agentry agent memory export %s <folder>.", a.Name)
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s The files in its share folder are kept.%s Type its name (%s) to go on: ", fmt.Sprintf(r.prompt, a.Name, wipes), export, a.Name)
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				confirm = strings.TrimSpace(line)
			}
			if confirm != a.Name {
				return engine.Classify(engine.ExitUsage, errors.New("the name did not match; nothing was reset"))
			}
			d := agentDocker()
			if err := r.reset(cmd.Context(), home, a.Name, d); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), r.done+"\n", a.Name)
			if shared := persistent.SharedVolumes(cmd.Context(), d, a.Name); len(shared) > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Volumes from before each runner home had its own still exist on this engine (%s); they may hold the conversation of this or another runner home. Remove them with docker volume rm once no runner home needs them.\n", strings.Join(shared, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "the agent's name, to confirm without a terminal")
	cmd.Flags().BoolVar(&history, "history", false, "the Assistant: wipe its history alone, keeping everything you chose, its secrets and its sign-ins")
	cmd.Flags().BoolVar(&keepCredentials, "keep-credentials", false, "the Assistant: put it back to its defaults, keeping the secrets you stored and its servers' sign-ins, switched off")
	return cmd
}

// upkeepLine is one upkeep record as show prints it: when, why, and what
// came of it.
func upkeepLine(r persistent.UpkeepRecord) string {
	at := r.Time.Local().Format("2006-01-02 15:04")
	facts := ""
	if r.FactsBefore >= 0 && r.FactsAfter >= 0 {
		facts = fmt.Sprintf("; %d fact lines before, %d after", r.FactsBefore, r.FactsAfter)
	}
	switch {
	case r.Trigger == persistent.UpkeepConsolidate && r.Reason == "":
		return at + facts
	case r.Swap == persistent.SwapInPlace && r.Reason == "":
		return fmt.Sprintf("%s (%s), compacted in place by the running harness", at, r.Trigger)
	case r.Swapped:
		how := "a new segment"
		if r.Swap == persistent.SwapInSegment {
			how = "the running segment"
		}
		return fmt.Sprintf("%s (%s), a fresh conversation in %s%s", at, r.Trigger, how, facts)
	case r.Trigger == persistent.UpkeepConsolidate:
		return fmt.Sprintf("%s, not done: %s", at, r.Reason)
	default:
		return fmt.Sprintf("%s (%s), not done: %s", at, r.Trigger, r.Reason)
	}
}
