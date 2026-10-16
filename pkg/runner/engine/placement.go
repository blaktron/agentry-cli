package engine

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// Where the run's docker filter runs is decided by where the daemon runs,
// not by the operating system. A daemon on this machine's own kernel can
// bind-mount a socket the CLI serves, so the CLI serves the filter
// (in-process). A daemon in a VM (Docker Desktop, Colima, Lima, a podman
// machine) cannot see a host socket, so a container of the run's own holds
// the daemon's socket and serves the filter beside the gateway (sidecar,
// filtersidecar.go). The compiled plan keeps naming DockerSocket either way;
// the placement maps it to the engine's real socket at run time, so a plan
// and its hash do not depend on the engine.

// EnvDockerFilter overrides the placement rule on a machine it misjudges:
// "process" or "sidecar".
const EnvDockerFilter = "AGENTRY_DOCKER_FILTER"

// filterPlacement is where a run's docker filter runs and what it reaches
// the daemon through.
type filterPlacement struct {
	engine dockerx.Engine
	// inProcess says the CLI serves the filter; otherwise a container of
	// the run's does.
	inProcess bool
	// reason says why, for the receipt and doctor.
	reason string
	// upstream is the daemon's socket the in-process filter dials: the
	// engine's own path.
	upstream string
	// mountSource is the daemon's socket as the filter container mounts it:
	// the engine's path for a daemon on this machine, and the literal
	// DockerSocket for one in a VM, where that is the daemon's own path and a
	// host path does not cross into it.
	mountSource string
}

// hostFacts is what the rule compares the daemon's answers with.
type hostFacts struct {
	goos     string
	hostname string
	kernel   string
	// inContainer says this CLI runs in a container: its hostname is the
	// container's, not the machine's, while its kernel is the machine's.
	inContainer bool
}

// thisHost reads this machine's facts; a variable so a test can be any host.
var thisHost = func() hostFacts {
	h, _ := os.Hostname()
	return hostFacts{goos: runtime.GOOS, hostname: h, kernel: dockerx.HostKernel(), inContainer: inContainer()}
}

// inContainer says whether this process runs in a container, by the marker
// files docker and podman put in every container's root.
func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// placeFilter decides the placement for engine e on host h, with override
// the value of AGENTRY_DOCKER_FILTER. An endpoint the filter cannot be put in
// front of (tcp://, ssh://, a named pipe outside Windows) is refused: the
// gateway would otherwise hold the daemon itself.
func placeFilter(e dockerx.Engine, h hostFacts, override string) (filterPlacement, error) {
	scheme, addr, err := dockerx.ParseEndpoint(e.Endpoint)
	if err != nil {
		return filterPlacement{}, Classify(ExitGateway, err)
	}
	switch {
	case scheme == dockerx.SchemeUnix:
	case scheme == dockerx.SchemeNpipe && h.goos == "windows":
		// Docker Desktop's and Rancher Desktop's pipe on Windows: the
		// daemon is in a VM, whose own socket the filter container mounts.
	default:
		return filterPlacement{}, Classify(ExitGateway, fmt.Errorf(
			"the container engine's endpoint %s is not a Unix socket on this machine, so the run's docker filter cannot be put in front of it: "+
				"point the docker CLI at an engine on this machine (unset DOCKER_HOST, or `docker context use` a local context)", e.Endpoint))
	}
	p := filterPlacement{engine: e, upstream: addr, mountSource: DockerSocket}
	sameKernel := h.kernel != "" && e.DaemonKernel == h.kernel
	local := h.goos == "linux" && scheme == dockerx.SchemeUnix && sameKernel && (e.DaemonHost == h.hostname || h.inContainer)
	if local {
		p.mountSource = addr
	}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "":
	case "process":
		if scheme != dockerx.SchemeUnix {
			return filterPlacement{}, Classify(ExitGateway, fmt.Errorf("%s=process needs an engine on a Unix socket, and this one is at %s", EnvDockerFilter, e.Endpoint))
		}
		p.inProcess, p.reason = true, EnvDockerFilter+"=process"
		return p, nil
	case "sidecar":
		p.reason = EnvDockerFilter + "=sidecar"
		return p, nil
	default:
		return filterPlacement{}, Classify(ExitGateway, fmt.Errorf("%s is %q: it takes process or sidecar", EnvDockerFilter, override))
	}
	switch {
	case local && e.DaemonHost == h.hostname:
		p.inProcess = true
		p.reason = fmt.Sprintf("the daemon reports this machine's hostname (%s) and kernel (%s)", h.hostname, h.kernel)
	case local:
		p.inProcess = true
		p.reason = fmt.Sprintf("the daemon reports this machine's kernel (%s) and the CLI runs in a container on it", h.kernel)
	case h.goos != "linux":
		p.reason = fmt.Sprintf("the daemon runs in a VM (%s, kernel %s) on %s", orUnknown(e.DaemonHost), orUnknown(e.DaemonKernel), h.goos)
	default:
		p.reason = fmt.Sprintf("the daemon reports %s and kernel %s, not this machine's %s and %s, so it runs in a VM",
			orUnknown(e.DaemonHost), orUnknown(e.DaemonKernel), orUnknown(h.hostname), orUnknown(h.kernel))
	}
	return p, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// placementFor reads d's engine and places the run's filter.
func placementFor(ctx context.Context, d dockerx.Client) (filterPlacement, error) {
	e, err := dockerx.DescribeEngine(ctx, d)
	if err != nil {
		return filterPlacement{}, Classify(ExitGateway, err)
	}
	return placeFilter(e, thisHost(), os.Getenv(EnvDockerFilter))
}

// line is the engine and the placement in one line: the receipt's engine
// line and doctor's.
func (p filterPlacement) line() string {
	where := "sidecar"
	if p.inProcess {
		where = "in-process"
	}
	ep := p.engine.SocketPath()
	if ep == "" {
		ep = p.engine.Endpoint
	}
	return p.engine.Summary() + ", " + ep + ", docker filter " + where + ": " + p.reason
}

// EngineLine is d's engine and where a run's docker filter would run on it,
// in the receipt's words, or why a run would be refused.
func EngineLine(ctx context.Context, d dockerx.Client) (string, error) {
	p, err := placementFor(ctx, d)
	if err != nil {
		return "", err
	}
	return p.line(), nil
}
