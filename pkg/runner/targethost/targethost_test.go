package targethost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

func init() {
	localUser = func() string { return "me" }
}

func TestParseAnswer(t *testing.T) {
	for in, want := range map[string]Answer{
		"box":                     {User: "me", Host: "box", Port: 22},
		"admin@box.example.com":   {User: "admin", Host: "box.example.com", Port: 22},
		"admin@203.0.113.4:2222":  {User: "admin", Host: "203.0.113.4", Port: 2222},
		"root@[2001:db8::1]:2200": {User: "root", Host: "2001:db8::1", Port: 2200},
		"[2001:db8::1]":           {User: "me", Host: "2001:db8::1", Port: 22},
		" ops@host. ":             {User: "ops", Host: "host", Port: 22},
	} {
		got, err := ParseAnswer(in)
		if err != nil || got != want {
			t.Errorf("ParseAnswer(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "admin:hunter2@box", "ssh://box", "box:99999", "box:x", "2001:db8::1", "[2001:db8::1", "-oProxyCommand=x@box",
		"box/etc", "bad_host!", "@box", "a b"} {
		if got, err := ParseAnswer(bad); err == nil {
			t.Errorf("ParseAnswer(%q) = %+v, want a refusal", bad, got)
		}
	}
	if _, err := ParseAnswer("admin:hunter2@box"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a password in the answer is refused without repeating it: %v", err)
	}
	a := Answer{User: "admin", Host: "Box.Example", Port: 2222}
	if a.String() != "admin@Box.Example:2222" || a.KnownHostsName() != "[Box.Example]:2222" || a.Account() != "host:admin@box.example:2222" {
		t.Errorf("%s %s %s", a, a.KnownHostsName(), a.Account())
	}
}

func TestOnlyLoopbackAndLinkLocalAndNoHostAreRefused(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1": false, "127.9.9.9": false, "169.254.169.254": false, "0.0.0.0": false, "0.1.2.3": false,
		"224.0.0.1": false, "255.255.255.255": false, "::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
		"10.1.2.3": true, "192.168.1.10": true, "100.101.102.103": true, "172.17.0.1": true, "203.0.113.4": true,
	} {
		if err := CheckAddress(netip.MustParseAddr(addr)); (err == nil) != ok {
			t.Errorf("CheckAddress(%s) = %v, want ok %v", addr, err, ok)
		}
	}
}

func TestResolveAddressUsesThisMachinesResolver(t *testing.T) {
	defer func(f func(context.Context, string) ([]netip.Addr, error)) { lookupIP = f }(lookupIP)
	answers := map[string][]netip.Addr{
		"box.tail":   {netip.MustParseAddr("fd7a:115c::5"), netip.MustParseAddr("100.101.102.103")},
		"local.test": {netip.MustParseAddr("127.0.0.1")},
		"v6.only":    {netip.MustParseAddr("2001:db8::1")},
	}
	lookupIP = func(_ context.Context, h string) ([]netip.Addr, error) {
		if a, ok := answers[h]; ok {
			return a, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
	}
	got, err := ResolveAddress(context.Background(), "server", Answer{Host: "box.tail", Port: 22}, policy.HostsPolicy{})
	if err != nil || got.String() != "100.101.102.103" {
		t.Fatalf("ResolveAddress = %v, %v", got, err)
	}
	for host, rule := range map[string]policy.HostsPolicy{
		"local.test": {}, "v6.only": {}, "nope.example": {}, "169.254.169.254": {},
		"box.tail": {Deny: []string{"100.64.0.0/10"}},
	} {
		_, err := ResolveAddress(context.Background(), "server", Answer{Host: host, Port: 22}, rule)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedAddress || r.ID != "server" {
			t.Errorf("%s: %v", host, err)
		}
	}
}

func newHostKey(t *testing.T) (ssh.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, priv
}

type fakeAsk struct {
	answers []string
	confirm bool
	asked   []string
}

func (f *fakeAsk) next(p string) (string, error) {
	f.asked = append(f.asked, p)
	if len(f.answers) == 0 {
		return "", nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}
func (f *fakeAsk) Ask(p, _ string) (string, error)    { return f.next(p) }
func (f *fakeAsk) AskSecret(p string) (string, error) { return f.next(p) }
func (f *fakeAsk) Confirm(p string, _ bool) (bool, error) {
	f.asked = append(f.asked, p)
	return f.confirm, nil
}

func TestHostKeys(t *testing.T) {
	served, _ := newHostKey(t)
	other, _ := newHostKey(t)
	defer func(f func(context.Context, netip.Addr, int, []string) (ssh.PublicKey, error)) { fetchHostKey = f }(fetchHostKey)
	fetchHostKey = func(context.Context, netip.Addr, int, []string) (ssh.PublicKey, error) { return served, nil }
	a := Answer{User: "admin", Host: "box.example", Port: 2222}
	addr := netip.MustParseAddr("203.0.113.4")
	say := func(string) {}

	t.Run("the person's file, hashed, is trusted and never written", func(t *testing.T) {
		dir := t.TempDir()
		f := KnownHostsFiles{Persons: filepath.Join(dir, "person"), Runners: filepath.Join(dir, "runner")}
		line := knownhosts.Line([]string{knownhosts.HashHostname(a.KnownHostsName())}, served)
		body := "# mine\nsomething we cannot parse at all\n" + line + "\n"
		writeFile(t, f.Persons, body)
		k, err := CheckHostKey(context.Background(), "server", a, addr, f, nil, say)
		if err != nil || k.From != TrustPersonsFile {
			t.Fatalf("CheckHostKey = %+v, %v", k, err)
		}
		if b, _ := os.ReadFile(f.Persons); string(b) != body {
			t.Error("the person's known_hosts was written")
		}
	})
	t.Run("a changed key refuses with both fingerprints", func(t *testing.T) {
		dir := t.TempDir()
		f := KnownHostsFiles{Persons: filepath.Join(dir, "person"), Runners: filepath.Join(dir, "runner")}
		writeFile(t, f.Runners, knownhosts.Line([]string{a.KnownHostsName()}, other)+"\n")
		_, err := CheckHostKey(context.Background(), "server", a, addr, f, &fakeAsk{confirm: true}, say)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedKeyChanged ||
			!strings.Contains(err.Error(), ssh.FingerprintSHA256(other)) || !strings.Contains(err.Error(), ssh.FingerprintSHA256(served)) {
			t.Fatalf("a changed key: %v", err)
		}
	})
	t.Run("an unknown key with no one to ask refuses", func(t *testing.T) {
		dir := t.TempDir()
		f := KnownHostsFiles{Persons: filepath.Join(dir, "none"), Runners: filepath.Join(dir, "runner")}
		_, err := CheckHostKey(context.Background(), "server", a, addr, f, nil, say)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedKeyUnknown || !strings.Contains(err.Error(), ssh.FingerprintSHA256(served)) {
			t.Fatalf("an unknown key: %v", err)
		}
	})
	t.Run("an unknown key a person trusts is kept in the runner's file", func(t *testing.T) {
		dir := t.TempDir()
		f := KnownHostsFiles{Persons: filepath.Join(dir, "person"), Runners: filepath.Join(dir, "home", "known_hosts")}
		ask := &fakeAsk{confirm: true}
		k, err := CheckHostKey(context.Background(), "server", a, addr, f, ask, say)
		if err != nil || k.From != TrustedNow || len(ask.asked) != 1 || !strings.Contains(ask.asked[0], ssh.FingerprintSHA256(served)) {
			t.Fatalf("trust: %+v, %v, %q", k, err, ask.asked)
		}
		k, err = CheckHostKey(context.Background(), "server", a, addr, f, nil, say)
		if err != nil || k.From != TrustRunnersFile {
			t.Fatalf("the kept key: %+v, %v", k, err)
		}
		if _, err := os.Stat(f.Persons); !errors.Is(err, os.ErrNotExist) {
			t.Error("the person's file was made")
		}
		if fi, err := os.Stat(f.Runners); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the runner's file: %v %v", fi.Mode(), err)
		}
	})
	t.Run("a declined key refuses", func(t *testing.T) {
		dir := t.TempDir()
		f := KnownHostsFiles{Runners: filepath.Join(dir, "runner")}
		_, err := CheckHostKey(context.Background(), "server", a, addr, f, &fakeAsk{}, say)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedKeyUnknown {
			t.Fatalf("declined: %v", err)
		}
	})
	if got := string(HostLinkKnownHosts(served)); !strings.HasPrefix(got, hostlink.HostKeyAlias+" ssh-ed25519 ") {
		t.Errorf("the host link's known_hosts: %q", got)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeKey(t *testing.T, dir, passphrase string) string {
	t.Helper()
	_, priv := newHostKey(t)
	var blk *pem.Block
	var err error
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "id_test")
	writeFile(t, p, string(pem.EncodeToMemory(blk)))
	return p
}

func fakeKeychain(t *testing.T) map[string]string {
	t.Helper()
	t.Setenv(modelkey.EnvNoKeyring, "")
	store := map[string]string{}
	g, s := keyringGet, keyringSet
	t.Cleanup(func() { keyringGet, keyringSet = g, s })
	keyringGet = func(svc, acct string) (string, error) {
		if v, ok := store[svc+"/"+acct]; ok {
			return v, nil
		}
		return "", keyring.ErrNotFound
	}
	keyringSet = func(svc, acct, v string) error { store[svc+"/"+acct] = v; return nil }
	return store
}

func TestCredentials(t *testing.T) {
	a := Answer{User: "admin", Host: "box", Port: 22}
	env := map[string]string{"PW": "hunter2-hunter2", "PP": "open sesame", "SUDO": "root-pw"}
	getenv := func(k string) string { return env[k] }
	warn := func(string) {}

	t.Run("a locked key and sudo from flags", func(t *testing.T) {
		fakeKeychain(t)
		key := writeKey(t, t.TempDir(), "open sesame")
		f, err := ParseFlags([]string{"server=" + key}, []string{"server=PP"}, nil, []string{"server=SUDO"})
		if err != nil {
			t.Fatal(err)
		}
		c, err := ResolveCredential("server", a, f, getenv, nil, warn)
		if err != nil || c.Auth != hostlink.AuthKey || c.Passphrase != "open sesame" || c.Sudo != "root-pw" || c.From != FromFlag {
			t.Fatalf("%+v, %v", c, err)
		}
		if w := c.Words(); !strings.Contains(w, key) || strings.Contains(w, "open sesame") || strings.Contains(w, "root-pw") {
			t.Errorf("words %q", w)
		}
	})
	t.Run("a wrong passphrase refuses", func(t *testing.T) {
		fakeKeychain(t)
		key := writeKey(t, t.TempDir(), "right one")
		f, _ := ParseFlags([]string{"server=" + key}, []string{"server=PP"}, nil, nil)
		_, err := ResolveCredential("server", a, f, getenv, nil, warn)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedCredential {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no sign-in and no terminal refuses", func(t *testing.T) {
		fakeKeychain(t)
		_, err := ResolveCredential("server", a, Flags{}, getenv, nil, warn)
		var r *Refusal
		if !errors.As(err, &r) || r.Detail != RefusedCredential || !strings.Contains(err.Error(), "--host-key server=PATH") {
			t.Fatalf("%v", err)
		}
		f, _ := ParseFlags(nil, nil, []string{"server=UNSET"}, nil)
		if _, err := ResolveCredential("server", a, f, getenv, nil, warn); !errors.As(err, &r) {
			t.Fatalf("an unset variable: %v", err)
		}
	})
	t.Run("a typed password is saved when the person says so, and read back", func(t *testing.T) {
		store := fakeKeychain(t)
		ask := &fakeAsk{answers: []string{"", "typed-pw", "typed-sudo"}, confirm: true}
		c, err := ResolveCredential("server", a, Flags{}, getenv, ask, warn)
		if err != nil || c.Auth != hostlink.AuthPassword || c.Password != "typed-pw" || c.Sudo != "typed-sudo" || c.From != FromPrompt {
			t.Fatalf("%+v, %v", c, err)
		}
		var s saved
		if err := json.Unmarshal([]byte(store[modelkey.Service+"/host:admin@box:22"]), &s); err != nil || s.Password != "typed-pw" || s.SudoPassword != "typed-sudo" {
			t.Fatalf("saved %q", store)
		}
		c, err = ResolveCredential("server", a, Flags{}, getenv, nil, warn)
		if err != nil || c.Password != "typed-pw" || c.From != FromKeychain || c.SudoFrom != FromKeychain {
			t.Fatalf("read back: %+v, %v", c, err)
		}
	})
	t.Run("a key path at the prompt", func(t *testing.T) {
		fakeKeychain(t)
		key := writeKey(t, t.TempDir(), "")
		ask := &fakeAsk{answers: []string{key, ""}}
		c, err := ResolveCredential("server", a, Flags{}, getenv, ask, warn)
		if err != nil || c.Auth != hostlink.AuthKey || c.KeyFile != key || len(c.Key) == 0 || c.Sudo != "" {
			t.Fatalf("%+v, %v", c, err)
		}
	})
	for _, bad := range [][]string{{"server"}, {"server="}, {"Server=x"}, {"server=a", "server=b"}} {
		if _, err := ParseFlags(nil, bad, nil, nil); err == nil {
			t.Errorf("--host-passphrase %q accepted", bad)
		}
	}
	if _, err := ParseFlags(nil, []string{"server=open sesame"}, nil, nil); err == nil || strings.Contains(err.Error(), "sesame") {
		t.Errorf("a value given as the variable: %v", err)
	}
	if _, err := ParseFlags([]string{"server=/k"}, nil, []string{"server=PW"}, nil); err == nil {
		t.Error("a key and a password for one host were both accepted")
	}
}
