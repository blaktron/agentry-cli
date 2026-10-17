package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
)

// Two things a machine can carry that make an engine that answers fail
// anyway, or make one that does not answer look present: a credential
// helper the docker CLI's configuration names and PATH lacks, which fails
// every image pull; and a socket file an engine left behind when it
// stopped, which answers nothing. doctor and the desktop app check both,
// here so they read the same files and say the same things.

// ConfigFile is the docker CLI's configuration file, config.json under
// DOCKER_CONFIG or under ~/.docker; "" when neither names a directory.
func ConfigFile() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// CredentialHelperGap is a credential helper the docker configuration
// names and PATH lacks: the docker CLI runs docker-credential-<Helper> for
// every pull the entry covers and fails the pull when it is not found.
type CredentialHelperGap struct {
	// Config is the file that names it.
	Config string
	// Helper is the entry's value, Program the binary it names.
	Helper  string
	Program string
	// Store says the entry is credsStore, which covers every registry;
	// otherwise Registry is the credHelpers key it covers.
	Store    bool
	Registry string
}

// MissingCredentialHelpers reads the docker configuration at path for
// credsStore and credHelpers entries whose docker-credential-<name> program
// lookPath cannot find (exec.LookPath when nil). A file that is absent or
// not JSON names nothing. The gaps come sorted, the store's first, one per
// helper: a registry entry naming the store's own missing helper adds
// nothing.
func MissingCredentialHelpers(path string, lookPath func(string) (string, error)) []CredentialHelperGap {
	if path == "" {
		return nil
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	b, err := os.ReadFile(path) //nolint:gosec // the docker CLI's own configuration file
	if err != nil {
		return nil
	}
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	var out []CredentialHelperGap
	seen := map[string]bool{}
	missing := func(helper string) bool {
		if helper == "" || seen[helper] {
			return false
		}
		seen[helper] = true
		_, err := lookPath("docker-credential-" + helper)
		return err != nil
	}
	if missing(cfg.CredsStore) {
		out = append(out, CredentialHelperGap{Config: path, Helper: cfg.CredsStore, Program: "docker-credential-" + cfg.CredsStore, Store: true})
	}
	registries := make([]string, 0, len(cfg.CredHelpers))
	for r := range cfg.CredHelpers {
		registries = append(registries, r)
	}
	sort.Strings(registries)
	for _, r := range registries {
		if h := cfg.CredHelpers[r]; missing(h) {
			out = append(out, CredentialHelperGap{Config: path, Helper: h, Program: "docker-credential-" + h, Registry: r})
		}
	}
	return out
}

// HostSocket is one place an engine on this machine serves the Docker API
// over a Unix socket, named for the engine that puts it there.
type HostSocket struct {
	Name string
	Path string
}

// HostSockets lists where each engine puts its socket on this machine:
// Docker Engine's own and Docker Desktop's, the VM engines' host sockets on
// macOS (Colima's and Lima's per profile or instance, OrbStack's, Rancher
// Desktop's dockerd, each podman machine's), podman's own on Linux. A path
// is listed whether or not it exists. Windows engines serve named pipes,
// which the docker CLI's own context reaches, so nothing is listed there.
func HostSockets() []HostSocket {
	home, _ := os.UserHomeDir()
	return hostSocketsFor(runtime.GOOS, home, os.Getenv("XDG_RUNTIME_DIR"), os.TempDir())
}

// hostSocketsFor is HostSockets with its inputs passed in. tmpDir is the
// person's temporary directory, where a podman machine on macOS serves its
// API as podman/<machine>-api.sock.
func hostSocketsFor(goos, home, runtimeDir, tmpDir string) []HostSocket {
	under := func(rel string) string {
		if home == "" {
			return ""
		}
		return filepath.Join(home, rel)
	}
	var out []HostSocket
	add := func(name, path string) {
		if path != "" {
			out = append(out, HostSocket{name, path})
		}
	}
	switch goos {
	case "darwin":
		add("Docker Desktop", under(".docker/run/docker.sock"))
		add("Docker Desktop", DefaultSocket)
		for _, s := range perInstanceSockets("Colima", under(".colima"), "docker.sock") {
			add(s.Name, s.Path)
		}
		add("OrbStack", under(".orbstack/run/docker.sock"))
		add("Rancher Desktop", under(".rd/docker.sock"))
		for _, s := range perInstanceSockets("Lima", under(".lima"), filepath.Join("sock", "docker.sock")) {
			add(s.Name, s.Path)
		}
		for _, s := range podmanMachineSockets(tmpDir) {
			add(s.Name, s.Path)
		}
	case "linux":
		add("Docker Engine", DefaultSocket)
		add("Docker Desktop", under(".docker/desktop/docker.sock"))
		if runtimeDir != "" {
			add("podman", filepath.Join(runtimeDir, "podman", "podman.sock"))
		}
		add("podman (rootful)", "/run/podman/podman.sock")
	}
	return out
}

// perInstanceSockets lists <dir>/<instance>/<rel> for every directory
// under dir, sorted: Colima's profiles and Lima's instances each keep a
// socket of their own.
func perInstanceSockets(name, dir, rel string) []HostSocket {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []HostSocket
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, HostSocket{name, filepath.Join(dir, e.Name(), rel)})
		}
	}
	return out
}

