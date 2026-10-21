package portrelay

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
)

func TestPipeCarriesAConnectionBothWays(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		got, _ := io.ReadAll(c)
		_, _ = c.Write(append([]byte("echo: "), got...))
	}()
	var out bytes.Buffer
	if err := Pipe(l.Addr().(*net.TCPAddr).Port, strings.NewReader("GET / HTTP/1.0\r\n\r\n"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "echo: GET / HTTP/1.0\r\n\r\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestPipeSaysNothingAnswers(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	err = Pipe(port, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "nothing answers") {
		t.Errorf("err = %v", err)
	}
}

func TestParsePortRefusesPrivilegedAndJunk(t *testing.T) {
	for _, s := range []string{"80", "0", "65536", "web", "-1", ""} {
		if _, err := ParsePort(s); err == nil {
			t.Errorf("%q was taken", s)
		}
	}
	if p, err := ParsePort("3000"); err != nil || p != 3000 {
		t.Errorf("3000 = %d, %v", p, err)
	}
}
