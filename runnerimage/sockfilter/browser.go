package sockfilter

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// The browser class (agentry-notes plans/browser-testing.md §4, §7, M1;
// agentry-cli#915). The run's browser server renders pages nobody vetted
// and, for a release that declares browser secrets, holds them, so it is
// held tighter than a declared server: no capability back, a read-only
// root with a tmpfs home and /tmp, the user the run gave it, and Chromium's
// own sandbox on. Under runc that sandbox needs user, PID and network
// namespaces, which docker's default seccomp profile refuses a process
// without CAP_SYS_ADMIN; the filter gives this one class moby's default
// profile plus clone, unshare, setns and chroot (measured by M1), still
// with every capability dropped and no new privileges. Under a sandboxed
// runtime (gVisor) Docker's default profile lets it run, and none is set.

// chromiumSeccomp is that profile: moby's default (as Docker 29.7.2 ships
// it) and one more rule, last, allowing the four calls.
//
//go:embed chromium-seccomp.json
var chromiumSeccomp []byte

// BrowserSeccomp is the profile, compacted, as a create's SecurityOpt
// carries it ("seccomp=<json>").
func BrowserSeccomp() (string, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, chromiumSeccomp); err != nil {
		return "", fmt.Errorf("sockfilter: the browser's seccomp profile: %w", err)
	}
	return b.String(), nil
}

// defaultBrowserUser is the agentry-browser image's own user.
const defaultBrowserUser = "10001:10001"

// browserImage is the browser server's image id (Config.BrowserImage),
// resolved and kept as toolsImage is.
func (f *Filter) browserImage(ctx context.Context) (string, error) {
	f.webMu.Lock()
	defer f.webMu.Unlock()
	if f.browserImageID != "" {
		return f.browserImageID, nil
	}
	id, err := f.resolveImage(ctx, f.cfg.BrowserImage)
	if err != nil {
		return "", err
	}
	f.browserImageID = id
	return id, nil
}

// isBrowser says whether a create, of an image by id, is the run's browser
// server.
func (f *Filter) isBrowser(ctx context.Context, imageID string) (bool, error) {
	if f.cfg.BrowserImage == "" {
		return false, nil
	}
	id, err := f.browserImage(ctx)
	if err != nil {
		return false, err
	}
	return imageID == id, nil
}

// browserShape holds the browser server to its class: its user the run's,
// nothing of the gateway's at or beneath the tmpfs targets, then the
// read-only root, the tmpfs mounts and, without a runtime, the seccomp
// profile. Applied after the storage check, as the run's own.
func (f *Filter) browserShape(c *containerCreateBody) error {
	hc := c.HostConfig
	user := f.cfg.BrowserUser
	if user == "" {
		user = defaultBrowserUser
	}
	if c.User != f.cfg.BrowserUser && !(f.cfg.BrowserUser == "" && c.User == defaultBrowserUser) {
		return deny("the browser server runs as %q, not the run's %q", c.User, f.cfg.BrowserUser)
	}
	uid, gid, ok := strings.Cut(user, ":")
	if !ok || uid == "" || gid == "" || strings.Trim(uid+gid, "0123456789") != "" {
		return deny("the browser server's user %q is not uid:gid", user)
	}
	targets := slices.Sorted(maps.Keys(f.cfg.BrowserTmpfs))
	for _, t := range targets {
		if beneath(hc, t) {
			return deny("the browser server's request names something at or beneath %s", t)
		}
	}
	if hc.Tmpfs == nil {
		hc.Tmpfs = map[string]string{}
	}
	for _, t := range targets {
		hc.Tmpfs[t] = f.cfg.BrowserTmpfs[t] + ",uid=" + uid + ",gid=" + gid
	}
	hc.ReadonlyRootfs = true
	if f.cfg.Runtime == "" {
		profile, err := BrowserSeccomp()
		if err != nil {
			return err
		}
		hc.SecurityOpt = append(hc.SecurityOpt, "seccomp="+profile)
	}
	return nil
}

// beneath says whether a container's request mounts anything at or beneath
// target: a bind, a mount or a tmpfs of the gateway's.
func beneath(hc *hostConfig, target string) bool {
	under := func(p string) bool {
		p = path.Clean(p)
		return p == target || strings.HasPrefix(p, target+"/")
	}
	for _, b := range hc.Binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 && under(parts[1]) {
			return true
		}
	}
	for _, m := range hc.Mounts {
		if under(m.Target) {
			return true
		}
	}
	for t := range hc.Tmpfs {
		if under(t) {
			return true
		}
	}
	return false
}

// relayPinHosts writes RelayPins' address for an L4 relay's host into its
// /etc/hosts, the run's own entry added after the shape check.
func (f *Filter) relayPinHosts(c *containerCreateBody) {
	for _, kv := range c.Env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "PROXY_HOSTNAME" {
			continue
		}
		if ip, ok := f.cfg.RelayPins[v]; ok {
			c.HostConfig.ExtraHosts = append(c.HostConfig.ExtraHosts, v+":"+ip)
		}
	}
}
