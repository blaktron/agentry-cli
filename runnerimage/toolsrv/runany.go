package toolsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// run_command under Exec.Any: the Coding agent's. Any program its exec image
// holds runs, named on the image's PATH or by a path, in a folder beneath the
// workspace, with HOME in the workspace so caches and settings last. What
// holds it is the same as for a declared command: the confinement (Landlock
// writes beneath /tmp, the scratch folder and the workspace, or no command
// at all), its own process group, a time limit, an output cap and the scrub.
// It runs as the person's ids where the spec names them, so what it writes in
// the share folder is theirs.

// passedEnv are the variables of this server's own environment a program run
// under Any is given beside PATH, HOME and LANG: the image's settings for
// its toolchain, never a secret.
var passedEnv = []string{"GOTOOLCHAIN", "NPM_CONFIG_UPDATE_NOTIFIER"}

// makeExecHome makes Exec.Home, and gives it and its parent to the person,
// so a program run as them can write its caches there. A program of an
// earlier start may have left a link or a file where HOME goes: it is
// removed, never followed, and nothing here follows a link, so the chown
// cannot be pointed elsewhere in the container. Best-effort: a program that
// needs HOME says so itself.
func (s *server) makeExecHome() {
	home := s.spec.Exec.Home
	if home == "" {
		return
	}
	if fi, err := os.Lstat(home); err == nil && !fi.IsDir() {
		if err := os.Remove(home); err != nil {
			s.logf("run_command: HOME %s is not a folder and could not be removed: %v", home, err)
			return
		}
	}
	// The parent is the agent's data volume's mount, which no program can
	// replace; it exists but in a test.
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil { //nolint:gosec // the agent's own folder
		s.logf("run_command: make HOME %s: %v", home, err)
		return
	}
	if err := os.Mkdir(home, 0o755); err != nil && !errors.Is(err, fs.ErrExist) { //nolint:gosec // the agent's own folder
		s.logf("run_command: make HOME %s: %v", home, err)
		return
	}
	if o := s.spec.Owner; o != nil {
		for _, p := range []string{filepath.Dir(home), home} {
			_ = os.Lchown(p, o.UID, o.GID)
		}
	}
}

func (s *server) runAnyDescription() string {
	net := " It has no network."
	if s.spec.Fetch.AllowAll {
		net = " It reaches the internet: it can download packages and clone repositories."
	}
	home := ""
	if s.spec.Exec.Home != "" {
		home = " HOME is " + s.spec.Exec.Home + ", kept between conversations, so caches and settings last."
	}
	return "Run a program in your toolchain image: a name on its PATH (build tools, git, curl, python3, go, node, npm and what they install) or a path to one, with its arguments, in dir, a folder beneath /agent. There is no shell unless you run one (bash -c)." +
		home + net + " " + s.confine.describe() +
		" Returns \"exit code N\" plus combined output; a non-zero exit is a result, not an error." +
		fmt.Sprintf(" Set background to start a program that keeps running after the call, such as a dev server or a watcher: the call returns after a few seconds with its id (bg1, bg2…) and its first output, and it runs with no time limit until %s or the end of this conversation's segment; read it with %s. At most %d run at once.",
			ToolStopBackground, ToolBackgroundOutput, maxBackground) +
		s.portsDescription() + s.accountsDescription()
}

// portsDescription says which of the server's ports the person's computer
// publishes, and how to serve one.
func (s *server) portsDescription() string {
	if len(s.spec.Exec.Ports) == 0 {
		return " No port of yours is published on the person's computer: they add one in your configuration."
	}
	var named []string
	for _, p := range s.spec.Exec.Ports {
		named = append(named, fmt.Sprintf("%s on port %d (http://127.0.0.1:%d there)", p.Name, p.Port, p.Port))
	}
	return " The person's computer publishes these ports of yours, each at its own 127.0.0.1 and the same number: " + strings.Join(named, ", ") +
		". A server you start in the background listening on that port, on 127.0.0.1 or 0.0.0.0, answers them there; no other port is published."
}

