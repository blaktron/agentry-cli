package dockerx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The configuration's credential helpers are held to PATH: a credsStore
// whose program is missing is a gap for every registry, a credHelpers
// entry's for its registry alone, and a present one is none. A file that is
// absent or not JSON names nothing.
func TestMissingCredentialHelpersReadsTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auths":{},"credsStore":"desktop","credHelpers":{"ghcr.io":"gh","gcr.io":"gcloud"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	have := func(name string) (string, error) {
		if name == "docker-credential-gcloud" {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	got := MissingCredentialHelpers(path, have)
	if len(got) != 2 {
		t.Fatalf("gaps = %+v, want the store's and ghcr.io's", got)
	}
	if !got[0].Store || got[0].Helper != "desktop" || got[0].Program != "docker-credential-desktop" || got[0].Config != path {
		t.Errorf("the store's gap = %+v", got[0])
	}
	if got[1].Store || got[1].Registry != "ghcr.io" || got[1].Helper != "gh" {
		t.Errorf("the registry's gap = %+v", got[1])
	}
	// A registry entry naming the store's own missing helper adds nothing.
	if err := os.WriteFile(path, []byte(`{"credsStore":"desktop","credHelpers":{"ghcr.io":"desktop"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := MissingCredentialHelpers(path, have); len(got) != 1 || !got[0].Store {
		t.Errorf("one helper twice, gaps = %+v", got)
	}
	all := func(string) (string, error) { return "/usr/bin/x", nil }
	if got := MissingCredentialHelpers(path, all); got != nil {
		t.Errorf("every helper present, gaps = %+v", got)
	}
	if got := MissingCredentialHelpers(filepath.Join(dir, "none.json"), have); got != nil {
		t.Errorf("no file, gaps = %+v", got)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := MissingCredentialHelpers(path, have); got != nil {
		t.Errorf("not JSON, gaps = %+v", got)
	}
	if got := MissingCredentialHelpers("", have); got != nil {
		t.Errorf("no path, gaps = %+v", got)
	}
}

// ConfigFile follows DOCKER_CONFIG, then the home directory.
func TestConfigFileFollowsDockerConfig(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/etc/dockercfg")
	if got := ConfigFile(); got != filepath.Join("/etc/dockercfg", "config.json") {
		t.Errorf("ConfigFile() = %q", got)
	}
	t.Setenv("DOCKER_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := ConfigFile(); got != filepath.Join(home, ".docker", "config.json") {
		t.Errorf("ConfigFile() = %q", got)
	}
}

// Each platform's engines are listed where they put their sockets, per
// profile for Colima and Lima, and nothing on Windows.
func TestHostSocketsPerPlatform(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".colima/default", ".colima/work", ".lima/docker"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := func(ss []HostSocket) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Name+" "+s.Path)
		}
		return out
	}
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "podman"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"podman-machine-default-api.sock", "work-api.sock", "podman-machine-default-gvproxy.sock"} {
		if err := os.WriteFile(filepath.Join(tmp, "podman", f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mac := paths(hostSocketsFor("darwin", home, "", tmp))
	want := []string{
		"Docker Desktop " + filepath.Join(home, ".docker/run/docker.sock"),
		"Docker Desktop /var/run/docker.sock",
		"Colima " + filepath.Join(home, ".colima/default/docker.sock"),
		"Colima " + filepath.Join(home, ".colima/work/docker.sock"),
		"OrbStack " + filepath.Join(home, ".orbstack/run/docker.sock"),
		"Rancher Desktop " + filepath.Join(home, ".rd/docker.sock"),
		"Lima " + filepath.Join(home, ".lima/docker/sock/docker.sock"),
		"podman machine " + filepath.Join(tmp, "podman/podman-machine-default-api.sock"),
		"podman machine " + filepath.Join(tmp, "podman/work-api.sock"),
	}
	if len(mac) != len(want) {
		t.Fatalf("darwin = %q, want %q", mac, want)
	}
	for i := range want {
		if mac[i] != want[i] {
			t.Errorf("darwin[%d] = %q, want %q", i, mac[i], want[i])
		}
	}
	linux := paths(hostSocketsFor("linux", home, "/run/user/1000", ""))
	wantLinux := []string{
		"Docker Engine /var/run/docker.sock",
		"Docker Desktop " + filepath.Join(home, ".docker/desktop/docker.sock"),
		"podman /run/user/1000/podman/podman.sock",
		"podman (rootful) /run/podman/podman.sock",
	}
	if len(linux) != len(wantLinux) {
		t.Fatalf("linux = %q, want %q", linux, wantLinux)
	}
	for i := range wantLinux {
		if linux[i] != wantLinux[i] {
			t.Errorf("linux[%d] = %q, want %q", i, linux[i], wantLinux[i])
		}
	}
	if got := hostSocketsFor("linux", "", "", ""); len(got) != 2 {
		t.Errorf("linux with no home or runtime dir = %+v, want the two fixed paths", got)
	}
	if got := hostSocketsFor("windows", home, "", ""); got != nil {
		t.Errorf("windows = %+v, want nothing", got)
	}
}

// A socket file with nothing behind it is a leftover; one that answers is
// an engine; an absent path is nothing.
func TestLeftSocketsAreTheOnesThatDoNotAnswer(t *testing.T) {
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	answering := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", answering)
	if err != nil {
		t.Skip("no Unix sockets here:", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })) //nolint:gosec // a test server on a private socket
	}()
	dead := filepath.Join(dir, "dead.sock")
	dl, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	_ = dl.Close()
	if err := os.WriteFile(dead, nil, 0o600); err != nil { // the file a closed listener may have removed
		t.Fatal(err)
	}
	got := LeftSockets(context.Background(), []HostSocket{
		{"Live", answering}, {"Dead", dead}, {"Dead again", dead}, {"Absent", filepath.Join(dir, "none.sock")}, {"Empty", ""},
	})
	if len(got) != 1 || got[0].Name != "Dead" || got[0].Path != dead || got[0].Err == nil {
		t.Fatalf("left = %+v, want the dead one once", got)
	}
}

// A socket an engine left is said with what to do about it: Rancher
// Desktop's, when its settings name the containerd backend, is that
// backend, which serves no Docker API; any other is a stopped engine.
func TestLeftSocketMessage(t *testing.T) {
	home := t.TempDir()
	rd := filepath.Join(home, ".rd", "docker.sock")
	settings := filepath.Join(home, "Library", "Preferences", "rancher-desktop", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := func(name string) {
		t.Helper()
		if err := os.WriteFile(settings, []byte(`{"version":19,"containerEngine":{"name":"`+name+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	left := func(name, path string) LeftSocket {
		return LeftSocket{HostSocket: HostSocket{Name: name, Path: path}, Err: errors.New("no answer: EOF")}
	}
	stopped := "Rancher Desktop left a socket at " + rd + " with nothing answering behind it (no answer: EOF): start it, or remove the file"

	backend("containerd")
	got := left("Rancher Desktop", rd).messageFor("darwin")
	if want := "Rancher Desktop runs its containerd backend, which serves no Docker API at " + rd + " (no answer: EOF): choose dockerd (moby) under Rancher Desktop's Preferences, Container Engine"; got != want {
		t.Errorf("containerd: %q, want %q", got, want)
	}
	for _, w := range []string{"Rancher Desktop runs its containerd backend, which serves no Docker API", rd, "no answer: EOF", "dockerd (moby)", "Container Engine"} {
		if !strings.Contains(got, w) {
			t.Errorf("containerd: %q lacks %q", got, w)
		}
	}
	if strings.Contains(got, "start it") {
		t.Errorf("containerd is running; it is not to be started: %q", got)
	}
	if got := left("Rancher Desktop", rd).messageFor("linux"); got != stopped {
		t.Errorf("no Rancher settings read off macOS: %q", got)
	}
	backend("moby")
	if got := left("Rancher Desktop", rd).messageFor("darwin"); got != stopped {
		t.Errorf("moby: %q, want %q", got, stopped)
	}
	if err := os.WriteFile(settings, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := left("Rancher Desktop", rd).messageFor("darwin"); got != stopped {
		t.Errorf("unreadable settings: %q", got)
	}
	_ = os.Remove(settings)
	if got := left("Rancher Desktop", rd).messageFor("darwin"); got != stopped {
		t.Errorf("no settings: %q", got)
	}
	backend("containerd")
	colima := filepath.Join(home, ".colima", "default", "docker.sock")
	if got, want := left("Colima", colima).messageFor("darwin"), "Colima left a socket at "+colima+" with nothing answering behind it (no answer: EOF): start it, or remove the file"; got != want {
		t.Errorf("another engine: %q, want %q", got, want)
	}
}
