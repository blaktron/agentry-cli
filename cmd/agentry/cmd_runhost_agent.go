package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/tui"
)

// Persistent agents on a remote runhost. The agents run on the runhost under
// its service, with the runhost's own policy and model, and a device drives
// them over the run key's agent verb:
//
//	agent run <subcommand> [args…]  runs the runhost's own `agentry agent <subcommand>`
//	agent socket <name>             relays one connection to the agent's owner socket
//
// run streams the command's stdout, stderr and descriptor 3 back as data
// frames named for them, and passes the device's input to its stdin. Only
// the subcommands in runhostAgentCommands run, and none of the flags that
// would read a file or a credential of the device's.

// runhostAgentCommands are the agent subcommands a device runs on a runhost.
var runhostAgentCommands = []string{"list", "show", "start", "stop", "attach", "enable", "disable", "reset", "grant", "configure", "tasks", "secret"}

// runhostAgentRefusedFlags name a file, a sign-in or a credential on the
// device, which the runhost's CLI would look for on the server instead.
// A sign-in whose browser answer is a callback in a container on the runhost
// (Google Workspace's, a calendar's) is refused too: nothing relays it yet.
var runhostAgentRefusedFlags = []string{
	"policy", "personality-file", "share", "knowledge", "subagent",
	"calendar-sign-in", "google-client", "google-sign-in",
	"dev-host", "dev-key", "dev-passphrase", "dev-password",
}

// The data frames' streams.
const (
	agentStreamStdout = "stdout"
	agentStreamStderr = "stderr"
	agentStreamEvents = "events"
)

// runhostAgentExe is the CLI the verb runs: this one (a variable for the
// tests).
var runhostAgentExe = os.Executable

// runhostCaller is the runhost connection the device's agent commands go
// over: a *runhost.Client.
type runhostCaller interface {
	Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(runhost.Frame) error) (runhost.Answer, error)
}

// runhostAgentsDir holds a marker for each agent the service started, which
// it starts again when it starts (variables for the tests).
var runhostAgentsDir = filepath.Join(runhost.StateDir, "agents")

func runhostAgentVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	switch {
	case len(req.Args) >= 2 && req.Args[0] == "run":
		return runhostAgentRun(ctx, req.Args[1], req.Args[2:], in, fw)
	case len(req.Args) == 2 && req.Args[0] == "socket":
		return runhostAgentSocket(ctx, req.Args[1], in, fw)
	}
	return engine.ExitUsage, errors.New("agent takes run <subcommand> [args…] or socket <name>")
}

// checkRunhostAgentArgs refuses a subcommand the runhost does not run and a
// flag that names something on the device.
func checkRunhostAgentArgs(sub string, args []string) error {
	if !slices.Contains(runhostAgentCommands, sub) {
		return fmt.Errorf("agentry agent %s does not run on a runhost (these do: %s)", sub, strings.Join(runhostAgentCommands, ", "))
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		name = strings.TrimPrefix(name, "no-")
		if slices.Contains(runhostAgentRefusedFlags, name) {
			return fmt.Errorf("--%s is not available with --on: it reads a file, a sign-in or a credential from this computer", name)
		}
	}
	return nil
}

