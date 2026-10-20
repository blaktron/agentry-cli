package persistent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The clients that own a running agent.
const (
	ClientCLI     = "cli"
	ClientDesktop = "desktop"
)

// Holder is who owns a running agent: written beside the lock once it is
// held, and read by a client that is refused it.
type Holder struct {
	Client string    `json:"client"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
}

// HeldError is the refusal of a second owner.
type HeldError struct {
	Agent  string
	Holder *Holder // nil when the holder's record could not be read
}

func (e *HeldError) Error() string {
	if e.Holder == nil {
		return fmt.Sprintf("the %s agent is already running under another process", e.Agent)
	}
	where := "agentry agent start"
	if e.Holder.Client == ClientDesktop {
		where = "Agentry Desktop"
	}
	return fmt.Sprintf("the %s agent is already running under %s (pid %d, since %s)", e.Agent, where, e.Holder.PID, e.Holder.Since.Local().Format("2006-01-02 15:04"))
}

// errLockHeld is a lock another open file holds.
var errLockHeld = errors.New("lock held")

// Owner is a held owner lock. The kernel releases it when the process ends
// however it ends, so an agent whose supervisor died can be started again.
type Owner struct {
	f     *os.File
	home  string
	agent string
}

// Acquire takes the agent's owner lock for client, or refuses with a
// HeldError naming the holder.
func Acquire(home, name, client string) (*Owner, error) {
	dir := Dir(home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("the %s agent's folder: %w", name, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the runner home's own file
	if err != nil {
		return nil, fmt.Errorf("the %s agent's lock: %w", name, err)
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			h, _ := ReadHolder(home, name)
			return nil, &HeldError{Agent: name, Holder: h}
		}
		return nil, fmt.Errorf("the %s agent's lock: %w", name, err)
	}
	b, _ := json.Marshal(Holder{Client: client, PID: os.Getpid(), Since: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(dir, holderFile), b, 0o600); err != nil {
		_ = unlockFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("the %s agent's lock: %w", name, err)
	}
	return &Owner{f: f, home: home, agent: name}, nil
}

// Release lets the lock go and removes the holder's record.
func (o *Owner) Release() {
	if o == nil || o.f == nil {
		return
	}
	_ = os.Remove(filepath.Join(Dir(o.home, o.agent), holderFile))
	_ = unlockFile(o.f)
	_ = o.f.Close()
	o.f = nil
}

// ReadHolder is the record of the agent's current owner, nil when there is
// none. A record left by an owner that died is stale; Held says whether the
// lock is actually held.
func ReadHolder(home, name string) (*Holder, error) {
	b, err := os.ReadFile(filepath.Join(Dir(home, name), holderFile)) //nolint:gosec // the runner home's own file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var h Holder
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Held reports whether some process holds the agent's owner lock, and who.
func Held(home, name string) (bool, *Holder) {
	f, err := os.OpenFile(filepath.Join(Dir(home, name), lockFile), os.O_RDWR, 0o600) //nolint:gosec // the runner home's own file
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	if err := tryLockFile(f); err != nil {
		h, _ := ReadHolder(home, name)
		return errors.Is(err, errLockHeld), h
	}
	_ = unlockFile(f)
	return false, nil
}
