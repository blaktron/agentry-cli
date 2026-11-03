package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// A runhost's persistent agents, as a device drives them over the run key's
// agent verb: `agent run <subcommand> [args…]` runs the runhost's own
// `agentry agent <subcommand>`, its stdout, stderr and descriptor 3 coming
// back as data frames named for them; `agent socket <name>` relays one
// connection to the agent's owner socket there.

// AgentCommands are the agent subcommands a device runs on a runhost.
var AgentCommands = []string{"list", "show", "start", "stop", "attach", "enable", "disable", "reset", "grant", "configure", "tasks", "secret"}

// AgentRefusedFlags name a file, a sign-in or a credential on the device,
// which the runhost's CLI would look for on the server instead. A sign-in
// whose browser answer is a callback in a container on the runhost (Google
// Workspace's, a calendar's) is refused too: nothing relays it yet.
var AgentRefusedFlags = []string{
	"policy", "personality-file", "share", "knowledge", "subagent",
	"calendar-sign-in", "google-client", "google-sign-in",
	"dev-host", "dev-key", "dev-passphrase", "dev-password",
}

// The agent verb's data frames' streams.
const (
	AgentStdout = "stdout"
	AgentStderr = "stderr"
	AgentEvents = "events"
)

// CheckAgentArgs refuses a subcommand the runhost does not run and a flag
// that names something on the device.
func CheckAgentArgs(sub string, args []string) error {
	if !slices.Contains(AgentCommands, sub) {
		return fmt.Errorf("agentry agent %s does not run on a runhost (these do: %s)", sub, strings.Join(AgentCommands, ", "))
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		name = strings.TrimPrefix(name, "no-")
		if slices.Contains(AgentRefusedFlags, name) {
			return fmt.Errorf("--%s is not available with --on: it reads a file, a sign-in or a credential from this computer", name)
		}
	}
	return nil
}

// Caller is the runhost connection the agent calls go over: a
// *runhost.Client.
type Caller interface {
	Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(runhost.Frame) error) (runhost.Answer, error)
}

// AgentIO is where an agent command's streams go, and what it reads: Stdin
// is attach's input (nil for none), Value one line sent on stdin to the
// commands that keep a value (secret and configure), and Events attach's
// events (nil drops them).
type AgentIO struct {
	Stdin          io.Reader
	Value          string
	Stdout, Stderr io.Writer
	Events         io.Writer
}

// AgentCommand runs `agentry agent sub args…` on the runhost c reaches and
// answers with its exit code and what it wrote to stderr (at most 64 KiB).
func AgentCommand(ctx context.Context, c Caller, sub string, args []string, o AgentIO) (int, string, error) {
	if err := CheckAgentArgs(sub, args); err != nil {
		return 0, "", err
	}
	var (
		mu     sync.Mutex
		stderr strings.Builder
	)
	send := func(w io.Writer) error {
		switch {
		case sub == "attach" && o.Stdin != nil:
			_, err := io.Copy(w, o.Stdin)
			return err
		case o.Value != "":
			_, err := io.WriteString(w, o.Value+"\n")
			return err
		}
		return nil
	}
	write := func(w io.Writer, b []byte) {
		if w != nil {
			_, _ = w.Write(b)
		}
	}
	a, err := c.Call(ctx, "agent", append([]string{"run", sub}, args...), send, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		switch f.Text {
		case AgentStdout:
			write(o.Stdout, f.Data)
		case AgentStderr:
			mu.Lock()
			if stderr.Len() < 64<<10 {
				stderr.Write(f.Data)
			}
			mu.Unlock()
			write(o.Stderr, f.Data)
		case AgentEvents:
			write(o.Events, f.Data)
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

// RelayAgentSocket listens on an owner socket of this device's own, in a
// runner home made for it, and relays each connection to the owner socket
// of the agent name on the runhost c reaches. persistent's socket calls
// (Attach, QueryStatus, RequestStop) are given the home and the key; stop
// closes the listener and removes the home.
func RelayAgentSocket(ctx context.Context, c Caller, name string) (home, key string, stop func(), err error) {
	key, ok := AgentSocketKey(name)
	if !ok {
		return "", "", nil, fmt.Errorf("%q is not an agent's name or an installed agent's reference (ns/name)", name)
	}
	home, err = os.MkdirTemp("", "agentry-rh-")
	if err != nil {
		return "", "", nil, err
	}
	sock := persistent.SocketPath(home, key)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		_ = os.RemoveAll(home)
		return "", "", nil, err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(home)
		return "", "", nil, err
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
	return home, key, func() {
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

// AgentSocketKey is the name the runner home keys the agent name or ref
// names by: a built-in's name, or an installed agent's ns.name.
func AgentSocketKey(name string) (string, bool) {
	if agentNameRE.MatchString(name) {
		return name, true
	}
	if m := agentRefRE.FindStringSubmatch(name); m != nil {
		return m[1] + "." + m[2], true
	}
	return "", false
}
