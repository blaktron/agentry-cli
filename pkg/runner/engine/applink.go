package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// prepareAppLink holds a segment to its plan's app link: the supervisor's
// socket is given exactly when the plan has one, and must be a socket, not
// a link or a file. Where the engine's daemon runs on this computer the app
// link's container binds it; where it runs in a VM, whose containers a
// socket here does not reach, the app link mounts the run's relay volume
// instead, and the relay carries each call to it (startAppLinkRelay).
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
	if p.place != nil && !p.place.inProcess {
		rd.appLinkRelay = appLinkVolume(filepath.Base(rd.root))
		rd.appLinkPlacement = p.place.reason
	}
	return nil
}

// appLinkRoot is spec.PHAppLink's value: the supervisor's socket, a bind
// root of its own, where the app link's container binds it; nothing where
// the relay carries the calls and the app link mounts a volume instead.
func (rd *runDirs) appLinkRoot(hostPath func(string) string) string {
	if rd.appLink == "" || rd.appLinkRelay != "" {
		return ""
	}
	return hostPath(rd.appLink)
}

// appLinkMount is spec.PHAppLinkMount's value: the app link container's one
// mount, the supervisor's socket at the socket's path, or the relay's volume
// at its folder.
func (rd *runDirs) appLinkMount(hostPath func(string) string) string {
	switch {
	case rd.appLink == "":
		return ""
	case rd.appLinkRelay != "":
		return rd.appLinkRelay + ":" + applink.SocketDir + ":rw"
	}
	return hostPath(rd.appLink) + ":" + applink.SocketMount + ":rw"
}
