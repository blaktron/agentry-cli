package engine

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// holdRunLockEnv makes this test binary a run that takes its directory's
// lock, writes the files a run's teardown must delete, says "ready" and
// waits to be killed (TestMain).
const holdRunLockEnv = "AGENTRY_TEST_HOLD_RUN_LOCK"

// playLockedRun is the process holdRunLockEnv asks for: the run directory
// is the variable's value, its run id the directory's name, and a second
// variable says whether the run keeps its directory.
func playLockedRun(root string) int {
	if _, err := holdRunLock(root, filepath.Base(root), os.Getenv(holdRunLockEnv+"_KEEP") != ""); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return 2
	}
	writeRunSecrets(root)
	_, _ = os.Stdout.WriteString("ready\n")
	time.Sleep(time.Hour)
	return 1
}

// writeRunSecrets writes what a live run holds in its directory: the
// secrets file, a connection's token, a model sign-in's token, and a
// workspace file that only an unkept run's teardown removes.
func writeRunSecrets(root string) {
	rd := runDirsAt(root)
	for path, body := range map[string]string{
		rd.secretsPath: "ENGINE_TEST_TOKEN=sekret-value-1234\n",
		filepath.Join(rd.connections(), "a-server", "me@agentry.invalid.json"): `{"access_token":"at-1234"}`,
		filepath.Join(rd.modelSignInDir(), "token"):                            "signin-1234",
		filepath.Join(rd.knowledge, "notes", "pages", "topic.md"):              "# A page of the attached pack, in the clear\n",
		filepath.Join(rd.workspace, "SKILL.md"):                                "Do the test.\n",
	} {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte(body), 0o600)
	}
}

// startLockedRun starts a run as a separate process that holds its run
// directory's lock and has written its secrets, and returns it once it
// says so.
func startLockedRun(t *testing.T, root string, keep bool) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), holdRunLockEnv+"="+root)
	if keep {
		cmd.Env = append(cmd.Env, holdRunLockEnv+"_KEEP=1")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("the run process did not start: %q, stderr %q", line, stderr.String())
	}
	return cmd
}

// killRun ends a run the way no teardown sees, and waits until it is gone.
func killRun(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

func pathExists(path string) bool { _, err := os.Lstat(path); return err == nil }

// TestSweepRemovesTheSecretsOfAKilledRunAndNeverALiveOnes: a run killed
// after its secrets are written leaves its run directory; while it is
// still alive the sweep leaves every file, and once it is killed the next
// sweep removes the secrets file, the connections' and the sign-in's
// tokens, and, the run not kept, the whole directory.
func TestSweepRemovesTheSecretsOfAKilledRunAndNeverALiveOnes(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "runs", "dead0001")
	cmd := startLockedRun(t, root, false)
	rd := runDirsAt(root)

	var out bytes.Buffer
	sweepRunDirs(home, home, nil, &out)
	if !pathExists(rd.secretsPath) || !pathExists(rd.connections()) || !pathExists(rd.modelSignInDir()) || !pathExists(rd.knowledge) {
		t.Fatalf("the sweep removed a live run's files: %s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("the sweep reported something for a live run: %q", out.String())
	}

	killRun(t, cmd)
	sweepRunDirs(home, home, nil, &out)
	if pathExists(root) {
		t.Errorf("the killed run's directory is still there")
	}
	if pathExists(rd.knowledge) {
		t.Errorf("the killed run's copy of an attached pack is still there")
	}
	if !strings.Contains(out.String(), "dead0001") {
		t.Errorf("the sweep did not say what it removed: %q", out.String())
	}
}

// TestSweepKeepsAKilledKeptRunsDirectoryButNotItsSecrets: a run killed
// with --keep-run-dir keeps its directory for debugging, as its teardown
// would have, and loses its secrets, as its teardown would have. Its lock
// goes too, so no later sweep takes it for a run again.
func TestSweepKeepsAKilledKeptRunsDirectoryButNotItsSecrets(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "runs", "dead0002")
	killRun(t, startLockedRun(t, root, true))
	rd := runDirsAt(root)

	sweepRunDirs(home, home, nil, nil)
	for _, gone := range []string{rd.secretsPath, rd.connections(), rd.modelSignInDir(), rd.knowledge, filepath.Join(root, RunLockFile)} {
		if pathExists(gone) {
			t.Errorf("%s survived the sweep of a killed kept run", gone)
		}
	}
	if !pathExists(filepath.Join(rd.workspace, "SKILL.md")) {
		t.Error("the sweep removed a kept run's workspace")
	}
}

