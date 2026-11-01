package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/blaktron/agentry-cli/pkg/runhost"
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
	for _, r := range []struct{ id, ver, sd string }{{"debian", "12", "252"}, {"debian", "13", "257"}, {"ubuntu", "22.04", "249"}, {"ubuntu", "24.04", "255"}} {
		f := debian13()
		f["os_id"], f["os_version_id"], f["systemd"] = r.id, r.ver, r.sd
		if p := Evaluate(f, nil); !p.Supported {
			t.Errorf("%s %s with systemd %s refused: %+v", r.id, r.ver, r.sd, p.Findings)
		}
	}
}

func TestPreflightStopsWhatCannotBeARunhost(t *testing.T) {
	cases := map[string]func(Facts){
		"fedora (not yet)":       func(f Facts) { f["os_id"], f["os_version_id"] = "fedora", "44" },
		"debian 11":              func(f Facts) { f["os_version_id"] = "11" },
		"no os-release":          func(f Facts) { delete(f, "os_id") },
		"riscv":                  func(f Facts) { f["arch"] = "riscv64" },
		"old systemd":            func(f Facts) { f["systemd"] = "247" },
		"systemd not running":    func(f Facts) { f["systemd_running"] = "no" },
		"no systemd":             func(f Facts) { delete(f, "systemd") },
		"small disk":             func(f Facts) { f["disk_free_kb"] = "9000000" },
		"little memory":          func(f Facts) { f["mem_available_kb"] = "1000000" },
		"podman":                 func(f Facts) { f["engine_podman"] = "yes" },
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
	p := Evaluate(f, nil)
	if !p.Supported || p.InstallDocker {
		t.Fatalf("plan %+v", p)
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
		if !strings.HasPrefix(l, `restrict,command="`+runhost.ServeCommand+`" `) {
			t.Errorf("a line without the forced command: %s", l)
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
