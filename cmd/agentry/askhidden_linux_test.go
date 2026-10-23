package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// openPty opens a pseudo-terminal: its controlling side and the terminal a
// program reads from.
func openPty(t *testing.T) (ptmx, tty *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlock the pseudo-terminal: %v", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("name the pseudo-terminal: %v", err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return m, s
}

func echoOn(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

// TestAskHiddenReadsWithoutEchoAndAnInterruptEndsIt: a value typed at the
// prompt is read and never echoed; an interrupt (the command's context
// cancelled, as Ctrl-C does) ends the read with nothing read and the
// terminal's echo back on.
func TestAskHiddenReadsWithoutEchoAndAnInterruptEndsIt(t *testing.T) {
	ptmx, tty := openPty(t)
	if !echoOn(t, tty) {
		t.Fatal("the terminal starts with its echo off")
	}
	const value = "typed-value-0123456789"
	cmd := &cobra.Command{}
	cmd.SetIn(tty)
	cmd.SetErr(tty)
	cmd.SetContext(context.Background())
	seen := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 256)
		for {
			n, err := ptmx.Read(buf)
			b.Write(buf[:n])
			if err != nil || strings.Contains(b.String(), "Second (not shown):") {
				break
			}
		}
		seen <- b.String()
	}()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = ptmx.Write([]byte(value + "\r"))
	}()
	got, err := askHidden(cmd, "First (not shown): ")
	if err != nil || got != value {
		t.Fatalf("askHidden = %q, %v", got, err)
	}
	if !echoOn(t, tty) {
		t.Error("the echo is off after a value was read")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		got, err := askHidden(cmd, "Second (not shown): ")
		if got != "" {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("an interrupted read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an interrupt did not end the read")
	}
	if !echoOn(t, tty) {
		t.Error("the echo is off after an interrupt")
	}
	select {
	case out := <-seen:
		if strings.Contains(out, value) {
			t.Errorf("the terminal echoed the value: %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Error("the terminal showed no second prompt")
	}
}
