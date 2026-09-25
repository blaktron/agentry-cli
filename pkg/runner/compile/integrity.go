package compile

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An installed plan is reused across runs (§R6/§R9), so plan.json sits on
// disk between the compile that wrote it and the run that reads it. Its
// Files map guards the generated files (VerifyFiles), but nothing guarded
// plan.json itself: an edit to it (a writable install dir, a copy from
// elsewhere) was used as though compiled (agentry-cli#266).
//
// The integrity record closes that. Every compile given the runner home's
// key writes planDir/plan.json.hmac — an HMAC-SHA256 of plan.json's exact
// bytes under a random key that lives only in the runner home
// (plan-integrity.key, 0600), outside every install dir. A reader checks
// the record before it parses the plan:
//
//   - no record, or a record made under another key (a plan compiled by an
//     older CLI, by another runner home, or before the key was replaced):
//     the plan is not known to be this home's compile, and is recompiled.
//     Recompiling is always safe; it is what a first run does.
//   - a record under this home's key that does not match: plan.json was
//     changed after this home compiled it. The run is refused rather than
//     quietly repaired, because an edited install dir is worth a person's
//     attention.
//
// The record holds the key's id beside the MAC, so the two cases are told
// apart without guessing. Someone who can write the install dir but not
// read the key can force a recompile (by deleting or re-keying the
// record); they cannot make an edited plan pass.

// IntegrityFile is the record's name beside plan.json.
const IntegrityFile = "plan.json.hmac"

// IntegrityKeyFile is the key's name directly under the runner home.
const IntegrityKeyFile = "plan-integrity.key"

const (
	integrityKeyLen    = 32
	integrityRecordTag = "hmac-sha256"
)

// ErrNoIntegrityRecord reports an installed plan with no record this
// runner home can check: missing, unreadable, malformed, or made under
// another key. The caller recompiles.
var ErrNoIntegrityRecord = errors.New("installed plan has no integrity record from this runner home")

// TamperedPlanError reports a plan.json that does not match the record
// this runner home wrote for it.
type TamperedPlanError struct{ PlanDir string }

func (e *TamperedPlanError) Error() string {
	return fmt.Sprintf("installed plan %s does not match its integrity record: plan.json was changed after it was compiled — "+
		"refusing to run it; remove %s to recompile", filepath.Join(e.PlanDir, "plan.json"), e.PlanDir)
}

