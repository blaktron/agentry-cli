package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// writeTurn replaces the run's turn count in dir with n in one rename, so the
// tools server never reads half of it.
func writeTurn(dir string, n int) error {
	tmp, err := os.CreateTemp(dir, ".turn-*")
	if err != nil {
		return fmt.Errorf("the turn count: %w", err)
	}
	if _, err := tmp.WriteString(strconv.Itoa(n) + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the turn count: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the turn count: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the turn count: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, spec.TurnsFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("the turn count: %w", err)
	}
	return nil
}

// nextTurn moves the run's turn count on, at a turn's start or a
// compaction: the tools server's reads before it are no longer in the
// history as the agent sees it, or may not be. A count that cannot be
// written is removed, so the tools server, which cannot read it, serves
// every read whole rather than stand a stub for an earlier turn's.
func (s *liveSession) nextTurn() {
	s.mu.Lock()
	rd := s.rd
	s.turnCount++
	n := s.turnCount
	s.mu.Unlock()
	if rd == nil || rd.turns == "" {
		return
	}
	if err := writeTurn(rd.turns, n); err != nil {
		_ = os.Remove(filepath.Join(rd.turns, spec.TurnsFile))
	}
}
