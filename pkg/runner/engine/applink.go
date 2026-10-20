package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// prepareAppLink holds a segment to its plan's app link: the supervisor's
// socket, which the app link's container binds, is given exactly when the
// plan has one, and must be a socket, not a link or a file.
func prepareAppLink(p Params, rd *runDirs) error {
	want := p.Plan.Capabilities.AppLink != ""
	got := p.Persistent != nil && p.Persistent.AppLink != ""
	if want != got {
		return Classify(ExitCompile, errors.New("the segment's app link socket and its plan disagree: recompile"))
	}
	if !want {
		return nil
	}
	sock := p.Persistent.AppLink
	if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock || strings.Contains(strings.TrimPrefix(sock, filepath.VolumeName(sock)), ":") {
		return Classify(ExitCLI, fmt.Errorf("the app link's socket %q is not a clean absolute path", sock))
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("the app link's socket: %w", err))
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return Classify(ExitCLI, fmt.Errorf("the app link's socket %s is not a socket", sock))
	}
	rd.appLink = sock
	return nil
}

// HostSocketsReachContainers says whether a socket on this machine can be
// bound into the run's containers: true where the engine's daemon runs
// here (the docker filter is served in-process), false where it runs in a
// VM (Docker Desktop, Colima, a podman machine), whose containers a host
// socket does not reach; why says which.
func HostSocketsReachContainers(ctx context.Context, d dockerx.Client) (ok bool, why string, err error) {
	p, err := placementFor(ctx, d)
	if err != nil {
		return false, "", err
	}
	return p.inProcess, p.reason, nil
}