// runhostAgentRun runs the runhost's own `agentry agent sub args…`, as the
// service's account and under its runner home.
func runhostAgentRun(ctx context.Context, sub string, args []string, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if err := checkRunhostAgentArgs(sub, args); err != nil {
		return engine.ExitUsage, err
	}
	exe, err := runhostAgentExe()
	if err != nil {
		return engine.ExitCLI, err
	}
	argv := append([]string{"agent", sub}, args...)
	c := exec.CommandContext(ctx, exe, argv...) //nolint:gosec // this binary, with an allowed subcommand
	c.Env = append(os.Environ(), "AGENTRY_NO_BROWSER=1")
	c.Stdout = streamWriter{fw: fw, stream: agentStreamStdout}
	c.Stderr = streamWriter{fw: fw, stream: agentStreamStderr}
	// Only attach reads the device's input and writes events on descriptor
	// 3, and secret and configure the value the device read for them on
	// stdin. Every other command gets neither: a supervisor that start
	// detaches would inherit them, and the answer would not end while it
	// runs.
	var werr error
	if sub == "secret" || sub == "configure" {
		c.Stdin = io.LimitReader(in, persistent.MaxSecretBytes+3)
	}
	if sub == "attach" {
		c.Stdin = in
		er, ew, err := os.Pipe()
		if err != nil {
			return engine.ExitCLI, err
		}
		c.ExtraFiles = []*os.File{ew}
		if err := c.Start(); err != nil {
			_ = er.Close()
			_ = ew.Close()
			return engine.ExitCLI, err
		}
		_ = ew.Close()
		copied := make(chan struct{})
		go func() {
			defer close(copied)
			_, _ = io.Copy(streamWriter{fw: fw, stream: agentStreamEvents}, er)
		}()
		werr = c.Wait()
		<-copied
		_ = er.Close()
	} else {
		werr = c.Run()
	}
	code := 0
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return engine.ExitCLI, werr
		}
		code = ee.ExitCode()
	}
	if code == 0 && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch sub {
		case "start":
			_ = markRunhostAgent(args[0], true)
		case "stop", "disable", "reset":
			_ = markRunhostAgent(args[0], false)
		}
	}
	if code != 0 {
		return code, fmt.Errorf("agentry agent %s exited %d", sub, code)
	}
	return 0, nil
}

// streamWriter writes what it is given as data frames of one stream.
type streamWriter struct {
	fw     *runhost.FrameWriter
	stream string
}

func (s streamWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), runhost.DataChunk)
		if err := s.fw.Frame(runhost.Frame{Type: runhost.FrameData, Text: s.stream, Data: append([]byte(nil), p[:c]...)}); err != nil {
			return n, err
		}
		n += c
		p = p[c:]
	}
	return n, nil
}

// markRunhostAgent records, or forgets, that the service started the agent
// arg names: a file named for the agent, holding the argument it was
// started by.
func markRunhostAgent(arg string, on bool) error {
	a, err := cliAgent(arg)
	if err != nil {
		return err
	}
	p := filepath.Join(runhostAgentsDir, a.Name)
	if !on {
		return os.Remove(p)
	}
	if err := os.MkdirAll(runhostAgentsDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(arg), 0o600)
}

// startRunhostAgents starts again each agent the service had started: a
// restart of the service (an update's) ends them with it. One that needs a
// new agreement is left stopped and its marker removed, since nobody is
// there to agree.
func startRunhostAgents(ctx context.Context, logf func(string, ...any)) {
	ents, err := os.ReadDir(runhostAgentsDir)
	if err != nil {
		return
	}
	exe, err := runhostAgentExe()
	if err != nil {
		return
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(runhostAgentsDir, e.Name())) //nolint:gosec // the service's own marker
		name := strings.TrimSpace(string(b))
		if err != nil || name == "" {
			continue
		}
		c := exec.CommandContext(ctx, exe, "agent", "start", name) //nolint:gosec // this binary, an agent the service started
		c.Env = append(os.Environ(), "AGENTRY_NO_BROWSER=1")
		out, err := c.CombinedOutput()
		if err != nil {
			logf("agent %s did not start again: %v: %s", name, err, firstLineOf(lastLines(string(out))))
			_ = markRunhostAgent(name, false)
			continue
		}
		logf("agent %s started again", name)
	}
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 2 {
		lines = lines[len(lines)-2:]
	}
	return strings.Join(lines, "; ")
}