// IntegrityKey returns the runner home's plan-integrity key, creating it
// on first use. Creation is race-free across processes: the key is
// written aside and hard-linked into place, so a concurrent first run
// either wins the link or reads the winner's complete key.
func IntegrityKey(home string) ([]byte, error) {
	path := filepath.Join(home, IntegrityKeyFile)
	if key, err := readIntegrityKey(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create runner home: %w", err)
	}
	key := make([]byte, integrityKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("plan integrity key: %w", err)
	}
	tmp, err := os.CreateTemp(home, IntegrityKeyFile+".*")
	if err != nil {
		return nil, fmt.Errorf("plan integrity key: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	// CreateTemp makes the file 0600.
	if _, err := tmp.Write([]byte(hex.EncodeToString(key) + "\n")); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("plan integrity key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("plan integrity key: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		// No hard links here (some filesystems): a rename can lose a
		// first-run race, and the loser's records then name another key,
		// which is recompiled, never refused.
		if rerr := os.Rename(tmp.Name(), path); rerr != nil {
			return nil, fmt.Errorf("plan integrity key: %w", rerr)
		}
	}
	return readIntegrityKey(path)
}

func readIntegrityKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed name under the runner home
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != integrityKeyLen {
		return nil, fmt.Errorf("plan integrity key %s is malformed — remove it and re-run (installed plans are then recompiled)", path)
	}
	return key, nil
}

// integrityRecord is "hmac-sha256 <key id> <mac>\n" for plan.json's bytes.
func integrityRecord(key, planBytes []byte) string {
	return fmt.Sprintf("%s %s %s\n", integrityRecordTag, integrityKeyID(key), integrityMAC(key, planBytes))
}

func integrityMAC(key, planBytes []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(planBytes)
	return hex.EncodeToString(mac.Sum(nil))
}

// integrityKeyID names a key without revealing it.
func integrityKeyID(key []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte("agentry plan-integrity key id\x00"))
	_, _ = h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// writeIntegrityRecord records plan.json's bytes under key.
func writeIntegrityRecord(key []byte, planDir string, planBytes []byte) error {
	if err := os.WriteFile(filepath.Join(planDir, IntegrityFile), []byte(integrityRecord(key, planBytes)), 0o644); err != nil { //nolint:gosec // a MAC, not a secret
		return fmt.Errorf("write plan integrity record: %w", err)
	}
	return nil
}

// Seal records planDir/plan.json, as it is now, as this home's compile.
// Compile does this itself when given the key; Seal is for a plan.json
// rewritten by the runner's own code.
func Seal(key []byte, planDir string) error {
	b, err := os.ReadFile(filepath.Join(planDir, "plan.json")) //nolint:gosec // our install layout
	if err != nil {
		return err
	}
	return writeIntegrityRecord(key, planDir, b)
}

// readHook is a test seam: called with the name of each file
// ReadInstalledPlan has just read, so a test can interleave a concurrent
// recompile between the reads (agentry-cli#289). Nil outside tests.
var readHook func(read string)

// readAttempts bounds how often ReadInstalledPlan re-reads a pair that
// disagrees before it believes the disagreement; readBackoff is the pause
// before each retry, doubling. A real edit stays put, so refusing one
// costs these few pauses and nothing else.
const (
	readAttempts = 4
	readBackoff  = 10 * time.Millisecond
)

// ReadInstalledPlan returns planDir/plan.json's bytes only after checking
// them against the integrity record: fs.ErrNotExist when there is no
// plan, ErrNoIntegrityRecord when the record cannot vouch for it (the
// caller recompiles), *TamperedPlanError when it vouches against it (the
// caller refuses). Every read of an installed plan for execution goes
// through here.
//
// Another process may recompile the same plan directory while this one
// reads it (two runs of one release and mode started together; the CLI
// and the desktop recompile each other's plans). A plan read from one
// compile and a record from the next disagree although nothing was
// edited, and were refused as tampering (agentry-cli#289). Compile now
// publishes a whole directory in one rename and the reads go through one
// handle on the directory, so they see one compile; a client built before
// that still writes the files in place. So the record is also read on
// both sides of the plan, and a pair is judged only when the two record
// reads agree: a record that changed, vanished or appeared meanwhile
// means a recompile was in flight, and the reads are taken again. A
// mismatch between a steady record and the plan is re-read too, after a
// pause, and only a mismatch that persists through every attempt is
// reported as tampering.
func ReadInstalledPlan(key []byte, planDir string) ([]byte, error) {
	wait := readBackoff
	var last error
	for attempt := 0; attempt < readAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		b, steady, err := readPlanPair(key, planDir)
		var tampered *TamperedPlanError
		switch {
		case err == nil:
			return b, nil
		case steady && !errors.As(err, &tampered):
			// No plan, or no record this home can check: answers in
			// themselves (the caller recompiles, which is always
			// safe), not disagreements to wait out.
			return nil, err
		}
		last = err
	}
	return nil, last
}

// readPlanPair reads the record, the plan and the record again, and
// judges the plan against the record when both record reads agree
// (steady). An unsteady read returns ErrNoIntegrityRecord with steady
// false, so a caller that stops retrying recompiles rather than refuses.
//
// All three reads go through one handle on the directory (os.Root), so
// they read one directory even if a compile publishes a new one under the
// same name meanwhile (publishPlanDir); the record comparison still
// catches a writer that rewrites files in place, as a client built before
// agentry-cli#289 does.
func readPlanPair(key []byte, planDir string) (plan []byte, steady bool, err error) {
	root, err := os.OpenRoot(planDir)
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = root.Close() }()
	before, berr := root.ReadFile(IntegrityFile)
	if readHook != nil {
		readHook("record")
	}
	b, err := root.ReadFile("plan.json")
	if err != nil {
		if berr == nil && errors.Is(err, fs.ErrNotExist) {
			// A record without its plan: a recompile removed both
			// between the reads, or is about to write the plan.
			return nil, false, ErrNoIntegrityRecord
		}
		return nil, true, err
	}
	if readHook != nil {
		readHook("plan")
	}
	rec, rerr := root.ReadFile(IntegrityFile)
	if (berr == nil) != (rerr == nil) || !bytes.Equal(before, rec) {
		return nil, false, ErrNoIntegrityRecord
	}
	if rerr != nil {
		return nil, true, ErrNoIntegrityRecord
	}
	fields := strings.Fields(string(rec))
	if len(fields) != 3 || fields[0] != integrityRecordTag || fields[1] != integrityKeyID(key) {
		return nil, true, ErrNoIntegrityRecord
	}
	if !hmac.Equal([]byte(integrityMAC(key, b)), []byte(fields[2])) {
		return nil, true, &TamperedPlanError{PlanDir: planDir}
	}
	return b, true, nil
}
