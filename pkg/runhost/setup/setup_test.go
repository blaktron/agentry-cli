package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

func debian13() Facts {
	return ParseFacts([]byte(strings.Join([]string{
		"os_id=debian", "os_version_id=13", "os_pretty=Debian GNU/Linux 13 (trixie)", "os_codename=trixie",
		"arch=x86_64", "systemd=257", "systemd_running=yes", "tpm2=no",
		"disk_free_kb=38000000", "mem_available_kb=2600000", "dockerd=no", "apt=yes",
		"listening=tcp/0.0.0.0:22 tcp/[::]:22", "reach_ghcr.io=yes",
		"this line is not a fact", "",
	}, "\n")))
}

func findingOf(p Plan, name string) Finding {
	for _, f := range p.Findings {
		if f.Name == name {
			return f
		}
	}
	return Finding{}
}

func TestASupportedServerWithNoEngineGetsDocker(t *testing.T) {
	p := Evaluate(debian13(), []string{"ghcr.io"})
	if !p.Supported || !p.InstallDocker || p.Arch != "amd64" {
		t.Fatalf("plan %+v", p)
	}
	if f := findingOf(p, "tpm2"); f.Level != LevelNote {
		t.Errorf("no TPM2 is a note, not %+v", f)
	}
}

func TestEachSupportedReleaseIsTaken(t *testing.T) {
	for _, r := range []struct{ id, ver, sd, pkg, repo, rel string }{
		{"debian", "12", "252", "apt", "debian", "12"}, {"debian", "13", "257", "apt", "debian", "13"},
		{"ubuntu", "22.04", "249", "apt", "ubuntu", "22.04"}, {"ubuntu", "24.04", "255", "apt", "ubuntu", "24.04"},
		{"fedora", "43", "258", "dnf", "fedora", "43"}, {"fedora", "44", "259", "dnf", "fedora", "44"},
		{"rhel", "9.6", "252", "dnf", "rhel", "9"}, {"rhel", "10.0", "257", "dnf", "rhel", "10"},
		{"almalinux", "9.6", "252", "dnf", "rhel", "9"}, {"almalinux", "10.2", "257", "dnf", "rhel", "10"},
		{"rocky", "9.6", "252", "dnf", "rhel", "9"}, {"rocky", "10.0", "257", "dnf", "rhel", "10"},
	} {
		f := debian13()
		f["os_id"], f["os_version_id"], f["systemd"] = r.id, r.ver, r.sd
		if r.pkg == "dnf" {
			// Docker's RPM tree needs no codename, and these servers have dnf.
			delete(f, "os_codename")
			delete(f, "apt")
			f["dnf"] = "yes"
		}
		p := Evaluate(f, nil)
		if !p.Supported || !p.InstallDocker {
			t.Errorf("%s %s with systemd %s refused: %+v", r.id, r.ver, r.sd, p.Findings)
		}
		if p.Package != r.pkg || p.DockerRepo != r.repo || p.OSRelease != r.rel {
			t.Errorf("%s %s installs Docker with %s from %s/%s", r.id, r.ver, p.Package, p.DockerRepo, p.OSRelease)
		}
	}
}

func TestAnIdlePodmanIsANoteAndARunningOneStops(t *testing.T) {
	f := debian13()
	f["os_id"], f["os_version_id"], f["dnf"], f["engine_podman"], f["podman_running"] = "fedora", "44", "yes", "yes", "0"
	if p := Evaluate(f, nil); !p.Supported || !p.InstallDocker {
		t.Fatalf("an idle podman stopped setup: %+v", p.Findings)
	}
	f["podman_running"] = "2"
	if p := Evaluate(f, nil); p.Supported || !strings.Contains(findingStop(p), "podman, with 2 container(s) running") {
		t.Fatalf("a podman running containers: %+v", p.Findings)
	}
}

func findingStop(p Plan) string {
	for _, f := range p.Findings {
		if f.Level == LevelStop {
			return f.Detail
		}
	}
	return ""
}

