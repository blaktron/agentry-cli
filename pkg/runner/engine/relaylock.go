package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RelayLockFile is the host-level lock a run whose plan needs relays holds
// from its relay listing to the end of its teardown. It lives directly
// under the runner home, which every client on the host shares (the CLI,
// the desktop, runhost's runner home).
const RelayLockFile = "relays.lock"

// errLockHeld is a lock file being held by another process (tryLockFile).
var errLockHeld = errors.New("lock held")

// holdRelayLock takes the runner home's relay lock without waiting, or
// refuses the run. refuseForeignRelays alone lists the relay network and
// the run then starts its gateway with nothing held across the two, so two
// runs starting together could both see an empty network and both start
// relays on it. And a gateway starts relays after its health probe too (the
// l4 relay for a server's host starts on that server's first connection),
// so a lock released once the gateway is up would still let a second run
// list the network empty while the first's late relays are yet to come. The
// lock is therefore held for the whole run, released by the run's teardown
// after its relays are reaped, and by the kernel if the process dies. A
// second relay run is refused, not queued: the message says to wait for the
// first, as the relay listing's refusal does.
//
// An empty home takes no lock (a client that has not adopted it); the
// relay listing is then the only check.
func holdRelayLock(home string) (release func(), err error) {
	if home == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, Classify(ExitGateway, fmt.Errorf("relay lock: %w", err))
	}
	path := filepath.Join(home, RelayLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed name under the runner home
	if err != nil {
		return nil, Classify(ExitGateway, fmt.Errorf("relay lock: %w", err))
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, Classify(ExitGateway, fmt.Errorf(
				"another run on this host whose declared servers reach their hosts through relays is starting or running (it holds %s). Relays share one network on the host (docker-mcp-proxies-int), so this run does not start beside it. Wait for that run to end", path))
		}
		return nil, Classify(ExitGateway, fmt.Errorf("relay lock %s: %w", path, err))
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
