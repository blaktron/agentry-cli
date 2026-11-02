package runhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client is a device's connection to a runhost over its run key.
type Client struct {
	ssh *ssh.Client
}

// ErrHostKeyChanged means the server offered a host key other than the one
// pinned at setup: someone may be in the way, or the server was reinstalled.
var ErrHostKeyChanged = errors.New("the runhost's host key is not the one pinned at setup")

// DialRunhost reaches the runhost rec over its run key. The server must offer
// exactly the host key pinned in rec; any other is refused.
func DialRunhost(ctx context.Context, rec Record, keys *DeviceKeys) (*Client, error) {
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rec.HostKey))
	if err != nil {
		return nil, fmt.Errorf("the record of %s holds no readable host key: %w", rec.Name, err)
	}
	signer, err := keys.Signer()
	if err != nil {
		return nil, err
	}
	target := net.JoinHostPort(rec.Host, strconv.Itoa(rec.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("reach %s at %s: %w", rec.Name, target, err)
	}
	algos := []string{pinned.Type()}
	if pinned.Type() == ssh.KeyAlgoRSA {
		algos = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	cfg := &ssh.ClientConfig{
		User: User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if !bytes.Equal(k.Marshal(), pinned.Marshal()) {
				return fmt.Errorf("%w: %s offers %s, and %s was pinned", ErrHostKeyChanged, rec.Name, ssh.FingerprintSHA256(k), ssh.FingerprintSHA256(pinned))
			}
			return nil
		},
		HostKeyAlgorithms: algos,
		Timeout:           20 * time.Second,
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(raw, target, cfg)
	if err != nil {
		_ = raw.Close()
		if errors.Is(err, ErrHostKeyChanged) {
			return nil, err
		}
		return nil, fmt.Errorf("sign in to %s with this device's run key: %w", rec.Name, err)
	}
	_ = raw.SetDeadline(time.Time{})
	return &Client{ssh: ssh.NewClient(c, chans, reqs)}, nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.ssh.Close() }

// Ask asks the service verb with args and reads its whole answer. out, when
// not nil, receives the narration as it comes.
func (c *Client) Ask(ctx context.Context, verb string, args []string, out func(string)) (Answer, error) {
	s, err := c.ssh.NewSession()
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = s.Close() }()
	stdout, err := s.StdoutPipe()
	if err != nil {
		return Answer{}, err
	}
	cmd := strings.Join(append([]string{verb}, args...), " ")
	if err := s.Start(cmd); err != nil {
		return Answer{}, err
	}
	type res struct {
		a   Answer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := ReadAnswer(stdout, out)
		done <- res{a, err}
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return Answer{}, ctx.Err()
	case r := <-done:
		_ = s.Wait()
		return r.a, r.err
	}
}

// Call asks the service verb with args, gives send the session's input
// (closed when send returns; nil sends nothing), and hands every frame of
// the answer to each until the exit frame. An error from send or each ends
// the session.
func (c *Client) Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(Frame) error) (Answer, error) {
	s, err := c.ssh.NewSession()
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = s.Close() }()
	stdout, err := s.StdoutPipe()
	if err != nil {
		return Answer{}, err
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		return Answer{}, err
	}
	if err := s.Start(strings.Join(append([]string{verb}, args...), " ")); err != nil {
		return Answer{}, err
	}
	sent := make(chan error, 1)
	go func() {
		var err error
		if send != nil {
			err = send(stdin)
		}
		_ = stdin.Close()
		sent <- err
	}()
	type res struct {
		a   Answer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := ReadFrames(stdout, each)
		done <- res{a, err}
	}()
	for {
		select {
		case <-ctx.Done():
			_ = s.Close()
			return Answer{}, ctx.Err()
		case err := <-sent:
			if err != nil {
				_ = s.Close()
				return Answer{}, err
			}
			sent = nil
		case r := <-done:
			if r.err != nil {
				// The reader stopped early (each refused a frame, or the
				// caller is done with the job): the session is closed, not
				// waited for, since its input may still be open.
				_ = s.Close()
			} else {
				_ = s.Wait()
			}
			return r.a, r.err
		}
	}
}

// Restriction is one thing a run key must not be able to do, and whether
// the server refused it.
type Restriction struct {
	What    string `json:"what"`
	Refused bool   `json:"refused"`
	Detail  string `json:"detail"`
}

// CheckRestrictions tries, over the run key, what the key must never do: run
// a command of its own, open a shell, take a terminal, and open a TCP channel
// through the server. Each must be refused.
func (c *Client) CheckRestrictions(ctx context.Context) []Restriction {
	var out []Restriction

	// A command of its own: the forced command runs instead, and serve
	// refuses it.
	if s, err := c.ssh.NewSession(); err == nil {
		var so bytes.Buffer
		s.Stdout = &so
		err := s.Run("id")
		a, _ := ReadAnswer(bytes.NewReader(so.Bytes()), nil)
		refused := err != nil && !strings.Contains(so.String(), "uid=") && a.Code == 2
		out = append(out, Restriction{What: "a command of its own (id)", Refused: refused, Detail: strings.TrimSpace(firstLine(nonEmptyStr(a.Error, so.String())))})
		_ = s.Close()
	}

	// A terminal: restrict takes away the pty.
	if s, err := c.ssh.NewSession(); err == nil {
		err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{})
		out = append(out, Restriction{What: "a terminal", Refused: err != nil, Detail: errString(err, "the server granted a terminal")})
		_ = s.Close()
	}

	// A shell: the forced command runs instead, and serve refuses it.
	if s, err := c.ssh.NewSession(); err == nil {
		var so bytes.Buffer
		s.Stdout = &so
		s.Stdin = strings.NewReader("id\nexit\n")
		_ = s.Shell()
		werr := waitTimeout(s, 10*time.Second)
		a, _ := ReadAnswer(bytes.NewReader(so.Bytes()), nil)
		refused := werr != nil && !strings.Contains(so.String(), "uid=") && a.Code == 2
		out = append(out, Restriction{What: "a shell", Refused: refused, Detail: strings.TrimSpace(firstLine(nonEmptyStr(a.Error, so.String())))})
		_ = s.Close()
	}

	// Port forwarding: restrict refuses the direct-tcpip channel.
	{
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := c.ssh.DialContext(dctx, "tcp", "127.0.0.1:22")
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		out = append(out, Restriction{What: "a TCP channel through the server (port forwarding)", Refused: err != nil, Detail: errString(err, "the server opened the channel")})
	}

	// A listener on the server: restrict refuses remote forwarding.
	{
		l, err := c.ssh.Listen("tcp", "127.0.0.1:0")
		if l != nil {
			_ = l.Close()
		}
		out = append(out, Restriction{What: "a listener on the server (remote forwarding)", Refused: err != nil, Detail: errString(err, "the server opened a listener")})
	}
	return out
}

func waitTimeout(s *ssh.Session, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = s.Close()
		return errors.New("timed out")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func nonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func errString(err error, otherwise string) string {
	if err != nil {
		return err.Error()
	}
	return otherwise
}
