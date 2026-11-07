package sockfilter

import (
	"net/http"
	"slices"
	"testing"
)

// The desktop link's relay volume is admitted to the desktop link entries
// alone: the runner image running one of the run's desktop link commands,
// as the run's user, on no network, with that volume as its one mount. The
// tools server, another image, another command, another entrypoint and a
// container joining a network are refused it, and an entry asking for
// anything beside the volume is refused.
func TestTheDesktopLinkAloneMountsItsRelayVolume(t *testing.T) {
	const target = "/etc/agentry/toolspec.json"
	const vol = "agentry-R-desktop"
	blender := []string{"desktoplink", "--server", "blender"}
	unreal := []string{"desktoplink", "--server", "unreal"}
	g := newRig(t, Limits{}, func(c *Config) {
		c.ToolsImage, c.ToolsCmd = "agentry-runner:test", []string{"tools", "--spec", target}
		c.DesktopVolume, c.DesktopImage, c.DesktopCmds, c.DesktopUser = vol, "agentry-runner:test", [][]string{blender, unreal}, "1000:1000"
	})
	create := func(image string, cmd []string, user string, binds ...string) map[string]any {
		return map[string]any{"Image": image, "Cmd": cmd, "User": user, "HostConfig": map[string]any{"Binds": binds, "NetworkMode": "none"}}
	}
	for _, cmd := range [][]string{blender, unreal} {
		if code, body := g.do(t, "POST", "/containers/create", create("agentry-runner:test", cmd, "1000:1000", vol+":/run/agentry-desktop:rw")); code != http.StatusCreated {
			t.Fatalf("the desktop link %v with its relay volume: %d %s", cmd, code, body)
		}
		hc := createdHostConfig(t, g)
		if hc["ReadonlyRootfs"] != true {
			t.Errorf("%v: ReadonlyRootfs %v", cmd, hc["ReadonlyRootfs"])
		}
		opts, _ := hc["SecurityOpt"].([]any)
		if !slices.Contains(opts, any("no-new-privileges")) {
			t.Errorf("%v: SecurityOpt %v", cmd, opts)
		}
	}
	g.refused(t, "the runner image with another server", "POST", "/containers/create",
		create("agentry-runner:test", []string{"desktoplink", "--server", "custom"}, "1000:1000", vol+":/run/agentry-desktop:rw"), "is not this run's")
	g.refused(t, "the tools server with the relay volume", "POST", "/containers/create",
		create("agentry-runner:test", []string{"tools", "--spec", target}, "1000:1000", vol+":/run/agentry-desktop:rw"), "is not this run's")
	g.refused(t, "another image running the link's command", "POST", "/containers/create",
		create("node:22", blender, "1000:1000", vol+":/run/agentry-desktop:rw"), "is not this run's")
	entry := create("agentry-runner:test", blender, "1000:1000", vol+":/run/agentry-desktop:rw")
	entry["Entrypoint"] = []string{"/bin/sh", "-c"}
	g.refused(t, "the link's command under another entrypoint", "POST", "/containers/create", entry, "is not this run's")

	// The class: the volume once, as the run's user, on no network, with no
	// other bind, mount or tmpfs.
	g.refused(t, "the link as another user", "POST", "/containers/create",
		create("agentry-runner:test", blender, "0:0", vol+":/run/agentry-desktop:rw"), "runs as")
	g.refused(t, "the link with another volume too", "POST", "/containers/create",
		create("agentry-runner:test", blender, "1000:1000", vol+":/run/agentry-desktop:rw", "agentry-persistent-assistant-data:/data"), "is not this run's")
	g.refused(t, "the link without its volume", "POST", "/containers/create",
		create("agentry-runner:test", blender, "1000:1000"), "once, not 0 times")
	// A network the run owns is refused by the class; the daemon's default
	// is refused before the class sees it.
	networked := create("agentry-runner:test", blender, "1000:1000", vol+":/run/agentry-desktop:rw")
	networked["HostConfig"].(map[string]any)["NetworkMode"] = "runa-net"
	g.refused(t, "the link on a network", "POST", "/containers/create", networked, "joins no network")
	withTmpfs := create("agentry-runner:test", blender, "1000:1000", vol+":/run/agentry-desktop:rw")
	withTmpfs["HostConfig"].(map[string]any)["Tmpfs"] = map[string]any{"/tmp": ""}
	g.refused(t, "the link with a tmpfs", "POST", "/containers/create", withTmpfs, "no tmpfs")

	// With no desktop link, the volume is nobody's.
	g2 := newRig(t, Limits{}, func(c *Config) {
		c.ToolsImage, c.ToolsCmd = "agentry-runner:test", []string{"tools", "--spec", target}
	})
	g2.refused(t, "the link's command in a run without the link", "POST", "/containers/create",
		create("agentry-runner:test", blender, "1000:1000", vol+":/run/agentry-desktop:rw"), "is not this run's")
}