// runhostAgentSocket relays one connection between the device and the owner
// socket of the agent name on the runhost.
func runhostAgentSocket(ctx context.Context, name string, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	a, err := cliAgent(name)
	if err != nil {
		return engine.ExitUsage, err
	}
	home, err := runnerHome()
	if err != nil {
		return engine.ExitCLI, err
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "unix", persistent.SocketPath(home, a.Name))
	cancel()
	if err != nil {
		return engine.ExitUsage, fmt.Errorf("the %s agent is not running on this runhost", a.Name)
	}
	defer func() { _ = conn.Close() }()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(streamWriter{fw: fw, stream: agentStreamEvents}, conn)
	return 0, nil
}

// --- the device's side

// wireAgentOn gives agent cmd --on and makes each subcommand a runhost runs
// run there when it is given.
func wireAgentOn(cmd *cobra.Command) {
	var on string
	cmd.PersistentFlags().StringVar(&on, "on", "", "run the command on the remote runhost named, where the agent runs (agentry runhost list)")
	for _, sub := range cmd.Commands() {
		name := sub.Name()
		if name != "chat" && !slices.Contains(runhostAgentCommands, name) {
			continue
		}
		// A command of subcommands (secret) runs each of them there, named
		// first among its arguments.
		if sub.HasSubCommands() {
			for _, child := range sub.Commands() {
				child, local := child, child.RunE
				child.RunE = func(c *cobra.Command, args []string) error {
					if on == "" {
						return local(c, args)
					}
					return remoteAgentCommand(c, on, name, append([]string{child.Name()}, args...))
				}
			}
			continue
		}
		local := sub.RunE
		sub.RunE = func(c *cobra.Command, args []string) error {
			if on == "" {
				return local(c, args)
			}
			if name == "chat" {
				return remoteAgentChat(c, on, args[0])
			}
			return remoteAgentCommand(c, on, name, args)
		}
	}
}

// forwardedArgs are the arguments and the changed flags of c, as the
// runhost's CLI takes them, the root's --json included.
func forwardedArgs(c *cobra.Command, args []string) []string {
	out := append([]string(nil), args...)
	c.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "on" {
			return
		}
		switch f.Value.Type() {
		case "bool":
			out = append(out, "--"+f.Name+"="+f.Value.String())
		case "stringArray", "stringSlice":
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				for _, v := range sv.GetSlice() {
					out = append(out, "--"+f.Name+"="+v)
				}
			}
		default:
			out = append(out, "--"+f.Name+"="+f.Value.String())
		}
	})
	if flags.jsonOut {
		out = append(out, "--json")
	}
	return out
}

// dialAgentRunhost opens the runhost name over this device's run key and
// checks it runs this CLI's version.
func dialAgentRunhost(cmd *cobra.Command, name string) (*runhost.Client, *runhost.Record, error) {
	home, err := runnerHome()
	if err != nil {
		return nil, nil, err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return nil, nil, err
	}
	rec, ok := recs.Find(name)
	if !ok {
		return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer (`agentry runhost list`)", name))
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return nil, rec, err
	}
	c, err := runhost.DialRunhost(cmd.Context(), *rec, keys)
	if err != nil {
		return nil, rec, engine.Classify(engine.ExitGateway, err)
	}
	if _, err := remote.SameVersion(cmd.Context(), c, name); err != nil {
		_ = c.Close()
		var skew *remote.SkewError
		if errors.As(err, &skew) {
			return nil, rec, engine.Classify(engine.ExitUsage, err)
		}
		return nil, rec, engine.Classify(engine.ExitGateway, err)
	}
	return c, rec, nil
}

