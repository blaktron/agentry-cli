package dockerx

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Unreachable is the error a client returns when its CLI is installed and
// no engine answers it. Its text is one sentence for a person: which engine
// this machine seems to have, read from where the client's requests go, and
// how to start it. The client's own error, with what the command printed,
// is kept beneath it for a verbose reading.
type Unreachable struct {
	// CLI is the client asked: "docker" or "podman".
	CLI string
	// Endpoint is where the client's requests went, as it resolves them
	// (unix:///path, npipe://…, tcp://…), "" when it could not say.
	Endpoint string
	// Engine names what the endpoint belongs to, when the machine's layout
	// says: Docker Desktop, Docker Engine, Colima, OrbStack, Rancher
	// Desktop, Lima, podman, podman machine. "" when it does not.
	Engine string
	// Remedy is how to start it, one clause.
	Remedy string
	// Denied says the engine answers but this user may not use its socket.
	Denied bool
	// Err is the client's own error.
	Err error
}

func (u *Unreachable) Error() string { return u.Problem() + ": " + u.Remedy }

// Problem is the sentence's first half: what is wrong, without the remedy.
func (u *Unreachable) Problem() string {
	switch {
	case u.Denied && u.Engine != "":
		return "this user may not use " + u.Engine + "'s socket" + u.at()
	case u.Denied:
		return "this user may not use the container engine's socket" + u.at()
	case u.Engine != "":
		return u.Engine + " is not running" + u.at()
	case u.Endpoint != "":
		return "no container engine answers at " + u.Endpoint
	}
	return "no container engine answers the " + u.CLI + " CLI"
}

// at is the endpoint's address in parentheses, for a socket or a pipe.
func (u *Unreachable) at() string {
	scheme, addr, err := ParseEndpoint(u.Endpoint)
	if err != nil || (scheme != SchemeUnix && scheme != SchemeNpipe) {
		return ""
	}
	return " (" + addr + ")"
}

func (u *Unreachable) Unwrap() error { return u.Err }

// Detail is the client's own error: what the command said.
func (u *Unreachable) Detail() string {
	if u.Err == nil {
		return ""
	}
	return u.Err.Error()
}

// unreachable builds the error for a client whose engine did not answer:
// the engine is named from the endpoint's path and the remedy chosen for
// it and for this operating system.
func unreachable(cli, endpoint string, err error) *Unreachable {
	return unreachableOn(runtime.GOOS, cli, endpoint, err)
}

func unreachableOn(goos, cli, endpoint string, err error) *Unreachable {
	u := &Unreachable{CLI: cli, Endpoint: strings.TrimSpace(endpoint), Err: err}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		u.Denied = true
	}
	u.Engine, u.Remedy = engineOfEndpoint(goos, cli, u.Endpoint)
	if u.Denied {
		switch {
		case strings.HasPrefix(u.Engine, "podman"):
			u.Remedy = "check the socket's owner and mode, or use the podman CLI as that user"
		default:
			u.Remedy = "add yourself to the docker group with `sudo usermod -aG docker $USER`, then sign in again"
		}
	}
	return u
}

// engineOfEndpoint names the engine an endpoint belongs to on goos, from
// the path each one serves its socket at, with how to start it.
func engineOfEndpoint(goos, cli, endpoint string) (engine, remedy string) {
	scheme, addr, err := ParseEndpoint(endpoint)
	switch {
	case endpoint == "" || err != nil:
		if cli == KindPodman {
			if goos == "darwin" || goos == "windows" {
				return "podman machine", "start it with `podman machine start`"
			}
			return "podman", "run `podman info` to see why it does not answer"
		}
		if goos == "linux" {
			return "", "start Docker Engine with `sudo systemctl start docker`, or start Docker Desktop or podman"
		}
		return "", "start Docker Desktop, or the engine you use in its place"
	case scheme == SchemeNpipe:
		return "Docker Desktop", "start Docker Desktop, then run this again"
	case scheme != SchemeUnix:
		return "", "check that the host it names is up and reachable, or point DOCKER_HOST at an engine that runs"
	}
	slash := filepath.ToSlash(addr)
	parts := strings.Split(slash, "/")
	instance := func(dir string) string {
		for i, p := range parts {
			if p == dir && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		return ""
	}
	switch {
	case strings.Contains(slash, "/.docker/desktop/") || strings.Contains(slash, "/.docker/run/"):
		return "Docker Desktop", "start Docker Desktop, then run this again"
	case strings.Contains(slash, "/.colima/"):
		if p := instance(".colima"); p != "" && p != "default" {
			return "Colima", "start it with `colima start --profile " + p + "`"
		}
		return "Colima", "start it with `colima start`"
	case strings.Contains(slash, "/.orbstack/"):
		return "OrbStack", "start OrbStack, then run this again"
	case strings.Contains(slash, "/.rd/"):
		return "Rancher Desktop", "start Rancher Desktop, then run this again"
	case strings.Contains(slash, "/.lima/"):
		if p := instance(".lima"); p != "" {
			return "Lima", "start it with `limactl start " + p + "`"
		}
		return "Lima", "start it with `limactl start`"
	case strings.HasSuffix(slash, "-api.sock") && strings.Contains(slash, "/podman/"):
		return "podman machine", "start it with `podman machine start`"
	case slash == "/run/podman/podman.sock":
		return "podman", "start its API socket with `sudo systemctl enable --now podman.socket`"
	case strings.Contains(slash, "/podman/"):
		return "podman", "start its API socket with `systemctl --user enable --now podman.socket`"
	case slash == DefaultSocket || slash == "/run/docker.sock":
		if goos == "darwin" {
			return "Docker Desktop", "start Docker Desktop, then run this again"
		}
		return "Docker Engine", "start it with `sudo systemctl start docker`"
	}
	return "", "start the engine that serves " + addr + ", or point DOCKER_HOST at one that runs"
}
