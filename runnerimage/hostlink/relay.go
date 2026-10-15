package hostlink

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Finding the relay. The gateway starts one L4 relay a target host, from
// the host link's catalog entry (allowHosts "<address>:<port>/tcp"), and
// links it to the host link under the address itself: the relay forwards
// to that address, and on the host link's network the same string names
// the relay. But the string is an IP address, which no resolver looks up:
// dialled as it is, it goes nowhere (the network is internal). So the host
// link asks for it as a name, as docker's link answers it:
//
//   - in /etc/hosts, where the run's docker filter pins each link of a
//     container that runs under a sandboxed runtime (pinLinks; its network
//     stack does not reach docker's DNS);
//   - else of the container's name server (docker's embedded DNS), with an
//     A query for the address as a name, which no library resolver makes
//     for an address (docker answers it with the
//     relay's address).

// relayLookup finds the relay of a target host's address; a variable so a
// test can stand in for docker's network.
var relayLookup = func(address string) (string, error) {
	return findRelay(address, "/etc/hosts", "/etc/resolv.conf", 5*time.Second)
}

func findRelay(alias, hostsFile, resolvConf string, timeout time.Duration) (string, error) {
	if ip, ok := hostsEntry(hostsFile, alias); ok {
		return ip, nil
	}
	servers := nameServers(resolvConf)
	if len(servers) == 0 {
		return "", fmt.Errorf("no relay for %s: no /etc/hosts entry and no name server", alias)
	}
	var last error
	for _, s := range servers {
		ip, err := queryA(s, alias, timeout)
		if err == nil {
			return ip, nil
		}
		last = err
	}
	return "", fmt.Errorf("no relay for %s: %w", alias, last)
}

// hostsEntry is the IPv4 address /etc/hosts gives name, if any.
func hostsEntry(file, name string) (string, bool) {
	f, err := os.Open(file)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		a, err := netip.ParseAddr(fields[0])
		if err != nil || !a.Is4() {
			continue
		}
		for _, n := range fields[1:] {
			if n == name {
				return a.String(), true
			}
		}
	}
	return "", false
}

// nameServers are the nameserver lines of resolv.conf, as host:53.
func nameServers(file string) []string {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil {
				out = append(out, netip.AddrPortFrom(a, 53).String())
			}
		}
	}
	return out
}

// queryA asks server, over UDP, for name's A record: one question,
// recursion desired, the answer's first A record.
func queryA(server, name string, timeout time.Duration) (string, error) {
	msg, id, err := aQuery(name)
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(msg); err != nil {
		return "", err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return firstA(buf[:n], id)
}

func aQuery(name string) ([]byte, uint16, error) {
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idb[:])
	msg := []byte{idb[0], idb[1], 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" || len(label) > 63 {
			return nil, 0, fmt.Errorf("%q is not a name a query can carry", name)
		}
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0, 0, 1, 0, 1) // root, type A, class IN
	return msg, id, nil
}

var errNoAnswer = errors.New("the name server has no address for it")

// firstA reads a response to the query id: its first A record's address.
func firstA(b []byte, id uint16) (string, error) {
	if len(b) < 12 || binary.BigEndian.Uint16(b) != id || b[2]&0x80 == 0 {
		return "", errors.New("not an answer to the query")
	}
	if rcode := b[3] & 0x0f; rcode != 0 {
		return "", fmt.Errorf("the name server answered rcode %d", rcode)
	}
	qd, an := binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:])
	off := 12
	var err error
	for range qd {
		if off, err = skipName(b, off); err != nil {
			return "", err
		}
		off += 4
	}
	for range an {
		if off, err = skipName(b, off); err != nil {
			return "", err
		}
		if off+10 > len(b) {
			return "", errors.New("a truncated answer")
		}
		typ, class := binary.BigEndian.Uint16(b[off:]), binary.BigEndian.Uint16(b[off+2:])
		rdlen := int(binary.BigEndian.Uint16(b[off+8:]))
		off += 10
		if off+rdlen > len(b) {
			return "", errors.New("a truncated answer")
		}
		if typ == 1 && class == 1 && rdlen == 4 {
			return netip.AddrFrom4([4]byte(b[off : off+4])).String(), nil
		}
		off += rdlen
	}
	return "", errNoAnswer
}

// skipName steps over a name at off: labels, ending at the root or at a
// compression pointer.
func skipName(b []byte, off int) (int, error) {
	for off < len(b) {
		l := int(b[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xc0 == 0xc0:
			return off + 2, nil
		default:
			off += 1 + l
		}
	}
	return 0, errors.New("a truncated name")
}
