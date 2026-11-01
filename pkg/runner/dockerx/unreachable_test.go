package dockerx

import (
	"errors"
	"strings"
	"testing"
)

// The sentence names the engine the endpoint belongs to and how to start
// it, per operating system; a socket this user may not open is told apart
// from one with nothing behind it; the client's own error stays beneath.
func TestUnreachableNamesTheEngineAndItsRemedy(t *testing.T) {
	raw := errors.New("docker version: exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	for _, c := range []struct {
		goos, cli, endpoint, want string
	}{
		{"linux", "docker", "unix:///var/run/docker.sock", "Docker Engine is not running (/var/run/docker.sock): start it with `sudo systemctl start docker`"},
		{"darwin", "docker", "unix:///var/run/docker.sock", "Docker Desktop is not running (/var/run/docker.sock): start Docker Desktop, then run this again"},
		{"linux", "docker", "unix:///home/me/.docker/desktop/docker.sock", "Docker Desktop is not running (/home/me/.docker/desktop/docker.sock): start Docker Desktop"},
		{"darwin", "docker", "unix:///Users/me/.colima/default/docker.sock", "Colima is not running (/Users/me/.colima/default/docker.sock): start it with `colima start`"},
		{"darwin", "docker", "unix:///Users/me/.colima/work/docker.sock", "start it with `colima start --profile work`"},
		{"darwin", "docker", "unix:///Users/me/.orbstack/run/docker.sock", "OrbStack is not running"},
		{"darwin", "docker", "unix:///Users/me/.rd/docker.sock", "Rancher Desktop is not running"},
		{"darwin", "docker", "unix:///Users/me/.lima/dev/sock/docker.sock", "Lima is not running (/Users/me/.lima/dev/sock/docker.sock): start it with `limactl start dev`"},
		{"linux", "docker", "unix:///run/user/1000/podman/podman.sock", "podman is not running (/run/user/1000/podman/podman.sock): start its API socket with `systemctl --user enable --now podman.socket`"},
		{"linux", "docker", "unix:///run/podman/podman.sock", "start its API socket with `sudo systemctl enable --now podman.socket`"},
		{"darwin", "docker", "unix:///var/folders/x/T/podman/podman-machine-default-api.sock", "podman machine is not running"},
		{"windows", "docker", "npipe:////./pipe/docker_engine", "Docker Desktop is not running (//./pipe/docker_engine): start Docker Desktop"},
		{"linux", "docker", "tcp://10.0.0.5:2376", "no container engine answers at tcp://10.0.0.5:2376: check that the host it names is up"},
		{"linux", "docker", "unix:///opt/odd/engine.sock", "no container engine answers at unix:///opt/odd/engine.sock: start the engine that serves /opt/odd/engine.sock"},
		{"linux", "docker", "", "no container engine answers the docker CLI: start Docker Engine with `sudo systemctl start docker`, or start Docker Desktop or podman"},
		{"darwin", "podman", "", "podman machine is not running: start it with `podman machine start`"},
		{"linux", "podman", "", "podman is not running: run `podman info` to see why it does not answer"},
	} {
		u := unreachableOn(c.goos, c.cli, c.endpoint, raw)
		if !strings.Contains(u.Error(), c.want) {
			t.Errorf("%s %s %q:\n  got  %s\n  want …%s…", c.goos, c.cli, c.endpoint, u.Error(), c.want)
		}
		if u.Detail() != raw.Error() || !errors.Is(u, raw) {
			t.Errorf("%q: the client's error is not kept beneath: %q", c.endpoint, u.Detail())
		}
	}
	denied := unreachableOn("linux", "docker", "unix:///var/run/docker.sock",
		errors.New("docker version: exit status 1: permission denied while trying to connect to the Docker daemon socket"))
	if want := "this user may not use Docker Engine's socket (/var/run/docker.sock): add yourself to the docker group with `sudo usermod -aG docker $USER`, then sign in again"; denied.Error() != want {
		t.Errorf("denied:\n  got  %s\n  want %s", denied.Error(), want)
	}
}