func (s *server) toolRunAny(raw json.RawMessage) (string, bool) {
	var a struct {
		Command        string   `json:"command"`
		Args           []string `json:"args"`
		Dir            string   `json:"dir"`
		Stdin          string   `json:"stdin"`
		TimeoutSeconds int      `json:"timeoutSeconds"`
		Background     bool     `json:"background"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "run_command: " + err.Error(), true
	}
	if a.Command == "" {
		return "run_command: command is required", true
	}
	if a.Background && a.TimeoutSeconds > 0 {
		return "run_command: a background program has no time limit: set background or timeoutSeconds, not both", true
	}
	if s.confine.refused != "" {
		return "run_command: " + s.confine.refused, true
	}
	dir := s.spec.WorkspaceDir
	if d := strings.TrimSpace(a.Dir); d != "" && d != "." {
		abs, err := resolveUnder(s.spec.WorkspaceDir, path.Clean(strings.TrimPrefix(d, s.spec.WorkspaceDir+"/")))
		if err != nil {
			return "run_command: dir: " + err.Error(), true
		}
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			return fmt.Sprintf("run_command: dir %q is not a folder beneath %s", a.Dir, s.spec.WorkspaceDir), true
		}
		dir = abs
	}
	prog, err := lookProgram(a.Command, dir)
	if err != nil {
		return "run_command: " + err.Error(), true
	}
	name, argv := s.confine.argv(prog, a.Args)
	if a.Background {
		// A context that is never done: the program has no time limit, and
		// its group is killed by stop_background or the server's end.
		cmd := exec.CommandContext(context.Background(), name, argv...)
		s.holdAny(cmd, dir, a.Stdin)
		return s.startBackground(cmd, strings.Join(append([]string{a.Command}, a.Args...), " "))
	}
	limitSec := s.spec.Limits.execTimeoutSec()
	if a.TimeoutSeconds > 0 && a.TimeoutSeconds < limitSec {
		limitSec = a.TimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(limitSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, argv...)
	s.holdAny(cmd, dir, a.Stdin)
	out := &capWriter{max: s.spec.Limits.execOutCap()}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	failed, err := s.confine.run(cmd)
	_ = killGroup(cmd)
	if failed != "" {
		return "run_command: " + failed, true
	}
	text := s.scrubSecrets(out.text())
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("run_command: %q timed out after %ds\n%s", a.Command, limitSec, text), true
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "exit code 0\n" + text, false
	case errors.As(err, &exitErr):
		return fmt.Sprintf("exit code %d\n%s", exitErr.ExitCode(), text), false
	default:
		return "run_command: " + err.Error(), true
	}
}

// holdAny sets up a program run under Any: its ids, its own process
// group, its folder, its environment and its stdin.
func (s *server) holdAny(cmd *exec.Cmd, dir, stdin string) {
	switch {
	case s.confine.asNobody:
		runAs(cmd, unprivilegedID)
	case s.spec.Owner != nil && s.spec.Owner.UID > 0 && s.spec.Owner.GID > 0:
		runAsIDs(cmd, uint32(s.spec.Owner.UID), uint32(s.spec.Owner.GID)) //nolint:gosec // ids from the run's own spec, checked positive
	}
	inOwnGroup(cmd)
	cmd.Dir = dir
	cmd.Env = s.anyEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
}

// lookProgram is the file a command names: a name with no slash found on
// this server's PATH, the image's own; a path as it is, relative to dir.
// The trampoline execs exactly this file.
func lookProgram(command, dir string) (string, error) {
	if strings.ContainsRune(command, '\\') {
		return "", fmt.Errorf("%q: a path is slash-separated", command)
	}
	if !strings.ContainsRune(command, '/') {
		p, err := exec.LookPath(command)
		if err != nil {
			return "", fmt.Errorf("%q is not a program on the image's PATH", command)
		}
		return p, nil
	}
	p := command
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("%q: %v", command, errors.Unwrap(err))
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%q is not an executable file", command)
	}
	return p, nil
}

// anyEnv is a program's environment under Any: the image's PATH, HOME in
// the workspace, a UTF-8 locale, the toolchain's own settings, the
// person's secrets the spec names and their git host accounts, and nothing
// else of this server's environment.
func (s *server) anyEnv() []string {
	home := s.spec.Exec.Home
	if home == "" {
		home = os.TempDir()
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8"}
	for _, k := range passedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	// A secret never takes the place of a variable set above, and the git
	// host accounts' configuration and CLI variables come before the stored
	// secrets: an account's GH_TOKEN is its own, whatever a secret of that
	// name holds.
	set := map[string]bool{}
	for _, kv := range env {
		set[kv[:strings.IndexByte(kv, '=')]] = true
	}
	for _, kv := range s.accountsEnv() {
		if k := kv[:strings.IndexByte(kv, '=')]; !set[k] {
			env = append(env, kv)
			set[k] = true
		}
	}
	for _, k := range s.spec.Exec.SecretEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" && !set[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}
