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

// relayHostSetting is an L4 relay's target host, as relayShape reads it.
const relayHostSetting = "PROXY_HOSTNAME"

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
	// The run's user; where the run gives none, the image's own, which the
	// gateway may also leave unnamed.
	if ok := c.User == f.cfg.BrowserUser || (f.cfg.BrowserUser == "" && c.User == defaultBrowserUser); !ok {
		return deny("the browser server runs as %q, not the run's %q", c.User, user)
	}
	uid, gid, ok := strings.Cut(user, ":")
	if !ok || uid == "" || gid == "" || strings.Trim(uid+gid, "0123456789") != "" {
		return deny("the browser server's user %q is not uid:gid", user)
	}
	var declared map[string]json.RawMessage
	if present(c.Volumes) {
		if err := json.Unmarshal(c.Volumes, &declared); err != nil {
			return deny("the browser server's Volumes are a map of targets")
		}
	}
	targets := slices.Sorted(maps.Keys(f.cfg.BrowserTmpfs))
	for _, t := range targets {
		if beneath(hc, slices.Collect(maps.Keys(declared)), t) {
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
	// No new privileges, whatever the gateway asked: the class's seccomp
	// profile lets it make namespaces, and nothing in them may gain more.
	if !slices.ContainsFunc(hc.SecurityOpt, strengthens) {
		hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges")
	}
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
// target: a declared volume, a bind, a mount or a tmpfs of the gateway's.
func beneath(hc *hostConfig, volumes []string, target string) bool {
	under := func(p string) bool {
		p = path.Clean(p)
		return p == target || strings.HasPrefix(p, target+"/")
	}
	for _, v := range volumes {
		if under(v) {
			return true
		}
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
		if k != relayHostSetting {
			continue
		}
		if ip, ok := f.cfg.RelayPins[v]; ok {
			c.HostConfig.ExtraHosts = append(c.HostConfig.ExtraHosts, v+":"+ip)
		}
	}
}

// browserScope is which side of the browser's network a create is.
type browserScope int

const (
	notBrowser browserScope = iota
	// browserServer is the browser server itself.
	browserServer
	// browserRelay is the one relay to the browse proxy.
	browserRelay
)

// browserScope says whether a create is the browser server or the relay to
// the browse proxy: an L4 relay of a pinned image whose PROXY_HOSTNAME is
// the proxy's name.
func (f *Filter) browserScope(ctx context.Context, c *containerCreateBody, imageID string) (browserScope, error) {
	if f.cfg.BrowserNetwork == "" {
		return notBrowser, nil
	}
	if browser, err := f.isBrowser(ctx, imageID); err != nil || browser {
		if browser {
			return browserServer, nil
		}
		return notBrowser, err
	}
	pin, err := f.relayPin(ctx, imageID)
	if err != nil || pin == "" {
		return notBrowser, err
	}
	for _, kv := range c.Env {
		if k, v, _ := strings.Cut(kv, "="); k == relayHostSetting && v == f.cfg.BrowseProxy {
			return browserRelay, nil
		}
	}
	return notBrowser, nil
}

// browserNetworkAllowed holds the browser's network to its two: the browser
// server joins it alone, its relay it and the way out, and no other
// container joins it.
func (f *Filter) browserNetworkAllowed(scope browserScope, network string) error {
	if f.cfg.BrowserNetwork == "" {
		return nil
	}
	switch {
	case network == f.cfg.BrowserNetwork && scope == notBrowser:
		return deny("network %s is the browser server's and its relay's alone", network)
	case scope == browserServer && network != f.cfg.BrowserNetwork:
		return deny("the browser server joins its own network alone, not %s", network)
	}
	return nil
}
