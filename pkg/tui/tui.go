// Package tui is the terminal chat for an interactive run: one full-screen
// conversation over a session.Session — transcript pane, input line, status
// line, and the slash commands /add, /finish, /quit, /caps. It is a view:
// every message goes through Session.Send, every file through
// Session.AddInput, and the engine's budgets end the session whatever the
// terminal does.
package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Options describe the run to the status line and /caps.
type Options struct {
	// Ref is namespace/name@version.
	Ref string
	// Model is the provider/model string.
	Model string
	// Caps are the enforced capability lines shown by /caps.
	Caps []string
	// Budgets are the session caps; MaxTurns and MaxCostUSD the run's.
	Budgets    spec.SessionBudgets
	MaxTurns   int
	MaxCostUSD float64
	// PlanRun says the run is paid by the person's own plan (a model
	// sign-in): the status line shows no dollars.
	PlanRun bool
	// Opening is the manifest's opening line, shown when the session
	// starts without a task.
	Opening string
	// HasTask says a task text was given (it arrives as the first user turn).
	HasTask bool
}

// Outcome is how the user left the chat.
type Outcome struct {
	// Finish asks the agent for its final result before teardown.
	Finish bool
	// Reason is set when the engine ended the session, not the user.
	Reason session.EndReason
}

// Run shows the chat until the user ends it or the session ends, then
// returns how. It never calls End: the caller does, so the envelope and
// receipts are written after the screen is restored.
func Run(ctx context.Context, s session.Session, opts Options, in io.Reader, out io.Writer) (Outcome, error) {
	m := newModel(s, opts)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil && ctx.Err() == nil {
		return Outcome{}, err
	}
	fm, _ := final.(model)
	return fm.outcome, nil
}

// Messages.
type (
	sessionEventMsg session.Event
	eventsClosedMsg struct{}
	turnDoneMsg     struct{ err error }
	inputDoneMsg    struct {
		path string
		err  error
	}
	tickMsg time.Time
)

type model struct {
	s    session.Session
	opts Options

	vp    viewport.Model
	input textinput.Model
	ready bool
	width int

	lines     []string
	streaming strings.Builder // assistant text of the turn in progress
	busy      bool
	outcome   Outcome
	ended     bool
	quitting  bool

	userTurns int
	toolCalls int
	costUSD   float64
	lastTurn  time.Time
	notice    string

	styles styles
}

type styles struct {
	user, assistant, runner, tool, dim, status, warn lipgloss.Style
}

func newStyles() styles {
	return styles{
		user:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#1d4ed8", Dark: "#93c5fd"}),
		assistant: lipgloss.NewStyle(),
		runner:    lipgloss.NewStyle().Italic(true).Foreground(lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#9ca3af"}),
		tool:      lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#9ca3af"}),
		dim:       lipgloss.NewStyle().Faint(true),
		status:    lipgloss.NewStyle().Reverse(true),
		warn:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#fbbf24"}),
	}
}

func newModel(s session.Session, opts Options) model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "message the agent, or /help"
	ti.CharLimit = 8000
	ti.Focus()
	m := model{s: s, opts: opts, input: ti, styles: newStyles(), lastTurn: time.Now()}
	if !opts.HasTask && strings.TrimSpace(opts.Opening) != "" {
		m.lines = append(m.lines, m.styles.assistant.Render("Agent: "+opts.Opening))
	}
	m.lines = append(m.lines, m.styles.dim.Render("Commands: /add <path>  /finish  /quit  /caps  /help"))
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, waitEvent(m.s), tick())
}

func waitEvent(s session.Session) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.Events()
		if !ok {
			return eventsClosedMsg{}
		}
		return sessionEventMsg(ev)
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		h := msg.Height - 3 // input + status + spacer
		if h < 3 {
			h = 3
		}
		if !m.ready {
			m.vp = viewport.New(msg.Width, h)
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = msg.Width, h
		}
		m.input.Width = msg.Width - 4
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.SetValue("")
			return m.submit(text)
		case tea.KeyPgUp, tea.KeyPgDown, tea.KeyUp, tea.KeyDown:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case sessionEventMsg:
		m.apply(session.Event(msg))
		if m.ended {
			return m, tea.Quit
		}
		return m, waitEvent(m.s)

	case eventsClosedMsg:
		m.ended = true
		return m, tea.Quit

	case turnDoneMsg:
		m.busy = false
		if msg.err != nil && msg.err != session.ErrEnded {
			m.appendLine(m.styles.warn.Render("error: " + msg.err.Error()))
		}
		return m, nil

	case inputDoneMsg:
		if msg.err != nil {
			m.appendLine(m.styles.warn.Render("could not add " + msg.path + ": " + msg.err.Error()))
		}
		return m, nil

	case tickMsg:
		return m, tick()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit handles one line from the input: a slash command or a turn.
