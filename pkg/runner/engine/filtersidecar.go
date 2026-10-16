package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// Where the daemon runs in a VM (Docker Desktop, Colima, Lima, a podman
// machine), the gateway's docker filter cannot be a socket the CLI serves: a
// host socket cannot be bind-mounted into the VM's containers the way the
// daemon's own /var/run/docker.sock can. So it runs in a container of the
// run's own, the runner image's docker-filter role, which holds the daemon's
// socket and serves the same package's filter on docker.sock in a volume the
// gateway mounts at /var/run, where it looks for the daemon. The gateway
// reaches the daemon only through it, as where the CLI serves the filter.

const (
	// sidecarListenDir is where the filter container keeps its socket: the
	// run's filter volume, which the gateway mounts at gatewaySocketDir.
	sidecarListenDir = "/f"
	gatewaySocketDir = "/var/run"
	sidecarConfig    = "/etc/agentry/docker-filter.json"
	sidecarLogDir    = "/r"
)

// sidecarReadyWait bounds the wait for the filter container's socket; a
// variable so a test need not wait a minute.
var sidecarReadyWait = 60 * time.Second

// filterVolume is the volume the filter container serves its socket in.
func filterVolume(runID string) string { return "agentry-" + runID + "-docker" }

// startFilterSidecar starts the run's filter container and returns the
// volume holding its socket. daemonSocket is the daemon's socket as the
// container mounts it (filterPlacement.mountSource). cfg is the policy the
// in-process filter would take, its host paths spelled the gateway's way,
// which is how the gateway names them in what it asks and where the
// container sees them.
func startFilterSidecar(ctx context.Context, d dockerx.Client, rd *runDirs, runID, image, daemonSocket string, cfg sockfilter.Config, lim policy.Limits, td *teardown) (string, error) {
	cfg = sidecarConfigOf(cfg)
	raw, err := sockfilter.MarshalConfig(cfg)
	if err != nil {
		return "", Classify(ExitGateway, err)
	}
	cfgFile := filepath.Join(rd.cfg, "gateway", "docker-filter.json")
	if err := os.WriteFile(cfgFile, raw, 0o644); err != nil { //nolint:gosec // read by the filter container; no secret in it
		return "", Classify(ExitGateway, fmt.Errorf("docker filter configuration: %w", err))
	}
	vol := filterVolume(runID)
	if err := d.VolumeCreate(ctx, vol, runLabels(runID)); err != nil {
		return "", Classify(ExitGateway, fmt.Errorf("create the docker filter's volume: %w", err))
	}
	// Queued first, so it runs last: after the container that holds it.
	td.add(func(c context.Context) { _ = d.VolumeRemove(c, vol) })
	name := containerName(runID, "docker-filter")
	mounts := []dockerx.Mount{
		{Source: daemonSocket, Target: DockerSocket},
		{Source: vol, Target: sidecarListenDir},
		{Source: cfgFile, Target: sidecarConfig, ReadOnly: true},
		{Source: rd.receiptsDir, Target: sidecarLogDir},
	}
	for _, root := range sidecarRoots(cfg) {
		mounts = append(mounts, dockerx.Mount{Source: nativeOfGatewayPath(root, gatewayOS), Target: root, ReadOnly: true})
	}
	spec := dockerx.RunSpec{
		Name:  name,
		Image: image,
		Argv: []string{"docker-filter", "--config", sidecarConfig,
			"--listen", sidecarListenDir + "/docker.sock",
			"--log", sidecarLogDir + "/" + dockerFilterLog},
		Network: "none",
		Labels:  runLabels(runID),
		// Root, to open the daemon's socket; with every capability
		// dropped, a read-only root and no network.
		User:   "0",
		Mounts: mounts,
		// It holds the daemon's socket, as the gateway does where the CLI
		// serves the filter: an SELinux-enabled daemon would refuse it the
		// socket otherwise. The exemption goes with the role.
		DisableSELinuxLabel: true,
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	spec.ReadOnly = true
	td.add(func(c context.Context) {
		_ = d.Stop(c, name, 5)
		_ = d.RemoveForce(c, name)
	})
	if err := d.RunDetached(ctx, spec); err != nil {
		return "", Classify(ExitGateway, fmt.Errorf("start the gateway's docker filter container: %w", err))
	}
	deadline := time.Now().Add(sidecarReadyWait)
	for {
		logs, _ := d.Logs(ctx, name, 20)
		if strings.Contains(logs, sockfilter.SidecarReady) {
			rd.filterSidecar = filepath.Join(rd.receiptsDir, dockerFilterLog)
			return vol, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", Classify(ExitGateway, fmt.Errorf("the gateway's docker filter container did not start: %s", strings.TrimSpace(logs)))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sidecarConfigOf spells the host paths of a filter's configuration the
// gateway's way. The bind roots already are: they come from the gateway's
// own environment.
func sidecarConfigOf(cfg sockfilter.Config) sockfilter.Config {
	gw := func(p string) string {
		if p == "" {
			return ""
		}
		return gatewayPath(p)
	}
	cfg.WebSpec = gw(cfg.WebSpec)
	cfg.WebResolvConf = gw(cfg.WebResolvConf)
	cfg.RelayResolvConf = gw(cfg.RelayResolvConf)
	if len(cfg.ToolsShared) > 0 {
		shared := make(map[string]string, len(cfg.ToolsShared))
		for target, src := range cfg.ToolsShared {
			shared[target] = gw(src)
		}
		cfg.ToolsShared = shared
	}
	return cfg
}

// sidecarRoots are the host paths the filter container must see to judge
// the gateway's binds as the in-process filter does (it resolves each
// source's links and compares files): the bind and writable roots and the
// run's own files it binds itself, each once and none beneath another.
func sidecarRoots(cfg sockfilter.Config) []string {
	var all []string
	all = append(all, cfg.BindRoots...)
	all = append(all, cfg.WritableRoots...)
	for _, p := range []string{cfg.WebSpec, cfg.WebResolvConf, cfg.RelayResolvConf} {
		if p != "" {
			all = append(all, p)
		}
	}
	for _, p := range cfg.ToolsShared {
		all = append(all, p)
	}
	slices.Sort(all)
	all = slices.Compact(all)
	var out []string
	for _, p := range all {
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		covered := false
		for _, q := range out {
			if p == q || strings.HasPrefix(p, strings.TrimSuffix(q, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// nativeOfGatewayPath is gatewayHostPath's inverse, for a bind the CLI
// gives the filter container: on Windows /c/Users/… is C:\Users\…;
// elsewhere a path is spelled one way.
func nativeOfGatewayPath(p, goos string) string {
	if goos != "windows" || len(p) < 2 || p[0] != '/' || p[1] < 'a' || p[1] > 'z' || (len(p) > 2 && p[2] != '/') {
		return p
	}
	rest := strings.ReplaceAll(strings.TrimPrefix(p[2:], "/"), "/", `\`)
	return strings.ToUpper(p[1:2]) + `:\` + rest
}

// throughFilterVolume points the gateway's daemon-socket mount at the
// filter container's volume: mounted at /var/run, it holds docker.sock.
func throughFilterVolume(mounts []dockerx.Mount, vol string) []dockerx.Mount {
	out := make([]dockerx.Mount, len(mounts))
	for i, m := range mounts {
		if m.Source == DockerSocket {
			m = dockerx.Mount{Source: vol, Target: gatewaySocketDir}
		}
		out[i] = m
	}
	return out
}
