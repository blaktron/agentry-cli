package persistent

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A supervisor that stops and closes its socket before its answer is
// written has stopped the agent when its lock is free, and has not while
// the lock is held.
func TestAStopThatClosesWithoutAnAnswerIsJudgedByTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the owner socket is a Unix socket")
	}
	home := t.TempDir()
	if err := os.MkdirAll(Dir(home, "assistant"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := SocketPath(home, "assistant")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	prev := stopSettle
	stopSettle = 300 * time.Millisecond
	t.Cleanup(func() { stopSettle = prev })
	// The supervisor: reads the command, then closes without answering.
	serve := func(t *testing.T) func() {
		t.Helper()
		_ = os.Remove(sock)
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadString('\n')
			_ = c.Close()
		}()
		return func() { _ = l.Close() }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	owner, err := Acquire(home, "assistant", ClientCLI)
	if err != nil {
		t.Fatal(err)
	}
	stop := serve(t)
	if _, err := RequestStop(ctx, home, "assistant"); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a held lock after no answer: %v, want ErrNoAnswer", err)
	}
	stop()

	owner.Release()
	stop = serve(t)
	defer stop()
	st, err := RequestStop(ctx, home, "assistant")
	if err != nil || st.Status != "stopped" {
		t.Fatalf("a free lock after no answer: %+v %v, want stopped", st, err)
	}
}