func (m model) submit(text string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(text, "/") {
		cmdName, arg, _ := strings.Cut(text, " ")
		arg = strings.TrimSpace(arg)
		switch cmdName {
		case "/finish":
			m.outcome.Finish = true
			m.quitting = true
			return m, tea.Quit
		case "/quit", "/exit":
			m.quitting = true
			return m, tea.Quit
		case "/caps":
			m.appendLine(m.styles.dim.Render("Enforced capabilities:"))
			for _, c := range m.opts.Caps {
				m.appendLine(m.styles.dim.Render("  " + c))
			}
			return m, nil
		case "/help":
			m.appendLine(m.styles.dim.Render("/add <path> — give the agent a file (read-only under /inputs); /finish — ask for the final result and end; /quit — end without a result; /caps — the enforced contract"))
			return m, nil
		case "/add":
			if arg == "" {
				m.appendLine(m.styles.warn.Render("usage: /add <path>"))
				return m, nil
			}
			s := m.s
			return m, func() tea.Msg {
				return inputDoneMsg{path: arg, err: s.AddInput(context.Background(), arg)}
			}
		default:
			m.appendLine(m.styles.warn.Render("unknown command " + cmdName + " (try /help)"))
			return m, nil
		}
	}
	if m.busy {
		m.notice = "the agent is still answering — wait for its turn to end"
		return m, nil
	}
	m.busy = true
	m.notice = ""
	s := m.s
	return m, func() tea.Msg { return turnDoneMsg{err: s.Send(context.Background(), text)} }
}

// apply renders one session event into the transcript.
func (m *model) apply(ev session.Event) {
	switch ev.Kind {
	case session.KindUserTurn:
		m.flushStreaming()
		if ev.Synthetic {
			m.appendLine(m.styles.runner.Render("Runner: " + ev.Text))
		} else {
			m.userTurns++
			m.appendLine(m.styles.user.Render("You: ") + ev.Text)
		}
		m.lastTurn = time.Now()
	case session.KindAssistantText:
		m.streaming.WriteString(ev.Text)
		m.refresh()
	case session.KindAssistantTurn:
		m.streaming.Reset()
		m.appendLine(m.styles.assistant.Render("Agent: ") + strings.TrimSpace(ev.Text))
	case session.KindToolCall:
		m.toolCalls++
		m.appendLine(m.styles.tool.Render("  · tool: " + ev.Text))
	case session.KindPermissionGranted:
		m.appendLine(m.styles.tool.Render("  · permission granted: " + ev.Text))
	case session.KindPermissionDenied:
		m.appendLine(m.styles.tool.Render("  · permission request denied: " + ev.Text))
	case session.KindInputAdded:
		m.appendLine(m.styles.tool.Render("  · input added: " + ev.Path))
	case session.KindUsage:
		if ev.CostUSD > m.costUSD {
			m.costUSD = ev.CostUSD
		}
	case session.KindNotice:
		m.appendLine(m.styles.runner.Render("Runner: " + ev.Text))
	case session.KindError:
		m.appendLine(m.styles.warn.Render("harness error: " + ev.Text))
	case session.KindSessionEnd:
		m.flushStreaming()
		m.ended = true
		m.outcome.Reason = ev.Reason
		m.appendLine(m.styles.warn.Render("Session ended: " + string(ev.Reason)))
	}
}

func (m *model) flushStreaming() {
	if m.streaming.Len() > 0 {
		m.appendLine(m.styles.assistant.Render("Agent: ") + strings.TrimSpace(m.streaming.String()))
		m.streaming.Reset()
	}
}

func (m *model) appendLine(s string) {
	m.lines = append(m.lines, s)
	m.refresh()
}

func (m *model) refresh() {
	if !m.ready {
		return
	}
	body := strings.Join(m.lines, "\n")
	if m.streaming.Len() > 0 {
		body += "\n" + m.styles.assistant.Render("Agent: ") + m.streaming.String()
	}
	m.vp.SetContent(wrap(body, m.vp.Width))
	m.vp.GotoBottom()
}

func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

func (m model) statusLine() string {
	turns := fmt.Sprintf("turns %d", m.userTurns)
	if m.opts.Budgets.MaxUserTurns > 0 {
		turns += fmt.Sprintf("/%d", m.opts.Budgets.MaxUserTurns)
	}
	tools := fmt.Sprintf("tools %d", m.toolCalls)
	if m.opts.MaxTurns > 0 {
		tools += fmt.Sprintf("/%d", m.opts.MaxTurns)
	}
	cost := fmt.Sprintf("$%.2f", m.costUSD)
	if m.opts.MaxCostUSD > 0 {
		cost += fmt.Sprintf("/$%.2f", m.opts.MaxCostUSD)
	}
	if m.opts.PlanRun {
		cost = "paid by your plan"
	}
	idle := ""
	if m.opts.Budgets.IdleTimeoutSeconds > 0 {
		left := time.Duration(m.opts.Budgets.IdleTimeoutSeconds)*time.Second - time.Since(m.lastTurn)
		if left < 0 {
			left = 0
		}
		idle = fmt.Sprintf(" · idle ends in %s", left.Round(time.Second))
	}
	state := ""
	if m.busy {
		state = " · thinking…"
	}
	if m.notice != "" {
		state += " · " + m.notice
	}
	return fmt.Sprintf(" %s · interactive · %s · %s · %s%s%s ", m.opts.Ref, turns, tools, cost, idle, state)
}

func (m model) View() string {
	if !m.ready {
		return "starting…"
	}
	if m.quitting || m.ended {
		return ""
	}
	return m.vp.View() + "\n" + m.input.View() + "\n" + m.styles.status.Width(m.width).Render(m.statusLine())
}
