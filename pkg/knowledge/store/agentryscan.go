package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrScanUnconfirmed is a kept pack whose scan in Agentry this client could
// not confirm: the error that wraps it says why.
var ErrScanUnconfirmed = errors.New("its scan in Agentry could not be confirmed")

// AgentryScan is the scan record of the kept pack name as the registry
// that keeps its copy reports it now (plan knowledge-and-drafter.md D14;
// agentry-cli#522). The record in a pack's manifest.json is only a claim
// (knowledge.Scan): this asks the registry. It answers the record only
// when the copy there is this pack's tree: the pack was uploaded to or
// downloaded from that registry (its own, or its organization's for a
// shared pack), is ready there with digest, the tree the caller opened and
// verified, and has not changed here since. A pack kept on this computer only has no record to
// confirm: nil, and no error. Otherwise it is an error wrapping
// ErrScanUnconfirmed, saying why, and the caller treats the pack as not
// scanned.
func (s *Store) AgentryScan(ctx context.Context, name, digest string) (json.RawMessage, error) {
	e, err := s.Find(name)
	if err != nil {
		return nil, err
	}
	a := e.Key.Agentry
	if a == nil {
		// Kept on this computer only: there is no scan to confirm.
		return nil, nil
	}
	if a.Digest != e.Manifest.SHA256 || digest != e.Manifest.SHA256 {
		return nil, fmt.Errorf("knowledge pack %s: %w: it changed here since it was uploaded", name, ErrScanUnconfirmed)
	}
	var list func(context.Context) ([]Remote, error)
	if e.Key.Org != "" {
		sh, err := s.shared(a.Registry, e.Key.Org)
		if err != nil {
			return nil, fmt.Errorf("knowledge pack %s: %w: %w", name, ErrScanUnconfirmed, err)
		}
		list = sh.List
	} else {
		reg, err := s.registry(a.Registry)
		if err != nil {
			return nil, fmt.Errorf("knowledge pack %s: %w: %w", name, ErrScanUnconfirmed, err)
		}
		list = reg.List
	}
	remotes, err := list(ctx)
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w: %w", name, ErrScanUnconfirmed, err)
	}
	for _, r := range remotes {
		if r.ID != a.ID {
			continue
		}
		switch {
		case r.State != StateReady:
			return nil, fmt.Errorf("knowledge pack %s: %w: it is %q in Agentry", name, ErrScanUnconfirmed, r.State)
		case r.Digest != digest:
			return nil, fmt.Errorf("knowledge pack %s: %w: Agentry's copy is another version of it", name, ErrScanUnconfirmed)
		case len(r.Scan) == 0:
			return nil, fmt.Errorf("knowledge pack %s: %w: Agentry holds no scan of it", name, ErrScanUnconfirmed)
		}
		return r.Scan, nil
	}
	return nil, fmt.Errorf("knowledge pack %s: %w: %w", name, ErrScanUnconfirmed, ErrRemoteNotFound)
}
