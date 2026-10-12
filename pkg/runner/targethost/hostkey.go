package targethost

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// Host keys (plan runtime-hosts.md §6, D7). The key a target host offers
// is read at run start and looked up under the host as the person named
// it: in their own ~/.ssh/known_hosts, which is read and never written,
// then in the runner home's own file. A match is trusted, and the host
// link holds ssh to that one key. A key that differs from the one on file
// refuses the run, whatever the mode, naming both. A key on no file is
// shown to the person, and trusted only by them, at a terminal, then kept
// in the runner's file; a run with no one to ask refuses it.

// Where a host key's trust came from, as the receipt says it.
const (
	TrustPersonsFile = "your known_hosts"
	TrustRunnersFile = "Agentry's known hosts"
	TrustedNow       = "trusted at this run's start"
)

// KnownHostsFiles are the two files a host key is looked up in.
type KnownHostsFiles struct {
	// Persons is the person's ~/.ssh/known_hosts, read only; "" or absent
	// is no file.
	Persons string
	// Runners is the runner home's known_hosts, where a key trusted at a
	// run's start is kept.
	Runners string
}

// DefaultKnownHostsFiles are the person's file and the runner home's.
func DefaultKnownHostsFiles(runnerHome string) KnownHostsFiles {
	f := KnownHostsFiles{Runners: filepath.Join(runnerHome, "known_hosts")}
	if home, err := os.UserHomeDir(); err == nil {
		f.Persons = filepath.Join(home, ".ssh", "known_hosts")
	}
	return f
}

// HostKey is a target host's checked key.
type HostKey struct {
	Key  ssh.PublicKey
	From string
}

// Fingerprint is the key's type and SHA256 fingerprint, as ssh shows it.
func (k HostKey) Fingerprint() string { return Fingerprint(k.Key) }

// Fingerprint says a key as ssh-keygen -l does: its type and SHA256.
func Fingerprint(k ssh.PublicKey) string {
	return keyTypeName(k.Type()) + " " + ssh.FingerprintSHA256(k)
}

func keyTypeName(t string) string {
	switch {
	case strings.HasPrefix(t, "ssh-ed25519"):
		return "ED25519"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ECDSA"
	case strings.HasPrefix(t, "ssh-rsa"), strings.HasPrefix(t, "rsa-"):
		return "RSA"
	}
	return strings.ToUpper(t)
}

// Algorithms is the host key algorithms ssh is held to for k: its type,
// and for an RSA key the two SHA-2 signatures OpenSSH accepts.
func Algorithms(k ssh.PublicKey) string {
	if k.Type() == ssh.KeyAlgoRSA {
		return ssh.KeyAlgoRSASHA512 + "," + ssh.KeyAlgoRSASHA256
	}
	return k.Type()
}

// HostLinkKnownHosts is the one-line known_hosts the host link is given:
// the checked key under the alias ssh looks it up by.
func HostLinkKnownHosts(k ssh.PublicKey) []byte {
	return []byte(knownhosts.Line([]string{hostlink.HostKeyAlias}, k) + "\n")
}

// preferred are the host key algorithms asked for when nothing is known:
// OpenSSH's own order, without the SHA-1 RSA signature.
var preferred = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
}

// fetchHostKey reads the key addr:port offers under one of algos, with no
// credential: the handshake stops at the key. A variable for tests.
var fetchHostKey = func(ctx context.Context, addr netip.Addr, port int, algos []string) (ssh.PublicKey, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	target := net.JoinHostPort(addr.String(), strconv.Itoa(port))
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var got ssh.PublicKey
	errGot := errors.New("host key read")
	_, _, _, err = ssh.NewClientConn(conn, target, &ssh.ClientConfig{
		User:              "agentry-host-key",
		HostKeyAlgorithms: algos,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			got = k
			return errGot
		},
	})
	if got != nil {
		return got, nil
	}
	return nil, err
}

// Asker is how a person at a terminal is asked; nil is no one to ask.
type Asker interface {
	Ask(prompt, def string) (string, error)
	AskSecret(prompt string) (string, error)
	Confirm(prompt string, def bool) (bool, error)
}