// remoteAgentCommand runs `agentry agent sub` on the runhost on. What the
// agent would be given for the first time is agreed to here, as on a
// computer, and the command then runs again with --yes.
func remoteAgentCommand(cmd *cobra.Command, on, sub string, args []string) error {
	fwd := forwardedArgs(cmd, args)
	if err := checkRunhostAgentArgs(sub, fwd); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	c, rec, err := dialAgentRunhost(cmd, on)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	value, err := runhostAgentValue(cmd, rec.Name, sub, args, fwd)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		code, stderr, err := runOnRunhostAgent(cmd, c, sub, fwd, value)
		if err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
		if code == 0 {
			return nil
		}
		if attempt == 0 && strings.Contains(stderr, "pass --yes to agree") && !flags.jsonOut && stdinIsTTY(cmd.InOrStdin()) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Give it these on runhost %s? [y/N] ", rec.Name)
			line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
				fwd = append(fwd, "--yes")
				continue
			}
			return engine.Classify(engine.ExitPolicy, errors.New("not agreed to; nothing was changed"))
		}
		return engine.Classify(code, fmt.Errorf("on runhost %s: agentry agent %s did not succeed", rec.Name, sub))
	}
}

// runOnRunhostAgent runs one agent subcommand on the runhost and answers with
// its exit code and what it wrote to stderr. Its stdout and stderr are this
// command's; its descriptor 3, attach's events, goes to this process's own
// descriptor 3 when that is open.
func runOnRunhostAgent(cmd *cobra.Command, c runhostCaller, sub string, fwd []string, value string) (int, string, error) {
	var (
		mu     sync.Mutex
		stderr strings.Builder
	)
	var events io.Writer
	if sub == "attach" {
		if f := os.NewFile(3, "events"); f != nil {
			if _, err := f.Stat(); err == nil {
				events = f
			}
		}
	}
	send := func(w io.Writer) error {
		switch {
		case sub == "attach":
			_, err := io.Copy(w, cmd.InOrStdin())
			return err
		case value != "":
			_, err := io.WriteString(w, value+"\n")
			return err
		}
		return nil
	}
	a, err := c.Call(cmd.Context(), "agent", append([]string{"run", sub}, fwd...), send, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		switch f.Text {
		case agentStreamStdout:
			_, _ = cmd.OutOrStdout().Write(f.Data)
		case agentStreamStderr:
			mu.Lock()
			if stderr.Len() < 64<<10 {
				stderr.Write(f.Data)
			}
			mu.Unlock()
			_, _ = cmd.ErrOrStderr().Write(f.Data)
		case agentStreamEvents:
			if events != nil {
				_, _ = events.Write(f.Data)
			}
		}
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	mu.Lock()
	defer mu.Unlock()
	return a.Code, stderr.String(), nil
}

// remoteAgentChat is agent chat with an agent running on the runhost on:
// this terminal's chat attaches to the agent's owner socket there, relayed
// through a socket of this device's own, so it is the chat a computer's
// agent has.
func remoteAgentChat(cmd *cobra.Command, on, name string) error {
	c, rec, err := dialAgentRunhost(cmd, on)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	key, ok := agentSocketKey(name)
	if !ok {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not an agent's name or an installed agent's reference (ns/name)", name))
	}
	home, stop, err := relayAgentSocket(cmd.Context(), c, name)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	defer stop()
	qctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	st, err := persistent.QueryStatus(qctx, home, key)
	cancel()
	if err != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running on runhost %s: agentry agent start %s --on %s", name, rec.Name, name, rec.Name))
	}
	s, err := persistent.Attach(cmd.Context(), home, key, nil)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	_, uiErr := runChat(cmd.Context(), s, tui.Options{Ref: name + " · on " + rec.Name, Model: st.Model, HasTask: true}, cmd.InOrStdin(), cmd.OutOrStdout())
	_, _ = s.End(context.Background(), false)
	if ended, reason := s.Ended(); ended && reason != "" && reason != "cancelled" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The %s agent's segment ended (%s).\n", name, reason)
	} else {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The %s agent keeps running on %s. Stop it: agentry agent stop %s --on %s\n", name, rec.Name, name, rec.Name)
	}
	if uiErr != nil {
		return engine.Classify(engine.ExitCLI, uiErr)
	}
	return nil
}

