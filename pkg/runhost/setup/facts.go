package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runhost"
)

// Facts are what the preflight read of a server, by key (assets/preflight.sh).
type Facts map[string]string

// ParseFacts reads the preflight's `key=value` lines; anything else is left
// out.
func ParseFacts(out []byte) Facts {
	f := Facts{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "fact: ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		f[k] = strings.TrimSpace(v)
	}
	return f
}

func (f Facts) yes(k string) bool { return f[k] == "yes" }

func (f Facts) kb(k string) (int64, bool) {
	n, err := strconv.ParseInt(f[k], 10, 64)
	return n, err == nil
}

// Arch is the server's architecture in Go's words (amd64, arm64), "" for one
// a runhost does not run on.
func (f Facts) Arch() string {
	switch f["arch"] {
	case "x86_64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

// Listening is the set of listening sockets the preflight saw.
func (f Facts) Listening() []string { return strings.Fields(f["listening"]) }

// DockerRepoHost is Docker's apt repository, which the server must reach only
// when setup installs Docker Engine.
const DockerRepoHost = "download.docker.com"

// Finding levels.
const (
	LevelOK   = "ok"
	LevelNote = "note"
	LevelStop = "stop"
)

// Finding is one line of the preflight's report.
type Finding struct {
	Level  string `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Supported servers: each distribution and the versions taken, for the
// package manager setup drives.
var supported = map[string][]string{
	"debian": {"12", "13"},
	"ubuntu": {"22.04", "24.04"},
}

// MinSystemd is the oldest systemd setup works with: a system account with
// linger and a user service are all it asks of it.
const MinSystemd = 249

// MinDiskKB and MinMemKB are the free disk under /var/lib and the available
// memory a runhost needs: Docker's images and a run's containers.
const (
	MinDiskKB = 20 * 1000 * 1000
	MinMemKB  = 2 * 1000 * 1000
)

// Plan is what the preflight decided: whether setup may go on, and what the
// install will do about the engine.
type Plan struct {
	Findings []Finding
	// Supported is false when a finding stops setup.
	Supported bool
	// InstallDocker is true when there is no engine and setup installs
	// Docker Engine from Docker's repository.
	InstallDocker bool
	// Arch is the server's architecture in Go's words.
	Arch string
}

// Evaluate decides, from the preflight's facts, whether setup may go on.
// reach names the hosts whose port 443 must answer.
func Evaluate(f Facts, reach []string) Plan {
	p := Plan{Supported: true, Arch: f.Arch()}
	add := func(level, name, format string, args ...any) {
		p.Findings = append(p.Findings, Finding{Level: level, Name: name, Detail: fmt.Sprintf(format, args...)})
		if level == LevelStop {
			p.Supported = false
		}
	}

	id, ver := f["os_id"], f["os_version_id"]
	switch {
	case id == "":
		add(LevelStop, "os", "the server has no /etc/os-release to say what it is")
	case !slices.Contains(supported[id], ver):
		add(LevelStop, "os", "%s is not a server a runhost is set up on yet: Debian 12 or 13, or Ubuntu 22.04 or 24.04", nonEmpty(f["os_pretty"], id+" "+ver))
	case f["os_codename"] == "":
		add(LevelStop, "os", "%s does not name its release's codename, which Docker's repository needs", f["os_pretty"])
	default:
		add(LevelOK, "os", "%s", nonEmpty(f["os_pretty"], id+" "+ver))
	}

	if p.Arch == "" {
		add(LevelStop, "arch", "the server's architecture is %q: a runhost runs on x86_64 (amd64) or aarch64 (arm64)", f["arch"])
	} else {
		add(LevelOK, "arch", "%s (%s)", f["arch"], p.Arch)
	}

	sd, err := strconv.Atoi(f["systemd"])
	switch {
	case f["systemd"] == "" || f["systemd_running"] != "yes":
		add(LevelStop, "systemd", "systemd is not running this server: the runhost's service is a systemd user service")
	case err != nil:
		add(LevelStop, "systemd", "systemd reports a version setup cannot read (%q)", f["systemd"])
	case sd < MinSystemd:
		add(LevelStop, "systemd", "systemd %d is older than %d", sd, MinSystemd)
	default:
		add(LevelOK, "systemd", "systemd %d", sd)
	}

	if f.yes("tpm2") {
		add(LevelOK, "tpm2", "a TPM2 is present: what the runhost keeps at rest is bound to it")
	} else {
		add(LevelNote, "tpm2", "no TPM2: what the runhost keeps at rest is bound to the server's own credential key, which protects a copied disk but not a running root")
	}

	if n, ok := f.kb("disk_free_kb"); !ok {
		add(LevelStop, "disk", "the free disk under /var/lib could not be read")
	} else if n < MinDiskKB {
		add(LevelStop, "disk", "%s free under /var/lib, under the %s a runhost needs", human(n), human(MinDiskKB))
	} else {
		add(LevelOK, "disk", "%s free under /var/lib", human(n))
	}
	if n, ok := f.kb("mem_available_kb"); !ok {
		add(LevelStop, "memory", "the available memory could not be read")
	} else if n < MinMemKB {
		add(LevelStop, "memory", "%s of memory available, under the %s a runhost needs", human(n), human(MinMemKB))
	} else {
		add(LevelOK, "memory", "%s of memory available", human(n))
	}

	var others []string
	for _, e := range []string{"podman", "nerdctl", "containerd", "crio"} {
		if f.yes("engine_" + e) {
			others = append(others, e)
		}
	}
	for _, e := range []string{"lxd", "incus"} {
		if f.yes("engine_" + e) {
			add(LevelNote, "engine", "%s runs here too: its containers are its own, and Docker Engine runs beside it", e)
		}
	}
	switch {
	case f.yes("dockerd"):
		add(LevelOK, "engine", "Docker Engine is installed (%s) and is kept", nonEmpty(f["docker_server"], f["docker_version"]))
	// containerd alone is what Docker Engine itself runs on, so it is no other engine.
	case len(others) > 1 || (len(others) == 1 && others[0] != "containerd"):
		add(LevelStop, "engine", "the server runs another container engine (%s): a runhost uses Docker Engine and will not install it beside another", strings.Join(others, ", "))
	case !f.yes("apt"):
		add(LevelStop, "engine", "there is no Docker Engine, and no apt to install it with")
	default:
		p.InstallDocker = true
		add(LevelOK, "engine", "no engine yet: Docker Engine will be installed from Docker's repository")
	}

	if rt := f["docker_runtimes"]; rt != "" {
		var table map[string]any
		if json.Unmarshal([]byte(rt), &table) == nil {
			var names []string
			for n := range table {
				names = append(names, n)
			}
			slices.Sort(names)
			add(LevelOK, "runtimes", "the daemon registers: %s", strings.Join(names, ", "))
		}
	}

	if f.yes("agentry_user") {
		if f["agentry_home"] != runhost.Home {
			add(LevelStop, "account", "an account named agentry already exists with the home %s; setup will not take it over", f["agentry_home"])
		} else {
			add(LevelOK, "account", "the agentry account from an earlier setup is there and is kept")
		}
	}
	if a := strings.TrimSpace(f["sshd_allow"]); a != "" {
		add(LevelNote, "sshd", "sshd limits who signs in (%s): the agentry account must be allowed, or the run key is refused", strings.TrimSuffix(a, ";"))
	}

	if l := f.Listening(); len(l) > 0 {
		add(LevelNote, "listening", "open now: %s (setup opens none; the docs say how to firewall the server)", strings.Join(l, " "))
	} else {
		add(LevelStop, "listening", "the server's listening sockets could not be read (no ss): setup must show it opens none")
	}

	for _, h := range reach {
		if h == DockerRepoHost && !p.InstallDocker {
			continue
		}
		switch f["reach_"+h] {
		case "yes":
			add(LevelOK, "reach", "%s answers on port 443", h)
		case "no":
			add(LevelStop, "reach", "%s does not answer on port 443 from the server", h)
		default:
			add(LevelNote, "reach", "%s on port 443 was not checked (no bash or timeout on the server)", h)
		}
	}
	return p
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func human(kb int64) string {
	switch {
	case kb >= 1000*1000:
		return fmt.Sprintf("%.1f GB", float64(kb)/1e6)
	case kb >= 1000:
		return fmt.Sprintf("%.0f MB", float64(kb)/1e3)
	}
	return fmt.Sprintf("%d kB", kb)
}

// NewListeners are the listening sockets in after that were not in before.
func NewListeners(before, after []string) []string {
	var out []string
	for _, a := range after {
		if !slices.Contains(before, a) {
			out = append(out, a)
		}
	}
	return out
}