func TestPreflightStopsWhatCannotBeARunhost(t *testing.T) {
	cases := map[string]func(Facts){
		"fedora 42":              func(f Facts) { f["os_id"], f["os_version_id"], f["dnf"] = "fedora", "42", "yes" },
		"rocky 8":                func(f Facts) { f["os_id"], f["os_version_id"], f["dnf"] = "rocky", "8.10", "yes" },
		"fedora without dnf":     func(f Facts) { f["os_id"], f["os_version_id"] = "fedora", "44" },
		"debian 11":              func(f Facts) { f["os_version_id"] = "11" },
		"no os-release":          func(f Facts) { delete(f, "os_id") },
		"riscv":                  func(f Facts) { f["arch"] = "riscv64" },
		"old systemd":            func(f Facts) { f["systemd"] = "247" },
		"systemd not running":    func(f Facts) { f["systemd_running"] = "no" },
		"no systemd":             func(f Facts) { delete(f, "systemd") },
		"small disk":             func(f Facts) { f["disk_free_kb"] = "9000000" },
		"little memory":          func(f Facts) { f["mem_available_kb"] = "1000000" },
		"podman running":         func(f Facts) { f["engine_podman"], f["podman_running"] = "yes", "1" },
		"no apt and no engine":   func(f Facts) { delete(f, "apt") },
		"ghcr unreachable":       func(f Facts) { f["reach_ghcr.io"] = "no" },
		"someone else's agentry": func(f Facts) { f["agentry_user"], f["agentry_home"] = "yes", "/home/agentry" },
		"no codename":            func(f Facts) { delete(f, "os_codename") },
	}
	for name, change := range cases {
		f := debian13()
		change(f)
		if p := Evaluate(f, []string{"ghcr.io"}); p.Supported {
			t.Errorf("%s: setup would go on: %+v", name, p.Findings)
		}
	}
}

func TestLXDBesideDockerIsANote(t *testing.T) {
	f := debian13()
	f["engine_lxd"] = "yes"
	if p := Evaluate(f, nil); !p.Supported || !p.InstallDocker || findingOf(p, "engine").Level == LevelStop {
		t.Fatalf("plan %+v", p)
	}
}

func TestAnExistingDockerEngineIsKept(t *testing.T) {
	f := debian13()
	f["dockerd"], f["docker_server"], f["engine_containerd"] = "yes", "29.1.0", "yes"
	// Docker's repository is not needed, so its being unreachable stops nothing.
	f["reach_"+DockerRepoHost] = "no"
	p := Evaluate(f, []string{"ghcr.io", DockerRepoHost})
	if !p.Supported || p.InstallDocker {
		t.Fatalf("plan %+v", p)
	}
	f["dockerd"] = "no"
	if Evaluate(f, []string{"ghcr.io", DockerRepoHost}).Supported {
		t.Fatal("a server that must install Docker went on without reaching Docker's repository")
	}
}

func TestNewListenersAreThoseSetupAdded(t *testing.T) {
	before := []string{"tcp/0.0.0.0:22", "udp/127.0.0.53:53"}
	if got := NewListeners(before, []string{"udp/127.0.0.53:53", "tcp/0.0.0.0:22"}); len(got) != 0 {
		t.Fatalf("no listener was added, got %v", got)
	}
	if got := NewListeners(before, []string{"tcp/0.0.0.0:22", "tcp/0.0.0.0:2375"}); len(got) != 1 || got[0] != "tcp/0.0.0.0:2375" {
		t.Fatalf("got %v", got)
	}
}

func TestMergeRuntimesKeepsTheRestAndIsIdempotent(t *testing.T) {
	existing := []byte(`{"log-driver":"journald","runtimes":{"crun":{"path":"/usr/bin/crun"}}}`)
	out, changed, err := MergeRuntimes(existing)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	rt := doc["runtimes"].(map[string]any)
	if doc["log-driver"] != "journald" || rt["crun"] == nil || rt["runsc"] == nil || rt["runsc-uds"] == nil {
		t.Fatalf("merged %s", out)
	}
	uds := rt["runsc-uds"].(map[string]any)
	if uds["path"] != runhost.RunscPath || uds["runtimeArgs"].([]any)[0] != "--host-uds=open" {
		t.Fatalf("runsc-uds %v", uds)
	}
	again, changed, err := MergeRuntimes(out)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Fatalf("a second merge changed the file: %v %v", changed, err)
	}
	back, changed, err := RemoveRuntimes(out)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	var after map[string]any
	_ = json.Unmarshal(back, &after)
	if r := after["runtimes"].(map[string]any); r["runsc"] != nil || r["crun"] == nil {
		t.Fatalf("after removal %s", back)
	}
}

func TestMergeRuntimesOnNoFile(t *testing.T) {
	out, changed, err := MergeRuntimes(nil)
	if err != nil || !changed || !strings.Contains(string(out), runhost.RunscPath) {
		t.Fatalf("%s %v %v", out, changed, err)
	}
}

