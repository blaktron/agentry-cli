package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// updateServer is a runhost's CLI and runsc layout under a temporary home,
// running CLI 1.2.3, with an update-signing key the test signs with.
type updateServer struct {
	dir  string
	priv minisign.PrivateKey
}

func newUpdateServer(t *testing.T) *updateServer {
	t.Helper()
	newRunhostServer(t)
	dir := t.TempDir()
	saved := []*string{&runhostBinDir, &runhostCLIDir, &runhostGVisorDir, &runhostAuthorizedKeys, &runhostConfigFile}
	old := make([]string, len(saved))
	for i, p := range saved {
		old[i] = *p
	}
	oldKeys, oldVersion := runhostReleaseKeys, runhostRunningVersion
	t.Cleanup(func() {
		for i, p := range saved {
			*p = old[i]
		}
		runhostReleaseKeys, runhostRunningVersion = oldKeys, oldVersion
		runhostRestart.Lock()
		runhostRestart.to = ""
		runhostRestart.Unlock()
		select {
		case <-runhostRestart.asked:
		default:
		}
	})
	runhostBinDir, runhostCLIDir, runhostGVisorDir = filepath.Join(dir, "bin"), filepath.Join(dir, "cli"), filepath.Join(dir, "gvisor")
	runhostAuthorizedKeys, runhostConfigFile = filepath.Join(dir, "authorized_keys"), filepath.Join(dir, "config.json")
	for _, d := range []string{runhostBinDir, filepath.Join(runhostCLIDir, "1.2.3")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runhostCLIDir, "1.2.3", "agentry"), []byte(fakeVersionCLI("1.2.3")), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	if err := pointAt(filepath.Join(runhostBinDir, "agentry"), filepath.Join(runhostCLIDir, "1.2.3", "agentry")); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clirelease.ParseKeys([]byte(fmt.Sprintf(`{"keys":[{"name":"test","publicKey":%q}]}`, pub.String())))
	if err != nil {
		t.Fatal(err)
	}
	runhostReleaseKeys = func() (*clirelease.KeySet, error) { return keys, nil }
	runhostRunningVersion = func() string { return "1.2.3" }
	return &updateServer{dir: dir, priv: priv}
}

func fakeVersionCLI(v string) string { return "#!/bin/sh\necho agentry " + v + "\n" }

// release is CLI v's binary for arch with sums over every architecture's,
// signed by key.
func (u *updateServer) release(v, arch string, key minisign.PrivateKey) (runhost.UpdateHeader, []byte) {
	var sums bytes.Buffer
	var bin []byte
	for _, a := range []string{"amd64", "arm64"} {
		b := []byte(fakeVersionCLI(v) + "# " + a + "\n")
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), clirelease.BinaryName(v, a))
		if a == arch {
			bin = b
		}
	}
	sig := minisign.SignWithComments(key, sums.Bytes(), "agentry-cli "+v+" (commit abcdef1)", "untrusted comment")
	return runhost.UpdateHeader{Name: clirelease.BinaryName(v, arch), Sums: sums.Bytes(), Sig: sig, Size: int64(len(bin))}, bin
}

