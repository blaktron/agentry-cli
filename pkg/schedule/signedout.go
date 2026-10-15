package schedule

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Running while signed out. On Linux with Docker Engine a schedule can also
// run while its person is signed out: linger starts their systemd user
// manager at boot and keeps it after sign-out, so the same user timer rings
// with nobody signed in. The keychain is locked then, so the keys a fire
// reads are sealed, when the schedule is made, into an encrypted systemd
// credential beside it (`systemd-creds encrypt --user`, bound to this
// machine, its TPM where there is one, and this user), which the unit loads
// with LoadCredentialEncrypted=. There is no plaintext copy and no fallback:
// below systemd 256 the option is refused.

// ErrSignedOutUnsupported is the system's reason a schedule cannot run
// while its person is signed out.
var ErrSignedOutUnsupported = errors.New("running while signed out is not available here: Docker Desktop stops when you sign out")

// MinSystemd is the oldest systemd whose `systemd-creds encrypt --user`
// seals a credential to one user, which a user manager can then load.
const MinSystemd = 256

// CredentialFile is a signed-out schedule's encrypted credential, in its
// folder in the store.
const CredentialFile = "keys.cred"

// CredentialPath is where sc's encrypted credential is kept.
func (s Store) CredentialPath(id string) (string, error) {
	d, err := s.dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, CredentialFile), nil
}

// PendingCredentialPath is a fresh path in the store for a credential
// sealed before its schedule has an id (add probes it first); MoveCredential
// gives it to the schedule.
func (s Store) PendingCredentialPath() (string, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	// An add killed before it kept its schedule leaves its pending
	// credential (ciphertext); one older than an hour is no add's any more.
	old, _ := filepath.Glob(filepath.Join(s.Dir, ".pending-*.cred"))
	for _, p := range old {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
			_ = os.Remove(p)
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, ".pending-"+hex.EncodeToString(b)+".cred"), nil
}

// MoveCredential makes the credential at from id's own.
func (s Store) MoveCredential(from, id string) error {
	to, err := s.CredentialPath(id)
	if err != nil {
		return err
	}
	return os.Rename(from, to)
}

// SealAccounts are the keychain accounts a fire under pol reads: the model
// key's, required, when the policy takes it from the keychain (an adapter's
// own block may say so where the model's does not), and the person's own
// provider keys the policy takes from the keychain (AI Review's, the search
// provider's, Hybrid Analysis's), optional, as a run signed in goes on
// without one the keychain does not hold.
func SealAccounts(pol *policy.Policy) (required, optional []string) {
	keyring := policy.KeySource(policy.HarnessPolicy{}, pol.Model) == modelkey.SourceKeyring
	for _, hp := range pol.Harness {
		keyring = keyring || policy.KeySource(hp, pol.Model) == modelkey.SourceKeyring
	}
	if keyring && pol.Model.Provider != "" {
		required = append(required, modelkey.Account(pol.Model.Provider))
	}
	if pol.Judge.APIKeySource == modelkey.SourceKeyring {
		optional = append(optional, modelkey.Account(policy.JudgeKeyProvider))
	}
	if pol.WebSearch.On() && pol.WebSearch.APIKeySource == modelkey.SourceKeyring {
		optional = append(optional, modelkey.Account(pol.WebSearch.ProviderName()))
	}
	if pol.HybridAnalysis.APIKeySource == modelkey.SourceKeyring {
		optional = append(optional, modelkey.Account(policy.HybridKeyProvider))
	}
	return required, optional
}

