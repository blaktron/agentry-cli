package hostpin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestUpstreamsSkipTheLocalStub: a resolv.conf naming only a local stub (as
// systemd-resolved's does) gives way to the next file, and loopback, IPv6
// and repeated servers are left out, three at most.
func TestUpstreamsSkipTheLocalStub(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.conf")
	upstream := filepath.Join(dir, "upstream.conf")
	must(t, os.WriteFile(stub, []byte("nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch example.com\n"), 0o600))
	must(t, os.WriteFile(upstream, []byte("# upstream\nnameserver 192.168.1.1\nnameserver fd00::1\nnameserver 192.168.1.1\nnameserver 0.0.0.0\nnameserver 1.1.1.1\nnameserver 9.9.9.9\nnameserver 8.8.8.8\n"), 0o600))
	old := resolvConfs
	t.Cleanup(func() { resolvConfs = old })

	resolvConfs = []string{stub, upstream}
	if got, want := Upstreams(), []string{"192.168.1.1", "1.1.1.1", "9.9.9.9"}; !slices.Equal(got, want) {
		t.Errorf("Upstreams() = %v, want %v", got, want)
	}
	resolvConfs = []string{upstream, stub}
	if got := Upstreams(); len(got) != 3 || got[0] != "192.168.1.1" {
		t.Errorf("the first file that names a server wins: %v", got)
	}
	resolvConfs = []string{stub, filepath.Join(dir, "missing.conf")}
	if got := Upstreams(); got != nil {
		t.Errorf("only a local stub named: %v, want none", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
