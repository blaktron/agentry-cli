package hostlink

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheRelayIsFoundInHostsFirst(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n172.20.0.7\t203.0.113.4 docker-mcp-l4proxy-abc # pinned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ip, err := findRelay("203.0.113.4", hosts, filepath.Join(dir, "none"), time.Second)
	if err != nil || ip != "172.20.0.7" {
		t.Fatalf("findRelay = %q, %v", ip, err)
	}
}

// TestTheRelayIsAskedOfTheNameServerAsAName stands in for docker's embedded
// DNS, which answers a link alias that is an address as a name.
func TestTheRelayIsAskedOfTheNameServerAsAName(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		got <- q
		resp := append([]byte(nil), q...)
		resp[2], resp[3] = 0x81, 0x80
		binary.BigEndian.PutUint16(resp[6:], 2)
		// a CNAME first, then the A record, both named by pointer
		resp = append(resp, 0xc0, 12, 0, 5, 0, 1, 0, 0, 0, 10, 0, 2, 0xc0, 12)
		resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 10, 0, 4, 172, 21, 0, 2)
		_, _ = pc.WriteTo(resp, addr)
	}()
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, []byte("search example\nnameserver 127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The test's server is not on port 53: ask it directly, then check the
	// resolv.conf reading separately.
	ip, err := queryA(pc.LocalAddr().String(), "203.0.113.4", 2*time.Second)
	if err != nil || ip != "172.21.0.2" {
		t.Fatalf("queryA = %q, %v", ip, err)
	}
	q := <-got
	if want := []byte{3, '2', '0', '3', 1, '0', 3, '1', '1', '3', 1, '4', 0, 0, 1, 0, 1}; string(q[12:]) != string(want) {
		t.Errorf("the question is %v, want the address asked as a name, type A", q[12:])
	}
	if s := nameServers(resolv); len(s) != 1 || s[0] != "127.0.0.1:53" {
		t.Errorf("nameServers = %q", s)
	}
}

func TestAnAnswerForAnotherQueryIsRefused(t *testing.T) {
	msg, id, err := aQuery("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	msg[2] |= 0x80
	if _, err := firstA(msg, id+1); err == nil {
		t.Error("an answer with another id was taken")
	}
	if _, err := firstA(msg, id); err != errNoAnswer {
		t.Errorf("an answer with no record: %v", err)
	}
}