// podmanMachineSockets lists the API socket of every podman machine whose
// files are under tmpDir, sorted by machine name.
func podmanMachineSockets(tmpDir string) []HostSocket {
	if tmpDir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(tmpDir, "podman", "*-api.sock"))
	sort.Strings(paths)
	out := make([]HostSocket, 0, len(paths))
	for _, p := range paths {
		out = append(out, HostSocket{"podman machine", p})
	}
	return out
}

// LeftSocket is a socket file with nothing answering behind it: the engine
// that put it there has stopped, or was removed and left it.
type LeftSocket struct {
	HostSocket
	Err error
}

// Message says what the socket is and what to do about it. Rancher
// Desktop keeps its socket file when it runs its containerd backend, which
// serves no Docker API, so that one is named from Rancher's own settings
// rather than taken for a stopped engine.
func (s LeftSocket) Message() string { return s.messageFor(runtime.GOOS) }

func (s LeftSocket) messageFor(goos string) string {
	if rancherBackend(goos, s.Path) == "containerd" {
		return fmt.Sprintf("Rancher Desktop runs its containerd backend, which serves no Docker API at %s (%v): choose dockerd (moby) under Rancher Desktop's Preferences, Container Engine", s.Path, s.Err)
	}
	return fmt.Sprintf("%s left a socket at %s with nothing answering behind it (%v): start it, or remove the file", s.Name, s.Path, s.Err)
}

// rancherBackend is the container engine Rancher Desktop's settings name
// ("moby" or "containerd") when path is its socket, ~/.rd/docker.sock, on
// macOS, where its settings are
// ~/Library/Preferences/rancher-desktop/settings.json; "" otherwise.
func rancherBackend(goos, path string) string {
	if goos != "darwin" || filepath.Base(path) != "docker.sock" || filepath.Base(filepath.Dir(path)) != ".rd" {
		return ""
	}
	home := filepath.Dir(filepath.Dir(path))
	raw, err := os.ReadFile(filepath.Join(home, "Library", "Preferences", "rancher-desktop", "settings.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		ContainerEngine struct {
			Name string `json:"name"`
		} `json:"containerEngine"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return cfg.ContainerEngine.Name
}

// LeftSockets pings each socket of sockets that exists as a file and
// returns those that do not answer the Docker API's /_ping, in order. An
// absent file is nothing; a socket that answers is an engine, not a
// leftover. Paths are not repeated.
func LeftSockets(ctx context.Context, sockets []HostSocket) []LeftSocket {
	var out []LeftSocket
	seen := map[string]bool{}
	for _, s := range sockets {
		if s.Path == "" || seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		if _, err := os.Lstat(s.Path); err != nil {
			continue
		}
		if err := PingSocket(ctx, s.Path); err != nil {
			out = append(out, LeftSocket{HostSocket: s, Err: err})
		}
	}
	return out
}