// EncryptCredential runs `systemd-creds encrypt --user`, handing it the
// plaintext on its stdin (never an argument, never a file in the clear);
// replaced in tests.
var EncryptCredential = func(name string, plaintext []byte, out string) error {
	cmd := exec.Command("systemd-creds", "encrypt", "--user", "--name="+name, "-", out)
	cmd.Stdin = bytes.NewReader(plaintext)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-creds encrypt --user: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// SealCredential encrypts values into path, readable by this user alone:
// through a temporary file beside it and one rename, so a fire never loads
// half of one.
func SealCredential(path string, values map[string]string) error {
	plain, err := modelkey.SealPlaintext(values)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := EncryptCredential(modelkey.CredentialName, plain, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// loadCredential is a unit's LoadCredentialEncrypted= setting for path.
func loadCredential(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\n\r") {
		return "", fmt.Errorf("the credential %q is not an absolute path on one line", path)
	}
	return "LoadCredentialEncrypted=" + modelkey.CredentialName + ":" + path, nil
}

// SystemdVersion is the running systemd's version; replaced in tests.
var SystemdVersion = func() (int, error) {
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl --version: %w", err)
	}
	return parseSystemdVersion(string(out))
}

var systemdVersionExpr = regexp.MustCompile(`^systemd (\d+)`)

func parseSystemdVersion(out string) (int, error) {
	m := systemdVersionExpr.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, fmt.Errorf("systemctl --version said %q", firstLine(out))
	}
	return strconv.Atoi(m[1])
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// Linger is the person's systemd linger: on, their user manager starts at
// boot and stays after they sign out, which is what lets a user timer ring
// with nobody signed in.
type Linger struct {
	// User is the person's login name.
	User string
	// Dir is where logind keeps a file per lingering user
	// (/var/lib/systemd/linger), which says whether linger is on without
	// asking logind for a user it may not know.
	Dir string
	// Loginctl runs loginctl with args, with in and errOut for polkit's
	// question where the system asks one; replaced in tests.
	Loginctl func(in io.Reader, errOut io.Writer, args ...string) error
}

// NewLinger is the linger of user, set through the real loginctl.
func NewLinger(user string) *Linger {
	return &Linger{User: user, Dir: "/var/lib/systemd/linger", Loginctl: func(in io.Reader, errOut io.Writer, args ...string) error {
		cmd := exec.Command("loginctl", args...)
		cmd.Stdin, cmd.Stderr = in, errOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("loginctl %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}}
}

// On reports whether linger is on for the person.
func (l *Linger) On() (bool, error) {
	_, err := os.Stat(filepath.Join(l.Dir, l.User))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// lingerRecord is the store's note that the CLI turned linger on, so it
// turns it off again only then.
type lingerRecord struct {
	Ours  bool      `json:"ours"`
	Since time.Time `json:"since"`
}

func (s Store) lingerRecordPath() string { return filepath.Join(s.Dir, "linger.json") }

func (s Store) lingerRecord() (lingerRecord, error) {
	var r lingerRecord
	b, err := os.ReadFile(s.lingerRecordPath())
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// EnableLinger turns linger on for the person if it is off, and records
// that it was this CLI's doing. Linger the person turned on themselves is
// left as it is, and never recorded as the CLI's. It reports whether it
// turned linger on now.
func (s Store) EnableLinger(l *Linger, in io.Reader, errOut io.Writer, now time.Time) (bool, error) {
	on, err := l.On()
	if err != nil {
		return false, err
	}
	if on {
		return false, nil
	}
	if err := l.Loginctl(in, errOut, "enable-linger"); err != nil {
		return false, err
	}
	if on, err := l.On(); err != nil || !on {
		return false, fmt.Errorf("loginctl enable-linger ran, but linger is not on for %s", l.User)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return true, err
	}
	b, err := json.Marshal(lingerRecord{Ours: true, Since: now.UTC()})
	if err != nil {
		return true, err
	}
	return true, writeAtomic(s.lingerRecordPath(), b, 0o600)
}

// SignedOutLeft counts the schedules that still run signed out: made so,
// and not ended (a paused one counts: it runs signed out again when
// resumed).
func (s Store) SignedOutLeft() (int, error) {
	list, _, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sc := range list {
		if !sc.SignedOut {
			continue
		}
		if st, _ := s.State(sc.ID); st.Ended {
			continue
		}
		n++
	}
	return n, nil
}

// ReleaseLinger turns linger off when no schedule runs signed out any
// more and the CLI was the one that turned it on; otherwise it leaves
// linger as it is. It reports whether it turned linger off.
func (s Store) ReleaseLinger(l *Linger, in io.Reader, errOut io.Writer) (bool, error) {
	rec, err := s.lingerRecord()
	if err != nil || !rec.Ours {
		return false, err
	}
	left, err := s.SignedOutLeft()
	if err != nil || left > 0 {
		return false, err
	}
	if on, err := l.On(); err != nil {
		return false, err
	} else if on {
		if err := l.Loginctl(in, errOut, "disable-linger"); err != nil {
			return false, err
		}
	}
	if err := os.Remove(s.lingerRecordPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	return true, nil
}
