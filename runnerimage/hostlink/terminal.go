package hostlink

import (
	"io"
	"os"
	"os/exec"
	"time"
)

// Terminal is a shell session's process: what is written to it is
// typed into the shell, and what it reads is everything the shell and ssh
// print, io.EOF once it has ended.
type Terminal interface {
	io.ReadWriter
	// Wait waits for the process to end, after a read has returned an
	// error, and gives its exit status.
	Wait() (int, error)
	// Kill ends the process, which ends the session on the host.
	Kill()
}

// typeTimeout bounds one write to a session: a host that reads nothing
// fills the pipe, and the host link does not wait on it for ever.
const typeTimeout = 20 * time.Second

type pipes struct {
	cmd     *exec.Cmd
	in, out *os.File
	ends    []*os.File // the child's ends, closed once it has them
}

// startTerminal starts argv, a shell session's ssh, its standard input one
// pipe and its standard output and error another: ssh asks the host for
// the pseudo-terminal itself (-tt), so this side needs none.
func startTerminal(argv, env []string) (Terminal, error) {
	p, err := pipeTerminal(argv, env)
	if err != nil {
		return nil, err
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	return p, nil
}

func pipeTerminal(argv, env []string) (*pipes, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // ssh, with arguments shellArgv builds
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
	return &pipes{cmd: cmd, in: inW, out: outR, ends: []*os.File{inR, outW}}, nil
}

func (p *pipes) start() error {
	err := p.cmd.Start()
	for _, f := range p.ends {
		_ = f.Close()
	}
	if err != nil {
		_ = p.in.Close()
		_ = p.out.Close()
	}
	return err
}

func (p *pipes) Read(b []byte) (int, error) { return p.out.Read(b) }

func (p *pipes) Write(b []byte) (int, error) {
	_ = p.in.SetWriteDeadline(time.Now().Add(typeTimeout))
	return p.in.Write(b)
}

func (p *pipes) Wait() (int, error) {
	err := p.cmd.Wait()
	_ = p.in.Close()
	_ = p.out.Close()
	return exitCode(p.cmd, err)
}

// Kill ends ssh, which ends the session on the host: ssh is the only
// process a session keeps (askpass runs and ends before the shell starts),
// and os.Process signals nothing once Wait has reaped it.
func (p *pipes) Kill() {
	_ = p.in.Close()
	_ = p.cmd.Process.Kill()
}
