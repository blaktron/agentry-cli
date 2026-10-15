package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// makeGrantedPaths creates, in the run's own workspace copy, each path a
// paths grant names that the release does not ship, as an empty folder.
// The compiled catalog binds every such path into the tools server, and a
// bind needs its source: the run's docker filter refuses an absent one, so
// a release whose write grant names a folder for its output (out/) would
// not start its tools server. The install itself is never touched. Every
// step runs under an os.Root of the workspace, so a link the release ships
// cannot carry a folder outside it.
func makeGrantedPaths(workspace string, caps spec.CapSummary) error {
	var paths []string
	if caps.FSReadMode == "paths" {
		paths = append(paths, caps.FSReadPaths...)
	}
	if caps.FSWriteMode == "paths" {
		paths = append(paths, caps.FSWritePaths...)
	}
	if len(paths) == 0 {
		return nil
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("open the run's workspace: %w", err))
	}
	defer func() { _ = root.Close() }()
	for _, p := range paths {
		name := filepath.FromSlash(p)
		if _, err := root.Lstat(name); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Classify(ExitCLI, fmt.Errorf("the granted path %s: %w", p, err))
		}
		if err := root.MkdirAll(name, 0o755); err != nil {
			return Classify(ExitCLI, fmt.Errorf("create the granted path %s: %w", p, err))
		}
	}
	return nil
}
