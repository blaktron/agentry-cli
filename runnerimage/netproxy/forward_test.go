package netproxy

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// TestForwardPipesBytesBothWays: the forwarder is a plain TCP pipe — what
// the host writes reaches the target and the target's answer comes back —
// so a host-run harness can reach the gateway through the proxy container
// without the gateway ever leaving the run network.
func TestForwardPipesBytesBothWays(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = c.Write([]byte("echo:" + line))
			}(c)
		}
	}()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Forward(ctx, front, target.Addr().String(), nil) }()

	c, err := net.DialTimeout("tcp", front.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != "echo:ping\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// TestForwardDialFailureClosesTheClient: an unreachable target closes the
// accepted connection instead of leaving the harness hanging.
func TestForwardDialFailureClosesTheClient(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dead.Addr().String()
	_ = dead.Close()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Forward(ctx, front, addr, nil) }()

	c, err := net.DialTimeout("tcp", front.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("client read succeeded against a dead target; expected the connection to be closed")
	}
}

// TestParseForward pins the LISTEN=TARGET flag grammar.
func TestParseForward(t *testing.T) {
	l, tgt, err := ParseForward(":8811=gateway:8811")
	if err != nil || l != ":8811" || tgt != "gateway:8811" {
		t.Fatalf("ParseForward = %q %q %v", l, tgt, err)
	}
	for _, bad := range []string{"", ":8811", "=gateway:8811", ":8811=", "a=b=c"} {
		if _, _, err := ParseForward(bad); err == nil {
			t.Errorf("ParseForward(%q) accepted", bad)
		}
	}
}
