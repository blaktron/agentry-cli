package runhost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Handler answers one verb. in is the rest of the device's input after the
// request line (a job's document and release, for job). It writes its
// narration and result to fw and returns the exit code and, when it is not
// 0, why.
type Handler func(ctx context.Context, req Request, in io.Reader, fw *FrameWriter) (int, error)

// Service is the runhost's service: an owner-only Unix socket and the
// handlers of the verbs it answers.
type Service struct {
	Socket   string
	Handlers map[string]Handler
	// Log, when set, is told of each request and of failures to answer.
	Log func(format string, args ...any)

	wg sync.WaitGroup
}

// Listen opens the socket, owner-only: a stale socket from an earlier run is
// removed first, and the socket is created under a umask that gives no one
// else any bit, so there is no moment when another user could connect.
func (s *Service) Listen() (net.Listener, error) {
	dir := filepath.Dir(s.Socket)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("the service's runtime directory: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is open to other users (%v): the service listens only in a directory of its own", dir, st.Mode().Perm())
	}
	if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := listenOwnerOnly(s.Socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.Socket, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Serve answers connections on l until ctx is done, then waits for the
// answers in flight.
func (s *Service) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.answer(ctx, conn)
		}()
	}
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func (s *Service) answer(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	fw := NewFrameWriter(conn)
	br := bufio.NewReaderSize(conn, 64<<10)
	req, err := ReadRequest(br)
	if err != nil {
		_ = fw.Exit(2, err)
		return
	}
	h, ok := s.Handlers[req.Verb]
	if !ok {
		_ = fw.Exit(2, fmt.Errorf("this runhost does not answer %q yet: update it", req.Verb))
		return
	}
	s.logf("request: %s", req.Verb)
	code, err := h(ctx, req, br, fw)
	if err != nil {
		s.logf("%s: %v", req.Verb, err)
	}
	if werr := fw.Exit(code, err); werr != nil {
		s.logf("%s: the answer did not reach the device: %v", req.Verb, werr)
	}
}

// ListenOwnerOnly listens on a Unix socket at path that only this user can
// connect to, from its first moment.
func ListenOwnerOnly(path string) (net.Listener, error) {
	l, err := listenOwnerOnly(path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}