// relayAgentSocket listens on an owner socket of this device's own, in a
// runner home made for it, and relays each connection to the agent name's
// owner socket on the runhost c reaches. The home is what persistent's
// socket calls are given; stop closes the listener and removes it.
func relayAgentSocket(ctx context.Context, c runhostCaller, name string) (string, func(), error) {
	key, ok := agentSocketKey(name)
	if !ok {
		return "", nil, fmt.Errorf("%q is not an agent's name or an installed agent's reference (ns/name)", name)
	}
	home, err := os.MkdirTemp("", "agentry-rh-")
	if err != nil {
		return "", nil, err
	}
	sock := persistent.SocketPath(home, key)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		_ = os.RemoveAll(home)
		return "", nil, err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(home)
		return "", nil, err
	}
	rctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = c.Call(rctx, "agent", []string{"socket", name}, func(w io.Writer) error {
					_, err := io.Copy(w, conn)
					return err
				}, func(f runhost.Frame) error {
					if f.Type == runhost.FrameData {
						_, err := conn.Write(f.Data)
						return err
					}
					return nil
				})
			}()
		}
	}()
	return home, func() {
		cancel()
		_ = l.Close()
		_ = os.RemoveAll(home)
	}, nil
}

// agentNameRE is a built-in agent's name; agentRefRE an installed agent's
// reference, whose runner home key is ns.name.
var (
	agentNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	agentRefRE  = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,62})/([a-z0-9][a-z0-9-]{0,62})$`)
)

// agentSocketKey is the name the runner home keys the agent name or ref
// names by: a built-in's name, or an installed agent's ns.name.
func agentSocketKey(name string) (string, bool) {
	if agentNameRE.MatchString(name) {
		return name, true
	}
	if m := agentRefRE.FindStringSubmatch(name); m != nil {
		return m[1] + "." + m[2], true
	}
	return "", false
}

// thisMachine is how the agent commands name the machine they run on: a
// runhost's CLI runs them for a device.
func thisMachine() string {
	if onRunhostServer() {
		return "this runhost"
	}
	return "this computer"
}

// onThisRunhost is what a hint the agent commands print adds on a runhost:
// the device runs the next command with --on.
func onThisRunhost() string {
	if onRunhostServer() {
		return " --on <this runhost>"
	}
	return ""
}

// runhostAgentValue is what a command that keeps a value on the runhost is
// sent: a stored secret's (secret set) or a git host account's token
// (configure --account), read here as the command reads it on a computer,
// after the person agrees to its being kept on the runhost, encrypted in
// its store. Any other command is sent nothing.
func runhostAgentValue(cmd *cobra.Command, runhostName, sub string, args, fwd []string) (string, error) {
	var what string
	switch {
	case sub == "secret" && len(args) >= 1 && args[0] == "set":
		if len(args) < 3 {
			return "", nil
		}
		what = "the secret " + args[2]
	case sub == "configure" && slices.ContainsFunc(fwd, func(a string) bool { return strings.HasPrefix(a, "--account=") }):
		what = "the account's token"
	case sub == "configure" && slices.Contains(fwd, "--fastmail-token=true"):
		what = "your Fastmail API token"
	case sub == "configure" && slices.ContainsFunc(fwd, func(a string) bool { return strings.HasPrefix(a, "--calendar-feed=") }):
		what = "the calendar's address"
	default:
		return "", nil
	}
	v, err := readSecretValue(cmd, "Value of "+what, what)
	if err != nil {
		return "", engine.Classify(engine.ExitUsage, err)
	}
	if !slices.Contains(fwd, "--yes=true") && !slices.Contains(fwd, "--yes") {
		ask := terminalAsker(cmd)
		if ask == nil {
			return "", engine.Classify(engine.ExitPolicy, fmt.Errorf("%s would be kept on runhost %s, and no one is at a terminal to agree: pass --yes", what, runhostName))
		}
		ok, err := ask.Confirm(fmt.Sprintf("Keep %s on runhost %s, encrypted in its store, for the agent that runs there?", what, runhostName), false)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", engine.Classify(engine.ExitPolicy, errors.New("nothing was kept"))
		}
	}
	return v, nil
}
