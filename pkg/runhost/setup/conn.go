package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// AdminAuth is the admin sign-in: the server account setup signs in as, root
// or one that can sudo. It lives in memory for one command and is never
// written anywhere, on either machine.
type AdminAuth struct {
	// Key is a private key's bytes, and Passphrase its passphrase, if any.
	Key        []byte
	Passphrase string
	// Agent signs in with the SSH agent at SSH_AUTH_SOCK.
	Agent bool
	// Password signs in with the account's password.
	Password string
	// Sudo is the account's sudo password, when sudo asks for one; Ask
	// supplies it when it is needed and not given.
	Sudo string
}

// Values are the sign-in's secrets, which no narration may carry.
func (a AdminAuth) Values() []string {
	var out []string
	for _, v := range []string{string(a.Key), a.Passphrase, a.Password, a.Sudo} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (a AdminAuth) methods() ([]ssh.AuthMethod, func(), error) {
	var out []ssh.AuthMethod
	closer := func() {}
	if len(a.Key) > 0 {
		var signer ssh.Signer
		var err error
		if a.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(a.Key, []byte(a.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(a.Key)
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, closer, errors.New("the key file needs its passphrase")
		}
		if err != nil {
			return nil, closer, fmt.Errorf("the key file is not readable: %w", err)
		}
		out = append(out, ssh.PublicKeys(signer))
	}
	if a.Agent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, closer, errors.New("no SSH agent: SSH_AUTH_SOCK is not set")
		}
		c, err := net.Dial("unix", sock)
		if err != nil {
			return nil, closer, fmt.Errorf("the SSH agent at SSH_AUTH_SOCK: %w", err)
		}
		closer = func() { _ = c.Close() }
		out = append(out, ssh.PublicKeysCallback(agent.NewClient(c).Signers))
	}
	if a.Password != "" {
		pw := a.Password
		out = append(out, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(out) == 0 {
		return nil, closer, errors.New("no admin sign-in given: a key file, the SSH agent or a password")
	}
	return out, closer, nil
}

// Conn is the admin connection to a server being set up.
type Conn struct {
	client *ssh.Client
	closer func()
	auth   AdminAuth
	// root is true when the account is root; else sudo runs the root steps.
	root bool
	// sudoNeedsPassword is true when sudo asks for a password.
	sudoNeedsPassword bool
}

// Dial signs in to addr:port as user with auth, holding the connection to
// hostKey, which the caller has already checked.
func Dial(ctx context.Context, addr netip.Addr, port int, user string, hostKey ssh.PublicKey, auth AdminAuth) (*Conn, error) {
	methods, closer, err := auth.methods()
	if err != nil {
		return nil, err
	}
	target := net.JoinHostPort(addr.String(), strconv.Itoa(port))
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		closer()
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:              user,
		Auth:              methods,
		HostKeyCallback:   ssh.FixedHostKey(hostKey),
		HostKeyAlgorithms: hostKeyAlgorithms(hostKey),
		Timeout:           20 * time.Second,
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(raw, target, cfg)
	if err != nil {
		_ = raw.Close()
		closer()
		return nil, fmt.Errorf("sign in to %s as %s: %w", target, user, err)
	}
	_ = raw.SetDeadline(time.Time{})
	conn := &Conn{client: ssh.NewClient(c, chans, reqs), closer: closer, auth: auth}
	return conn, nil
}

func hostKeyAlgorithms(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{k.Type()}
}

// Close ends the connection.
func (c *Conn) Close() error {
	err := c.client.Close()
	c.closer()
	return err
}

// Result is a command's outcome.
type Result struct {
	Stdout, Stderr []byte
	Code           int
}

// Run runs cmd as the admin account, with stdin as its input. out, when not
// nil, also receives the command's standard output as it comes.
func (c *Conn) Run(ctx context.Context, cmd string, stdin io.Reader, out io.Writer) (Result, error) {
	s, err := c.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = s.Close() }()
	var so, se bytes.Buffer
	if out != nil {
		s.Stdout = io.MultiWriter(&so, out)
	} else {
		s.Stdout = &so
	}
	s.Stderr = &se
	if stdin != nil {
		s.Stdin = stdin
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = s.Signal(ssh.SIGTERM)
		_ = s.Close()
		return Result{Stdout: so.Bytes(), Stderr: se.Bytes(), Code: -1}, ctx.Err()
	case err = <-done:
	}
	r := Result{Stdout: so.Bytes(), Stderr: se.Bytes()}
	var exit *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		r.Code = exit.ExitStatus()
	default:
		return r, err
	}
	return r, nil
}