// TestSweepLeavesARunDirectoryItCannotJudge: a lock file with no record
// (a run that died, or is starting, between creating it and writing it:
// nothing sensitive is written before the record), and a directory with no
// lock file that the container sweep does not name dead, are left alone.
// A directory without a lock that the container sweep does name dead loses
// its sensitive files only, since whether it was kept is not known.
func TestSweepLeavesARunDirectoryItCannotJudge(t *testing.T) {
	home := t.TempDir()
	starting := filepath.Join(home, "runs", "abcd0001")
	unknown := filepath.Join(home, "runs", "abcd0002")
	noLockDead := filepath.Join(home, "runs", "abcd0003")
	for _, root := range []string{starting, unknown, noLockDead} {
		writeRunSecrets(root)
	}
	if err := os.WriteFile(filepath.Join(starting, RunLockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sweepRunDirs(home, home, map[string]bool{"abcd0003": true}, nil)
	for _, root := range []string{starting, unknown} {
		if !pathExists(runDirsAt(root).secretsPath) {
			t.Errorf("%s: the sweep removed a run's secrets it could not prove dead", root)
		}
	}
	if pathExists(runDirsAt(noLockDead).secretsPath) || pathExists(runDirsAt(noLockDead).connections()) {
		t.Error("a run the container sweep proved dead kept its secrets")
	}
	if !pathExists(filepath.Join(runDirsAt(noLockDead).workspace, "SKILL.md")) {
		t.Error("a run directory whose keep is unknown lost more than its secrets")
	}
}

// TestRunSweepsAKilledRunsFilesAndLeavesNoLock: Run starts by sweeping the
// install's run directories, so a killed run's secrets go with the next
// run of the same install; and a run that ends leaves no lock behind,
// kept directory or not.
func TestRunSweepsAKilledRunsFilesAndLeavesNoLock(t *testing.T) {
	for _, keep := range []bool{false, true} {
		p, f := fixture(t)
		happyFake(f)
		p.KeepRunDir = keep
		dead := filepath.Join(p.RunRoot, "runs", "dead0003")
		killRun(t, startLockedRun(t, dead, false))
		if _, err := Run(context.Background(), f, p); err != nil {
			t.Fatalf("keep=%v: Run: %v", keep, err)
		}
		if pathExists(dead) {
			t.Errorf("keep=%v: the killed run's directory survived the next run", keep)
		}
		own := filepath.Join(p.RunRoot, "runs", "cafe0000")
		if pathExists(filepath.Join(own, RunLockFile)) {
			t.Errorf("keep=%v: the run left its lock behind", keep)
		}
		if pathExists(own) != keep {
			t.Errorf("keep=%v: the run's directory exists = %v", keep, pathExists(own))
		}
	}
}

// TestSweepLeavesALiveRunOfThisProcessAlone: a run that this same process
// holds (two runs of one desktop, one MCP server) is as live to the sweep
// as another process's, and a second lock on its directory is refused.
func TestSweepLeavesALiveRunOfThisProcessAlone(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "runs", "live0001")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := holdRunLock(root, "live0001", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release(false)
	writeRunSecrets(root)
	sweepRunDirs(home, home, map[string]bool{"live0001": true}, nil)
	if !pathExists(runDirsAt(root).secretsPath) {
		t.Fatal("the sweep removed the secrets of a run this process holds")
	}
	if err := os.Remove(filepath.Join(root, RunLockFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, RunLockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := holdRunLock(root, "live0001", false); err == nil {
		t.Error("a run directory whose lock file exists was locked again")
	}
}

// TestSweepRetriesAKeptRunsSecretItCouldNotDelete: a kept run's secret the
// sweep cannot delete leaves the lock and its record in place, so the next
// sweep tries again, and the one after that finds nothing to do.
func TestSweepRetriesAKeptRunsSecretItCouldNotDelete(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("a read-only directory keeps its files only off Windows, and not from root")
	}
	home := t.TempDir()
	root := filepath.Join(home, "runs", "dead0004")
	killRun(t, startLockedRun(t, root, true))
	gw := filepath.Dir(runDirsAt(root).secretsPath)
	if err := os.Chmod(gw, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gw, 0o755) })
	var out bytes.Buffer
	sweepRunDirs(home, home, nil, &out)
	if !strings.Contains(out.String(), "secrets.env") || !pathExists(filepath.Join(root, RunLockFile)) {
		t.Fatalf("a secret the sweep could not delete was not named, or its lock went: %q", out.String())
	}
	if err := os.Chmod(gw, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepRunDirs(home, home, nil, nil)
	if pathExists(runDirsAt(root).secretsPath) || pathExists(filepath.Join(root, RunLockFile)) {
		t.Error("the next sweep did not finish the kept run's secrets and lock")
	}
}

// TestStartSessionSweepsAKilledRunsFiles: an interactive session sweeps the
// install's run directories as a run does.
func TestStartSessionSweepsAKilledRunsFiles(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	dead := filepath.Join(p.RunRoot, "runs", "dead0005")
	killRun(t, startLockedRun(t, dead, false))
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, done := collect(s)
	if _, err := s.End(context.Background(), false); err != nil {
		t.Logf("end: %v", err)
	}
	<-done
	if pathExists(dead) {
		t.Error("the killed run's directory survived the session's start")
	}
}

// TestRunsInUseNamesTheRunsALiveProcessHolds: a run another process holds
// and one this process holds are in use; a killed run's directory and a
// directory with no lock are not.
func TestRunsInUseNamesTheRunsALiveProcessHolds(t *testing.T) {
	home := t.TempDir()
	startLockedRun(t, filepath.Join(home, "runs", "aaaa0001"), false)
	killRun(t, startLockedRun(t, filepath.Join(home, "runs", "aaaa0002"), false))
	own := filepath.Join(home, "runs", "aaaa0003")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := holdRunLock(own, "aaaa0003", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release(false)
	writeRunSecrets(own)
	writeRunSecrets(filepath.Join(home, "runs", "aaaa0004"))
	got := RunsInUse(home)
	if strings.Join(got, ",") != "aaaa0001,aaaa0003" {
		t.Errorf("RunsInUse = %v, want the two live runs", got)
	}
	// Asking takes nothing from the run that holds it.
	sweepRunDirs(home, home, nil, nil)
	if !pathExists(runDirsAt(own).secretsPath) {
		t.Error("asking which runs are in use released one")
	}
}

// TestSweepRemovesTheNetworksOfARunKilledBeforeItsContainers: a run killed
// after it made its networks but before any container existed leaves
// nothing the container sweep can find; its free lock proves it dead, so
// the next run's sweep removes its networks, the labelled one among them,
// and its volumes. A live run's networks stay.
func TestSweepRemovesTheNetworksOfARunKilledBeforeItsContainers(t *testing.T) {
	home := t.TempDir()
	killRun(t, startLockedRun(t, filepath.Join(home, "runs", "dead0006"), false))
	startLockedRun(t, filepath.Join(home, "runs", "live0006"), false)
	labels := func(r string) map[string]string { return map[string]string{runLabel: r} }
	f := &dockerx.Fake{Networks: []dockerx.Labeled{
		{Name: networkName("dead0006"), Labels: labels("dead0006")},
		{Name: sourceNetworkName("dead0006", "repo"), Labels: labels("dead0006")},
		{Name: networkName("live0006"), Labels: labels("live0006")},
	}}
	sweepAbandonedRun(context.Background(), f, home, home, nil)

	ops := f.Ops()
	for _, want := range []string{"NetworkRemove:" + networkName("dead0006"), "NetworkRemove:" + sourceNetworkName("dead0006", "repo"), "VolumeRemove:" + sessionVolumeName("dead0006")} {
		if !slices.Contains(ops, want) {
			t.Errorf("missing %s in %v", want, ops)
		}
	}
	for _, op := range ops {
		if strings.Contains(op, "live0006") {
			t.Errorf("the sweep touched a live run: %s", op)
		}
	}
	if got := f.NetworkState(); len(got) != 1 || got[0].Name != networkName("live0006") {
		t.Errorf("networks left = %v, want the live run's only", got)
	}
}

// TestSweepRemovesTheContainersOfARunItsLockProvesDead: where no owner
// labels judge a container (off Linux), a run killed with its containers
// standing leaves them holding its networks. Its free lock proves it dead,
// so the next run's sweep removes the containers carrying its run label,
// the gateway's helpers among them, and only then its networks. A live
// run's containers stay.
func TestSweepRemovesTheContainersOfARunItsLockProvesDead(t *testing.T) {
	home := t.TempDir()
	killRun(t, startLockedRun(t, filepath.Join(home, "runs", "dead0007"), false))
	startLockedRun(t, filepath.Join(home, "runs", "live0007"), false)
	labels := func(r string) map[string]string { return map[string]string{runLabel: r} }
	f := &dockerx.Fake{
		Networks: []dockerx.Labeled{
			{Name: networkName("dead0007"), Labels: labels("dead0007")},
			{Name: networkName("live0007"), Labels: labels("live0007")},
		},
		ByLabel: map[string][]string{
			runLabel + "=dead0007": {containerName("dead0007", "gateway"), "docker-mcp-l4proxy-dead"},
			runLabel + "=live0007": {containerName("live0007", "gateway")},
		},
	}
	var out strings.Builder
	sweepAbandonedRun(context.Background(), f, home, home, &out)

	ops := f.Ops()
	gateway := slices.Index(ops, "RemoveForce:"+containerName("dead0007", "gateway"))
	helper := slices.Index(ops, "RemoveForce:docker-mcp-l4proxy-dead")
	network := slices.Index(ops, "NetworkRemove:"+networkName("dead0007"))
	if gateway < 0 || helper < 0 || network < 0 || network < gateway || network < helper {
		t.Fatalf("want the dead run's containers removed, then its network: %v", ops)
	}
	for _, op := range ops {
		if strings.Contains(op, "live0007") && !strings.HasPrefix(op, "PSByLabel:") && !strings.HasPrefix(op, "NetworksByLabel:") {
			t.Errorf("the sweep touched a live run: %s", op)
		}
	}
	if !strings.Contains(out.String(), "removed 2 container(s) of 1 run(s) whose process had died: dead0007") {
		t.Errorf("the sweep said %q", out.String())
	}
}

// TestSweepReachesTheRunsOfEveryInstallInTheRunnerHome: a run that died
// under an earlier install of the release (a new built-in version installs
// beside the old one) is swept by a start from the new install: its files,
// its containers, then its networks. A live run under any other install
// keeps everything.
func TestSweepReachesTheRunsOfEveryInstallInTheRunnerHome(t *testing.T) {
	home := t.TempDir()
	install := func(name, ver string) string { return filepath.Join(home, "agents", "agentry", name, ver) }
	oldInstall, newInstall := install("assistant", "1.6.0-aaaaaaaaaaaa"), install("assistant", "1.7.0-bbbbbbbbbbbb")
	dead := filepath.Join(oldInstall, "runs", "dead0008")
	killRun(t, startLockedRun(t, dead, false))
	live := filepath.Join(install("coder", "1.3.0-cccccccccccc"), "runs", "live0008")
	startLockedRun(t, live, false)
	if err := os.MkdirAll(filepath.Join(newInstall, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	labels := func(r string) map[string]string { return map[string]string{runLabel: r} }
	f := &dockerx.Fake{
		Networks: []dockerx.Labeled{
			{Name: networkName("dead0008"), Labels: labels("dead0008")},
			{Name: networkName("live0008"), Labels: labels("live0008")},
		},
		ByLabel: map[string][]string{
			runLabel + "=dead0008": {containerName("dead0008", "gateway")},
			runLabel + "=live0008": {containerName("live0008", "gateway")},
		},
	}
	sweepAbandonedRun(context.Background(), f, newInstall, home, nil)

	ops := f.Ops()
	gateway := slices.Index(ops, "RemoveForce:"+containerName("dead0008", "gateway"))
	network := slices.Index(ops, "NetworkRemove:"+networkName("dead0008"))
	if gateway < 0 || network < gateway {
		t.Fatalf("want the earlier install's dead run's container removed, then its network: %v", ops)
	}
	if pathExists(dead) {
		t.Error("the earlier install's dead run directory survived")
	}
	if !pathExists(live) {
		t.Error("another install's live run directory was removed")
	}
	for _, op := range ops {
		if strings.Contains(op, "live0008") && !strings.HasPrefix(op, "PSByLabel:") && !strings.HasPrefix(op, "NetworksByLabel:") {
			t.Errorf("the sweep touched a live run: %s", op)
		}
	}
}