func askUpdate(t *testing.T, args []string, h runhost.UpdateHeader, body []byte) runhost.Answer {
	t.Helper()
	line, _ := json.Marshal(h)
	in := append(append(line, '\n'), body...)
	var buf bytes.Buffer
	fw := runhost.NewFrameWriter(&buf)
	code, err := runhostUpdateVerb(context.Background(), runhost.Request{Verb: "update", Args: args}, bytes.NewReader(in), fw)
	_ = fw.Exit(code, err)
	a, rerr := runhost.ReadAnswer(&buf, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return a
}

func (u *updateServer) current(t *testing.T) string {
	t.Helper()
	target, err := filepath.EvalSymlinks(filepath.Join(runhostBinDir, "agentry"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(filepath.Dir(target))
}

func TestARunhostInstallsOnlyASignedNewerReleaseForItsArchitecture(t *testing.T) {
	u := newUpdateServer(t)
	other := func() minisign.PrivateKey {
		_, k, err := minisign.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}()
	wrongArch := "arm64"
	if runtime.GOARCH == "arm64" {
		wrongArch = "amd64"
	}

	h, bin := u.release("1.2.4", runtime.GOARCH, other)
	if a := askUpdate(t, []string{"cli", "1.2.4"}, h, bin); a.Code != engine.ExitVerify {
		t.Errorf("a release signed by an unknown key: code %d %s", a.Code, a.Error)
	}
	h, bin = u.release("1.2.4", runtime.GOARCH, u.priv)
	h.Sig = nil
	if a := askUpdate(t, []string{"cli", "1.2.4"}, h, bin); a.Code != engine.ExitVerify {
		t.Errorf("an unsigned release: code %d %s", a.Code, a.Error)
	}
	h, bin = u.release("1.2.4", wrongArch, u.priv)
	if a := askUpdate(t, []string{"cli", "1.2.4"}, h, bin); a.Code != engine.ExitVerify || !strings.Contains(a.Error, "linux/"+runtime.GOARCH) {
		t.Errorf("another architecture's release: code %d %s", a.Code, a.Error)
	}
	h, bin = u.release("1.2.4", runtime.GOARCH, u.priv)
	h.Name = clirelease.BinaryName("1.2.4", wrongArch)
	if a := askUpdate(t, []string{"cli", "1.2.4"}, h, bin); a.Code != engine.ExitVerify {
		t.Errorf("this architecture's bytes named as another's: code %d %s", a.Code, a.Error)
	}
	h, bin = u.release("1.2.2", runtime.GOARCH, u.priv)
	if a := askUpdate(t, []string{"cli", "1.2.2"}, h, bin); a.Code != engine.ExitUsage || !strings.Contains(a.Error, "older") {
		t.Errorf("an older release: code %d %s", a.Code, a.Error)
	}
	h, bin = u.release("1.2.4", runtime.GOARCH, u.priv)
	if a := askUpdate(t, []string{"cli", "1.2.5"}, h, bin); a.Code == 0 {
		t.Error("1.2.4's release installed as 1.2.5")
	}
	if got := u.current(t); got != "1.2.3" || runhostRestarting() != "" {
		t.Fatalf("a refused update changed the runhost: CLI %s, restarting %q", got, runhostRestarting())
	}

	a := askUpdate(t, []string{"cli", "1.2.3"}, runhost.UpdateHeader{}, nil)
	var up runhost.Updated
	if a.Code != 0 || json.Unmarshal(a.Result, &up) != nil || up.Installed {
		t.Fatalf("the running version: %d %s %s", a.Code, a.Error, a.Result)
	}

	h, bin = u.release("1.2.4", runtime.GOARCH, u.priv)
	a = askUpdate(t, []string{"cli", "1.2.4"}, h, bin)
	if a.Code != 0 || json.Unmarshal(a.Result, &up) != nil || !up.Installed || up.From != "1.2.3" || up.To != "1.2.4" {
		t.Fatalf("a signed newer release: %d %s %s", a.Code, a.Error, a.Result)
	}
	if got := u.current(t); got != "1.2.4" {
		t.Fatalf("the CLI's path points at %s", got)
	}
	if runhostRestarting() != "1.2.4" {
		t.Fatal("the service was not asked to restart into the update")
	}
	// From now on the service takes no job until it has restarted.
	var buf bytes.Buffer
	fw := runhost.NewFrameWriter(&buf)
	if code, err := runhostJobVerb(context.Background(), runhost.Request{Verb: "job"}, strings.NewReader("{}"), fw); code == 0 || err == nil || !strings.Contains(err.Error(), "restarting into CLI 1.2.4") {
		t.Fatalf("a job during the restart: %d %v", code, err)
	}
	sweepOldCLIs()
	if _, err := os.Stat(filepath.Join(runhostCLIDir, "1.2.3")); !os.IsNotExist(err) {
		t.Fatalf("the CLI the runhost left is still there: %v", err)
	}
}

func TestTheServiceRestartsOnlyOnceItsJobsFinish(t *testing.T) {
	newUpdateServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &runhostLive{id: "j1", changed: make(chan struct{})}
	runhostLiveJobs.Lock()
	runhostLiveJobs.m["j1"] = l
	runhostLiveJobs.Unlock()
	defer l.forget()
	stopped := make(chan struct{})
	done := awaitRunhostRestart(ctx, func() { close(stopped) }, func(string, ...any) {})
	askRunhostRestart("1.2.4")
	select {
	case <-stopped:
		t.Fatal("the service stopped with a job running")
	case <-func() <-chan struct{} {
		c := make(chan struct{})
		go func() { defer close(c); <-time.After(3 * time.Second) }()
		return c
	}():
	}
	l.mu.Lock()
	l.done = true
	l.mu.Unlock()
	<-stopped
	if !<-done {
		t.Fatal("the restart did not say it stopped the service")
	}
}

func TestADeviceLeavesWithItsKeysAndTheCreditsItMinted(t *testing.T) {
	newUpdateServer(t)
	fr := newFakeTokenRegistry(t)
	const second = "bbbbbbbbbbbbbbbb"
	devs := runhost.Devices{Devices: []runhost.Device{{ID: testDevice, SealKey: "AAAA"}, {ID: second, SealKey: "BBBB"}}}
	b, _ := json.Marshal(devs)
	if err := os.WriteFile(runhostDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	keys := runhost.AuthorizedKeyLine("ssh-ed25519 KEYA", testDevice) + "\n" + runhost.AuthorizedKeyLine("ssh-ed25519 KEYB", second) + "\n"
	if err := os.WriteFile(runhostAuthorizedKeys, []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := askStore(t, []string{"put-credits"}, creditsDoc(fr.srv.URL, testCreditsID, testCreditsToken)); a.Code != 0 {
		t.Fatalf("put-credits: %d %s", a.Code, a.Error)
	}
	unpair := func(device string) runhost.Answer {
		var buf bytes.Buffer
		fw := runhost.NewFrameWriter(&buf)
		code, err := runhostUnpairVerb(context.Background(), runhost.Request{Verb: "unpair", Device: device}, strings.NewReader(""), fw)
		_ = fw.Exit(code, err)
		a, _ := runhost.ReadAnswer(&buf, nil)
		return a
	}
	if a := unpair(""); a.Code == 0 {
		t.Fatal("a run key that names no device unpaired one")
	}
	if a := unpair("cccccccccccccccc"); a.Code == 0 {
		t.Fatal("a device that is not paired unpaired")
	}

	// The second device minted nothing: the token stays.
	a := unpair(second)
	var out remote.Unpaired
	if a.Code != 0 || json.Unmarshal(a.Result, &out) != nil || out.Device != second || out.Credits.Removed {
		t.Fatalf("unpair %s: %d %s %s", second, a.Code, a.Error, a.Result)
	}
	if got, _ := os.ReadFile(runhostAuthorizedKeys); strings.Contains(string(got), "KEYB") || !strings.Contains(string(got), "KEYA") {
		t.Fatalf("authorized_keys after the second device left:\n%s", got)
	}
	if !runhostDevicePaired(testDevice) || runhostDevicePaired(second) {
		t.Fatal("the devices file kept the wrong device")
	}
	if len(fr.seen()) != 0 {
		t.Fatalf("a token was revoked: %v", fr.seen())
	}
	// The last device cannot leave: removing the runhost does that.
	if a := unpair(testDevice); a.Code == 0 || !strings.Contains(a.Error, "only paired device") {
		t.Fatalf("the last device left: %d %s", a.Code, a.Error)
	}

	// With two again, the device that minted the token takes it with it.
	if err := os.WriteFile(runhostDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a = unpair(testDevice)
	if a.Code != 0 || json.Unmarshal(a.Result, &out) != nil || !out.Credits.Removed || !out.Credits.Revoked {
		t.Fatalf("unpair %s: %d %s %s", testDevice, a.Code, a.Error, a.Result)
	}
	if seen := fr.seen(); len(seen) != 1 || !strings.HasPrefix(seen[0], testCreditsID+" ") {
		t.Fatalf("revoked %v", seen)
	}
}

func TestARunKeyCannotSendAJobOrATokenForAnotherDevice(t *testing.T) {
	newRunhostServer(t)
	j, _ := unsignedJob(t, "0123456789abcdef")
	var doc bytes.Buffer
	if err := job.Write(&doc, j); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	fw := runhost.NewFrameWriter(&buf)
	code, err := runhostJobVerb(context.Background(), runhost.Request{Verb: "job", Device: "bbbbbbbbbbbbbbbb"}, &doc, fw)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "run key sent it") {
		t.Fatalf("a job for another device: %d %v", code, err)
	}
}

func TestThePostUpdateStepUpdatesEachRunhostOncePerVersion(t *testing.T) {
	isolateEnv(t)
	home := filepath.Join(t.TempDir(), "runner")
	t.Setenv(policy.HomeEnv, home)
	old := version.Version
	version.Version = "1.2.4"
	t.Cleanup(func() { version.Version = old })
	recs := &runhost.Records{Runhosts: []runhost.Record{{Name: "lab", CLIVersion: "1.2.3"}, {Name: "attic", CLIVersion: "1.2.4"}}}
	if err := recs.Save(home); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"lab", "attic"} {
		keys, err := runhost.NewDeviceKeys()
		if err != nil {
			t.Fatal(err)
		}
		if err := runhost.SaveKeys(n, keys); err != nil {
			t.Fatal(err)
		}
	}
	var asked []string
	oldUpdate := runhostUpdate
	runhostUpdate = func(_ context.Context, rec *runhost.Record, _ *runhost.DeviceKeys, _ remote.UpdateOptions) (remote.UpdateOutcome, error) {
		asked = append(asked, rec.Name)
		from := rec.CLIVersion
		rec.CLIVersion = "1.2.4"
		return remote.UpdateOutcome{Name: rec.Name, From: from, To: "1.2.4", Updated: true}, nil
	}
	t.Cleanup(func() { runhostUpdate = oldUpdate })

	_, stderr, err := runCLI(t, nil, "runhost", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "lab" || !strings.Contains(stderr, "updating lab") || !strings.Contains(stderr, "runhost lab runs CLI 1.2.4 (from 1.2.3)") {
		t.Fatalf("asked %v, said:\n%s", asked, stderr)
	}
	got, _ := runhost.LoadRecords(home)
	if r, _ := got.Find("lab"); r.CLIVersion != "1.2.4" {
		t.Fatalf("the record was not saved: %+v", r)
	}
	asked = nil
	if _, _, err := runCLI(t, nil, "runhost", "list"); err != nil || len(asked) != 0 {
		t.Fatalf("the step ran again for the same version: %v %v", asked, err)
	}
}
