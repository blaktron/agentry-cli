package targethost

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The lines a device hands a runhost are those naming the host, written
// plainly, hashed, with a port, or marked revoked; another host's never.
func TestKnownLinesAreTheHostsAlone(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	dir := t.TempDir()
	persons, runners := filepath.Join(dir, "persons"), filepath.Join(dir, "runners")
	plain := "box.test " + key
	hashed := knownhosts.HashHostname("box.test") + " " + key
	ported := "[box.test]:2222 " + key
	revoked := "@revoked box.test " + key
	other := "other.test,box.test.evil " + key
	_ = os.WriteFile(persons, []byte("# a comment\n"+plain+"\n"+other+"\nnot a line\n"+revoked+"\n"), 0o600)
	_ = os.WriteFile(runners, []byte(hashed+"\n"+ported+"\n"), 0o600)
	files := KnownHostsFiles{Persons: persons, Runners: runners}

	got, err := KnownLines(files, Answer{User: "root", Host: "box.test", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join([]string{plain, revoked, hashed}, "\n") {
		t.Errorf("port 22:\n%s", strings.Join(got, "\n"))
	}
	got, _ = KnownLines(files, Answer{User: "root", Host: "BOX.test", Port: 2222})
	if strings.Join(got, "\n") != ported {
		t.Errorf("port 2222:\n%s", strings.Join(got, "\n"))
	}
	if got, _ := KnownLines(KnownHostsFiles{Persons: filepath.Join(dir, "none")}, Answer{Host: "box.test", Port: 22}); len(got) != 0 {
		t.Errorf("a missing file gave %v", got)
	}
}
