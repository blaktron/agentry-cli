package mcpinstall

// One writer per server in a store: whoever installs a server outside a
// run's own start holds the server's lock while it writes the server's
// directory and its record, and a start that finds the lock held leaves the
// server out of what it runs.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/internal/storefile"
)

// lockPoll is how often LockServer tries a lock another holder has.
const lockPoll = 200 * time.Millisecond

// lockPath is a server's lock file, beside its directory in the store and
// never inside it, where the store's digest and the server's mount are.
func lockPath(storeDir, id string) string { return filepath.Join(storeDir, "."+id+".lock") }

// LockServer takes the lock of the server id in the store, waiting while
// another holder has it, until ctx ends. unlock lets it go.
func LockServer(ctx context.Context, storeDir, id string) (unlock func(), err error) {
	for {
		unlock, held, err := storefile.TryLock(lockPath(storeDir, id), "the MCP server "+id)
		if err != nil {
			return nil, err
		}
		if !held {
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// Installing says another holder has the lock of the server id in the
// store: an install is writing it. It tries the lock and lets it go, which
// leaves the server's lock file in the store. A store that does not exist
// has no install under way.
func Installing(storeDir, id string) bool {
	if _, err := os.Stat(storeDir); errors.Is(err, os.ErrNotExist) {
		return false
	}
	unlock, held, err := storefile.TryLock(lockPath(storeDir, id), "the MCP server "+id)
	if err != nil || held {
		return held
	}
	unlock()
	return false
}

// Installed says the store holds the pin srv names: a record of its
// install for that package and version. A remote server is never installed
// and needs nothing, so it is always held.
func Installed(storeDir string, srv manifest.MCPServer) bool {
	if srv.IsRemote() {
		return true
	}
	meta, err := LoadMeta(storeDir, IDFor(srv))
	return err == nil && meta != nil && meta.Package == PackageOf(srv) && meta.Version == srv.Version
}
