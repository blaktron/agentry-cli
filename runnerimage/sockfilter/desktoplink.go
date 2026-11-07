package sockfilter

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// The desktop link class. A desktop link entry carries the agent's calls out
// of the sandbox to Agentry Desktop, so it is held to exactly what its
// catalog entry asks and nothing more: the runner image running one of the
// run's desktop link commands (Config.DesktopCmds) as the run's user, on no
// network, with a read-only root and the run's relay volume
// (Config.DesktopVolume) as its one mount; and that volume is admitted to
// these containers alone, so nothing else the gateway launches reaches the
// relay's socket.

// isDesktopLink says whether a create is one of the run's desktop link
// entries: one of Config.DesktopCmds in Config.DesktopImage (by id), run by
// the image's own entrypoint.
func (f *Filter) isDesktopLink(ctx context.Context, c *containerCreateBody, imageID string) (bool, error) {
	if f.cfg.DesktopVolume == "" || f.cfg.DesktopImage == "" || len(f.cfg.DesktopCmds) == 0 {
		return false, nil
	}
	if present(c.Entrypoint) || present(c.Shell) {
		return false, nil
	}
	var cmd []string
	if !present(c.Cmd) || json.Unmarshal(c.Cmd, &cmd) != nil {
		return false, nil
	}
	if !slices.ContainsFunc(f.cfg.DesktopCmds, func(want []string) bool { return slices.Equal(cmd, want) }) {
		return false, nil
	}
	id, err := f.resolveImage(ctx, f.cfg.DesktopImage)
	if err != nil {
		return false, err
	}
	return imageID == id, nil
}

// desktopShape holds a desktop link entry to its class: no network, the
// run's user, the relay volume as its one mount and no other bind, mount or
// tmpfs, then a read-only root. Applied after the storage check, as the
// run's own.
func (f *Filter) desktopShape(c *containerCreateBody) error {
	hc := c.HostConfig
	if hc.NetworkMode != "none" || (c.NetworkingConfig != nil && len(c.NetworkingConfig.EndpointsConfig) > 0) {
		return deny("the desktop link joins no network")
	}
	if f.cfg.DesktopUser != "" && c.User != f.cfg.DesktopUser {
		return deny("the desktop link runs as %q, not the run's %q", c.User, f.cfg.DesktopUser)
	}
	mounts := 0
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 || parts[0] != f.cfg.DesktopVolume {
			return deny("the desktop link mounts its relay volume %s and nothing else, not %q", f.cfg.DesktopVolume, b)
		}
		mounts++
	}
	for _, m := range hc.Mounts {
		if m.Type != "volume" || m.Source != f.cfg.DesktopVolume {
			return deny("the desktop link mounts its relay volume %s and nothing else, not %s %q", f.cfg.DesktopVolume, m.Type, m.Source)
		}
		mounts++
	}
	if mounts != 1 {
		return deny("the desktop link mounts its relay volume %s once, not %d times", f.cfg.DesktopVolume, mounts)
	}
	if len(hc.Tmpfs) > 0 {
		return deny("the desktop link mounts no tmpfs")
	}
	hc.ReadonlyRootfs = true
	if !slices.ContainsFunc(hc.SecurityOpt, strengthens) {
		hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges")
	}
	return nil
}