// CheckHostKey reads the key the host offers at addr and decides whether
// the run may trust it (plan D7).
func CheckHostKey(ctx context.Context, id string, a Answer, addr netip.Addr, files KnownHostsFiles, ask Asker, say func(string)) (HostKey, error) {
	name := a.KnownHostsName()
	remote := &net.TCPAddr{IP: addr.AsSlice(), Port: a.Port}
	offered, err := fetchHostKey(ctx, addr, a.Port, preferred)
	if err != nil {
		return HostKey{}, refuse(id, RefusedUnreached, "could not read %s's host key at %s:%d: %v", a.Host, addr, a.Port, err)
	}
	for _, src := range []struct{ file, from string }{{files.Persons, TrustPersonsFile}, {files.Runners, TrustRunnersFile}} {
		check, err := knownHostsCallback(src.file)
		if err != nil {
			say(fmt.Sprintf("warning: %s: %v (looked past)", src.file, err))
			continue
		}
		if check == nil {
			continue
		}
		key := offered
		err = check(name, remote, key)
		var keyErr *knownhosts.KeyError
		var revoked *knownhosts.RevokedError
		switch {
		case err == nil:
			return HostKey{Key: key, From: src.from}, nil
		case errors.As(err, &revoked):
			return HostKey{}, refuse(id, RefusedKeyChanged, "%s offers the host key %s, which %s marks revoked", a.Host, Fingerprint(key), src.from)
		case errors.As(err, &keyErr) && len(keyErr.Want) > 0:
			// Keys of this host are on file. The host may hold several
			// types and have offered another: ask for the known ones.
			again, ferr := fetchHostKey(ctx, addr, a.Port, algorithmsOf(keyErr.Want))
			if ferr == nil && check(name, remote, again) == nil {
				return HostKey{Key: again, From: src.from}, nil
			}
			if ferr == nil {
				key = again
			}
			return HostKey{}, refuse(id, RefusedKeyChanged,
				"%s's host key has changed: %s holds %s, and the host offers %s. Someone may be in the way (or the host was reinstalled); the run will not reach it until the key on file is corrected",
				a.Host, src.from, Fingerprint(keyErr.Want[0].Key), Fingerprint(key))
		case errors.As(err, &keyErr):
			continue
		default:
			return HostKey{}, refuse(id, RefusedKeyChanged, "check %s's host key against %s: %v", a.Host, src.from, err)
		}
	}
	if ask == nil {
		return HostKey{}, refuse(id, RefusedKeyUnknown,
			"%s's host key (%s) is in neither your known_hosts nor Agentry's, and no one is at a terminal to trust it: run once at a terminal, or add the key to your known_hosts",
			a.Host, Fingerprint(offered))
	}
	ok, err := ask.Confirm(fmt.Sprintf("The host key of %s (%s, port %d) is not known:\n  %s\nTrust it, and keep it in Agentry's known hosts for later runs?", a.String(), addr, a.Port, Fingerprint(offered)), false)
	if err != nil {
		return HostKey{}, err
	}
	if !ok {
		return HostKey{}, refuse(id, RefusedKeyUnknown, "%s's host key (%s) was not trusted", a.Host, Fingerprint(offered))
	}
	if err := appendKnownHost(files.Runners, name, offered); err != nil {
		say(fmt.Sprintf("warning: the key is trusted for this run, but could not be kept: %v", err))
	}
	return HostKey{Key: offered, From: TrustedNow}, nil
}

func algorithmsOf(keys []knownhosts.KnownKey) []string {
	var out []string
	for _, k := range keys {
		for _, a := range strings.Split(Algorithms(k.Key), ",") {
			if !containsStr(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

func containsStr(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// knownHostsCallback reads a known_hosts file into a checker; nil when the
// file is not there. Lines this reader cannot parse (a key type it does
// not know, a marker of a newer OpenSSH) are left out rather than fail the
// file: they name some other key, never the one checked here.
func knownHostsCallback(file string) (ssh.HostKeyCallback, error) {
	if file == "" {
		return nil, nil
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var kept bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if t := bytes.TrimSpace(line); len(t) == 0 || t[0] == '#' {
			continue
		}
		if _, _, _, _, _, err := ssh.ParseKnownHosts(line); err != nil {
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}
	tmp, err := os.CreateTemp("", "agentry-known-hosts-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(kept.Bytes()); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return knownhosts.New(tmp.Name())
}

// appendKnownHost keeps a trusted key in the runner's own file, 0600.
func appendKnownHost(file, name string, k ssh.PublicKey) error {
	if file == "" {
		return errors.New("no file to keep it in")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{name}, k))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
