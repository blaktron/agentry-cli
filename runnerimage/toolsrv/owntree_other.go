//go:build !linux

package toolsrv

// ownTree does nothing off Linux, where no command is held by ownership.
func (s *server) ownTree(string) {}