// ErrNoRoot means the account is neither root nor able to sudo.
var ErrNoRoot = errors.New("the admin account is not root and cannot use sudo: sign in as root, or as an account in the sudo group")

// ErrSudoPassword means sudo asks for a password and none was given.
var ErrSudoPassword = errors.New("sudo asks for a password")

// FindRoot learns how the root steps run: as root itself, through sudo with
// no password, or through sudo with the sudo password (or, when sudo takes
// it, the sign-in password).
func (c *Conn) FindRoot(ctx context.Context) error {
	r, err := c.Run(ctx, "id -u", nil, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(r.Stdout)) == "0" {
		c.root = true
		return nil
	}
	r, err = c.Run(ctx, "command -v sudo >/dev/null && sudo -n true", nil, nil)
	if err != nil {
		return err
	}
	if r.Code == 0 {
		return nil
	}
	r, err = c.Run(ctx, "command -v sudo", nil, nil)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return ErrNoRoot
	}
	c.sudoNeedsPassword = true
	if c.auth.Sudo == "" && c.auth.Password != "" {
		c.auth.Sudo = c.auth.Password
		if c.checkSudo(ctx) == nil {
			return nil
		}
		c.auth.Sudo = ""
	}
	if c.auth.Sudo == "" {
		return ErrSudoPassword
	}
	return c.checkSudo(ctx)
}

// SetSudo gives the sudo password after FindRoot asked for it, and checks it.
func (c *Conn) SetSudo(ctx context.Context, pw string) error {
	c.auth.Sudo = pw
	return c.checkSudo(ctx)
}

func (c *Conn) checkSudo(ctx context.Context) error {
	r, err := c.Run(ctx, "sudo -S -k -p '' true", strings.NewReader(c.auth.Sudo+"\n"), nil)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return errors.New("sudo refused the password, or this account may not sudo")
	}
	return nil
}

// RunRoot runs the shell command cmd as root. Under sudo with a password, the
// password is sudo's first line of input and nothing else is: a root step
// that needs input reads it from a staged file.
func (c *Conn) RunRoot(ctx context.Context, cmd string, out io.Writer) (Result, error) {
	switch {
	case c.root:
		return c.Run(ctx, "sh -c "+shellQuote(cmd), nil, out)
	case c.sudoNeedsPassword:
		return c.Run(ctx, "sudo -S -k -p '' -- sh -c "+shellQuote(cmd), strings.NewReader(c.auth.Sudo+"\n"), out)
	default:
		return c.Run(ctx, "sudo -n -- sh -c "+shellQuote(cmd), nil, out)
	}
}

// Stage makes a private directory on the server for the files a step needs,
// owned by the admin account, and returns its path.
func (c *Conn) Stage(ctx context.Context) (string, error) {
	r, err := c.Run(ctx, "umask 077 && mktemp -d /tmp/agentry-runhost.XXXXXXXX", nil, nil)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(r.Stdout))
	if r.Code != 0 || !strings.HasPrefix(dir, "/tmp/agentry-runhost.") || strings.ContainsAny(dir, " '\"\\\n") {
		return "", fmt.Errorf("could not make a staging directory on the server: %s", strings.TrimSpace(string(r.Stderr)))
	}
	return dir, nil
}

// Upload writes data to the file name in the staging directory dir,
// readable by its owner alone.
func (c *Conn) Upload(ctx context.Context, dir, name string, data []byte) error {
	if strings.ContainsAny(name, "/ '\"\\") {
		return fmt.Errorf("an unsafe file name %q", name)
	}
	r, err := c.Run(ctx, "umask 077 && cat > "+shellQuote(dir+"/"+name), bytes.NewReader(data), nil)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("upload %s: %s", name, strings.TrimSpace(string(r.Stderr)))
	}
	return nil
}

// Unstage removes the staging directory, as root, since root steps may have
// left files in it.
func (c *Conn) Unstage(ctx context.Context, dir string) {
	if !strings.HasPrefix(dir, "/tmp/agentry-runhost.") {
		return
	}
	_, _ = c.RunRoot(ctx, "rm -rf "+shellQuote(dir), nil)
}

// ReadRoot reads a file as root; a missing file is nil and no error.
func (c *Conn) ReadRoot(ctx context.Context, path string) ([]byte, error) {
	r, err := c.RunRoot(ctx, "if [ -e "+shellQuote(path)+" ]; then cat "+shellQuote(path)+"; else exit 3; fi", nil)
	if err != nil {
		return nil, err
	}
	switch r.Code {
	case 0:
		return r.Stdout, nil
	case 3:
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %s", path, strings.TrimSpace(string(r.Stderr)))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
