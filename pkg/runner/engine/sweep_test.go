package engine

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// deadOwner is the owner labels of a process that has exited: a child
// started, its start time read, then reaped.
func deadOwner(t *testing.T) map[string]string {
	t.Helper()
	self := owner.Labels()
	if self == nil {
		t.Skip("no owner identity on this platform: nothing is labelled, nothing is swept")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start, ok := owner.StartTime(pid)
	_ = cmd.Wait()
	if !ok {
		t.Skip("the child exited before its start time was read")
	}
	return map[string]string{owner.LabelHost: self[owner.LabelHost], owner.LabelPID: strconv.Itoa(pid), owner.LabelStart: strconv.FormatUint(start, 10)}
}

func withRun(l map[string]string, run string) map[string]string {
	m := map[string]string{"agentry.run": run}
	for k, v := range l {
		m[k] = v
	}
	return m
}

// TestSweepRemovesOnlyWhatADeadRunLeft: a crashed run's containers, its
// network and its session volume go; a live sibling run's containers, a
// container from another host or namespace, and one with no owner labels
// (created before they existed) all stay (agentry-cli#161).
func TestSweepRemovesOnlyWhatADeadRunLeft(t *testing.T) {
	dead := deadOwner(t)
	live := owner.Labels()
	f := &dockerx.Fake{Containers: []dockerx.Labeled{
		{Name: "agentry-dead0001-gateway", Labels: withRun(dead, "dead0001")},
		{Name: "agentry-dead0001-agent", Labels: withRun(dead, "dead0001")},
		{Name: "agentry-mcpinstall-srv", Labels: dead}, // no run: nothing else to remove
		{Name: "agentry-live0002-gateway", Labels: withRun(live, "live0002")},
		{Name: "agentry-else0003-gateway", Labels: withRun(map[string]string{owner.LabelHost: "another.host.0", owner.LabelPID: "1", owner.LabelStart: "1"}, "else0003")},
		{Name: "agentry-old00004-gateway", Labels: map[string]string{"agentry.run": "old00004"}},
	}}
	var stderr strings.Builder
	sweepAbandoned(context.Background(), f, &stderr)

	ops := f.Ops()
	for _, want := range []string{
		"RemoveForce:agentry-dead0001-gateway", "RemoveForce:agentry-dead0001-agent", "RemoveForce:agentry-mcpinstall-srv",
		"NetworkRemove:agentry-dead0001-net", "VolumeRemove:agentry-dead0001-session",
	} {
		if !slices.Contains(ops, want) {
			t.Errorf("missing %s in %v", want, ops)
		}
	}
	for _, op := range ops {
		for _, kept := range []string{"live0002", "else0003", "old00004"} {
			if strings.Contains(op, kept) {
				t.Errorf("the sweep touched %s: %s", kept, op)
			}
		}
	}
	// The containers go before the network and the volume they use.
	if slices.Index(ops, "NetworkRemove:agentry-dead0001-net") < slices.Index(ops, "RemoveForce:agentry-dead0001-agent") {
		t.Errorf("network removed before its containers: %v", ops)
	}
	if !strings.Contains(stderr.String(), "removed 3 container(s)") {
		t.Errorf("stderr = %q, want the sweep to say what it removed", stderr.String())
	}
}

// TestRunSweepsBeforeItBuilds: a run removes a dead run's leftovers before
// it creates its own network, and its own containers carry the owner labels
// the next run's sweep reads.
func TestRunSweepsBeforeItBuilds(t *testing.T) {
	dead := deadOwner(t)
	p, f := fixture(t)
	happyFake(f)
	f.Containers = []dockerx.Labeled{{Name: "agentry-dead0001-agent", Labels: withRun(dead, "dead0001")}}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ops := f.Ops()
	swept, built := slices.Index(ops, "RemoveForce:agentry-dead0001-agent"), slices.Index(ops, "NetworkCreate:agentry-cafe0000-net(internal=true)")
	if swept < 0 || built < 0 || swept > built {
		t.Errorf("sweep at %d, network at %d, want the sweep first: %v", swept, built, ops)
	}
	gw, ok := f.FindRun("-gateway")
	if !ok {
		t.Fatal("no gateway run")
	}
	if gw.Labels[owner.LabelPID] != strconv.Itoa(os.Getpid()) || gw.Labels[owner.LabelHost] == "" || gw.Labels["agentry.run"] != "cafe0000" {
		t.Errorf("gateway labels = %v, want the run id and this process as owner", gw.Labels)
	}
}

// TestStartSessionSweepsBeforeItBuilds: an interactive session sweeps as a
// run does, before its own network.
func TestStartSessionSweepsBeforeItBuilds(t *testing.T) {
	dead := deadOwner(t)
	p, f := sessionFixture(t)
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	f.Containers = []dockerx.Labeled{{Name: "agentry-dead0001-agent", Labels: withRun(dead, "dead0001")}}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, done := collect(s)
	if _, err := s.End(context.Background(), false); err != nil {
		t.Logf("end: %v", err)
	}
	<-done
	ops := f.Ops()
	swept := slices.Index(ops, "RemoveForce:agentry-dead0001-agent")
	built := -1
	for i, op := range ops {
		if strings.HasPrefix(op, "NetworkCreate:") {
			built = i
			break
		}
	}
	if swept < 0 || built < 0 || swept > built {
		t.Errorf("sweep at %d, network at %d, want the sweep first: %v", swept, built, ops)
	}
}