func TestAPersonsOwnRunscIsNotTakenOver(t *testing.T) {
	if _, _, err := MergeRuntimes([]byte(`{"runtimes":{"runsc":{"path":"/usr/bin/runsc"}}}`)); err == nil {
		t.Fatal("setup would take over a runsc registration it did not make")
	}
	if _, _, err := MergeRuntimes([]byte(`{not json`)); err == nil {
		t.Fatal("an unreadable daemon.json was overwritten")
	}
}

func TestAuthorizedKeysKeepOtherDevicesAndReplaceThisOne(t *testing.T) {
	other := runhost.AuthorizedKeyLine("ssh-ed25519 OTHER", "other")
	old := runhost.AuthorizedKeyLine("ssh-ed25519 OLD", "me")
	got := string(mergeAuthorizedKeys([]byte(other+"\n"+old+"\n\n"), runhost.AuthorizedKeyLine("ssh-ed25519 NEW", "me"), "me"))
	if !strings.Contains(got, "OTHER") || strings.Contains(got, "OLD") || strings.Count(got, "NEW") != 1 || strings.Count(got, "\n") != 2 {
		t.Fatalf("merged:\n%s", got)
	}
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		id := strings.TrimPrefix(l[strings.LastIndex(l, " ")+1:], runhost.DeviceComment(""))
		if !strings.HasPrefix(l, `restrict,command="`+runhost.ServeCommandFor(id)+`" `) {
			t.Errorf("a line without its device's forced command: %s", l)
		}
	}
}

func TestDevicesReplaceThisDevicesEntry(t *testing.T) {
	first, _ := mergeDevices(nil, runhost.Device{ID: "a", Label: "laptop"})
	second, _ := mergeDevices(first, runhost.Device{ID: "b", Label: "desk"})
	third, err := mergeDevices(second, runhost.Device{ID: "a", Label: "laptop-2"})
	if err != nil {
		t.Fatal(err)
	}
	var ds runhost.Devices
	_ = json.Unmarshal(third, &ds)
	if len(ds.Devices) != 2 || ds.Devices[0].ID != "b" || ds.Devices[1].Label != "laptop-2" {
		t.Fatalf("devices %+v", ds.Devices)
	}
	if _, err := mergeDevices([]byte("{"), runhost.Device{ID: "a"}); err == nil {
		t.Fatal("an unreadable devices file was overwritten")
	}
}

func TestEnvFileQuotesEveryValue(t *testing.T) {
	got := string(envFile(map[string]string{"B": "it's", "A": "x y"}))
	if got != "A='x y'\nB='it'\\''s'\n" {
		t.Fatalf("%q", got)
	}
}

func zstdTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for name, body := range entries {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(name, "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	var z bytes.Buffer
	zw, _ := zstd.NewWriter(&z)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	return z.Bytes()
}

func TestRepackGVisorKeepsRunscAndItsHelpersOnly(t *testing.T) {
	in := zstdTar(t, map[string]string{
		"runsc": "RUNSC", "containerd-shim-runsc-v1": "SHIM", "gvisor-bin/": "", "gvisor-bin/gvisor_sentry": "S",
		"../escape": "X", "/abs": "Y",
	})
	out, err := RepackGVisor(in)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got["runsc"] != "RUNSC" || got["gvisor-bin/gvisor_sentry"] != "S" || len(got) != 3 {
		t.Fatalf("repacked %v", got)
	}
	if _, err := RepackGVisor(zstdTar(t, map[string]string{"gvisor-bin/x": "x"})); err == nil {
		t.Fatal("an archive with no runsc was accepted")
	}
}

func TestAddRefusesABuildThatIsNotARelease(t *testing.T) {
	for _, v := range []string{"0.0.0-dev", "v0.6.1-3-gabcdef", "0.6.1-dirty"} {
		if _, err := Add(t.Context(), Options{Name: "lab", Version: v}); err != ErrNotARelease {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// A refusal of the address or host-key check, which setup shares with
// target hosts, names the runhost being set up.
func TestSetupsHostRefusalsNameTheRunhost(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "clirelease", "testdata", "release-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clirelease.ParseKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Add(t.Context(), Options{Name: "lab", Version: "0.6.1", Keys: keys,
		Answer: targethost.Answer{User: "admin", Host: "127.0.0.1", Port: 22}})
	var r *targethost.Refusal
	if !errors.As(err, &r) || !strings.HasPrefix(err.Error(), "runhost lab: ") {
		t.Fatalf("Add's address refusal = %v", err)
	}
	err = Remove(t.Context(), RemoveOptions{
		Record: runhost.Record{Name: "lab", Host: "127.0.0.1", Port: 22, HostKey: testHostKey(t)},
		Answer: targethost.Answer{User: "admin"}})
	if !errors.As(err, &r) || !strings.HasPrefix(err.Error(), "runhost lab: ") {
		t.Fatalf("Remove's address refusal = %v", err)
	}
	for _, detail := range []string{targethost.RefusedKeyUnknown, targethost.RefusedKeyChanged, targethost.RefusedUnreached} {
		err := asRunhost(&targethost.Refusal{ID: "lab", Detail: detail, Err: errors.New("the key")})
		if want := "runhost lab: the key"; err.Error() != want {
			t.Errorf("%s: %q, want %q", detail, err, want)
		}
	}
}

func testHostKey(t *testing.T) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestMergeConfigKeepsWhatSetupFoundAndBindsAnOwner(t *testing.T) {
	devices := runhost.Owner{Kind: runhost.OwnerDevices}
	me := runhost.Owner{Kind: runhost.OwnerAccount, Registry: "https://registry.example", Handle: "me"}
	first, err := mergeConfig(nil, runhost.ServerConfig{Name: "lab", Owner: devices, InstalledDocker: true, GVisor: "1", SetUpAt: "t1"})
	if err != nil || !first.InstalledDocker || first.Owner != devices {
		t.Fatalf("%+v %v", first, err)
	}
	b, _ := json.Marshal(first)
	second, err := mergeConfig(b, runhost.ServerConfig{Name: "other", Owner: me, InstalledDocker: false, GVisor: "2", SetUpAt: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Owner != me || !second.InstalledDocker || second.GVisor != "2" || second.SetUpAt != "t1" || second.Name != "lab" {
		t.Fatalf("after a signed-in add: %+v", second)
	}
	b, _ = json.Marshal(second)
	if _, err := mergeConfig(b, runhost.ServerConfig{Owner: runhost.Owner{Kind: runhost.OwnerAccount, Registry: "https://registry.example", Handle: "someone-else"}}); err == nil {
		t.Fatal("another account took over the runhost")
	}
	// A device pairs with an account's runhost only signed in to it.
	if _, err := mergeConfig(b, runhost.ServerConfig{Owner: devices}); err == nil {
		t.Fatal("a signed-out device paired with an account's runhost")
	}
}

func TestTheStagingPathIsMktempsAndNothingElse(t *testing.T) {
	if !stageRE.MatchString("/tmp/agentry-runhost.Ab12Cd34") {
		t.Fatal("mktemp's own path refused")
	}
	for _, bad := range []string{"/tmp/agentry-runhost.Ab12Cd34/..", "/tmp/agentry-runhost.../etc", "/tmp/agentry-runhost.Ab12Cd34 /", "/", "/tmp/agentry-runhost."} {
		if stageRE.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheRedactorMasksAPasswordTypedLater(t *testing.T) {
	r := newRedactor(AdminAuth{Password: "sign-in-pw"})
	r.add("typed-sudo-pw")
	var said []string
	say := r.say(func(s string) { said = append(said, s) })
	say("sudo said: %s / %s", "typed-sudo-pw", "sign-in-pw")
	err := r.err(errors.New("exit 1: typed-sudo-pw is wrong"))
	if strings.Contains(said[0], "pw") || strings.Contains(err.Error(), "typed-sudo-pw") {
		t.Fatalf("a secret got through: %q, %v", said[0], err)
	}
}

func TestFetchGVisorRefusesAnArchiveOtherThanThePinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zstdTar(t, map[string]string{"runsc": "not the pinned runsc"}))
	}))
	defer srv.Close()
	old := GVisorBaseURL
	GVisorBaseURL = srv.URL
	defer func() { GVisorBaseURL = old }()
	if _, err := FetchGVisor(t.Context(), srv.Client(), "x86_64"); err == nil || !strings.Contains(err.Error(), "SHA-512") {
		t.Fatalf("a changed archive: %v", err)
	}
	if _, err := FetchGVisor(t.Context(), srv.Client(), "riscv64"); err == nil {
		t.Fatal("an architecture with no pin was fetched")
	}
}

func TestAnOldSystemdIsANoteThatSchedulesNeedANewerOne(t *testing.T) {
	f := debian13()
	f["systemd"] = "252"
	p := Evaluate(f, nil)
	if !p.Supported || findingOf(p, "systemd").Level != LevelNote || !strings.Contains(findingOf(p, "systemd").Detail, "schedules need 256") {
		t.Fatalf("plan %+v", p.Findings)
	}
	f["systemd"] = "257"
	if findingOf(Evaluate(f, nil), "systemd").Level != LevelOK {
		t.Fatal("systemd 257 is noted")
	}
}
