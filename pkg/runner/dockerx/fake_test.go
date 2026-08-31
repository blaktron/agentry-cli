// Fake recorder tests: the engine's entire lifecycle proof (DR-7) leans
// on this double, so its recording formats, scripted failures, and the
// Block/ctx foreground behavior are contracts of their own and pinned
// here — a Fake that drifts would green-light a broken engine.
package dockerx

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFakeRecordsOpsAndFindRun(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	_ = f.Available(ctx)
	_, _ = f.ImageExists(ctx, "img:1")
	_ = f.Pull(ctx, "img:1")
	_ = f.Build(ctx, "/ctx", "tag:1", nil)
	_ = f.NetworkCreate(ctx, "net", true, nil)
	_ = f.NetworkConnect(ctx, "bridge", "ctr")
	_ = f.RunDetached(ctx, RunSpec{Name: "agentry-x-gateway", Image: "g"})
	_, _ = f.RunForeground(ctx, RunSpec{Name: "agentry-x-agent", Image: "a"}, nil, io.Discard, io.Discard)
	_ = f.Stop(ctx, "agentry-x-gateway", 10)
	_ = f.RemoveForce(ctx, "agentry-x-gateway")
	_ = f.NetworkRemove(ctx, "net")
	_, _ = f.PSByLabel(ctx, "agentry.run=x")
	_, _ = f.Logs(ctx, "agentry-x-gateway", 40)

	want := []string{
		"Available:",
		"ImageExists:img:1",
		"Pull:img:1",
		"Build:tag:1<-/ctx",
		"NetworkCreate:net(internal=true)",
		"NetworkConnect:bridge<-ctr",
		"RunDetached:agentry-x-gateway",
		"RunForeground:agentry-x-agent",
		"Stop:agentry-x-gateway",
		"RemoveForce:agentry-x-gateway",
		"NetworkRemove:net",
		"PSByLabel:agentry.run=x",
		"Logs:agentry-x-gateway",
	}
	if got := f.Ops(); !reflect.DeepEqual(got, want) {
		t.Errorf("Ops() =\n%q\nwant\n%q", got, want)
	}

	// FindRun matches Run* calls by name substring, first hit wins.
	if run, ok := f.FindRun("gateway"); !ok || run.Image != "g" {
		t.Errorf("FindRun(gateway) = (%+v, %v)", run, ok)
	}
	if run, ok := f.FindRun("-agent"); !ok || run.Image != "a" {
		t.Errorf("FindRun(-agent) = (%+v, %v)", run, ok)
	}
	if _, ok := f.FindRun("no-such-container"); ok {
		t.Error("FindRun matched a name never run")
	}
}

func TestFakeScriptedFailures(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{
		AvailableErr:   boom,
		ExistingImages: map[string]bool{"have:1": true},
		FailPull:       map[string]error{"img:1": boom},
		FailBuild:      map[string]error{"tag:1": boom},
		FailDetached:   map[string]error{"-gateway": boom},
	}
	ctx := context.Background()
	if err := f.Available(ctx); !errors.Is(err, boom) {
		t.Errorf("Available = %v", err)
	}
	if ok, _ := f.ImageExists(ctx, "have:1"); !ok {
		t.Error("scripted image not reported present")
	}
	if ok, _ := f.ImageExists(ctx, "missing:1"); ok {
		t.Error("unscripted image reported present")
	}
	if err := f.Pull(ctx, "img:1"); !errors.Is(err, boom) {
		t.Errorf("Pull = %v", err)
	}
	if err := f.Pull(ctx, "other:1"); err != nil {
		t.Errorf("unscripted Pull = %v", err)
	}
	if err := f.Build(ctx, "/ctx", "tag:1", nil); !errors.Is(err, boom) {
		t.Errorf("Build = %v", err)
	}
	// Detached failures key on a name SUBSTRING, so one entry covers every
	// run id.
	if err := f.RunDetached(ctx, RunSpec{Name: "agentry-cafe0000-gateway"}); !errors.Is(err, boom) {
		t.Errorf("RunDetached = %v", err)
	}
	if err := f.RunDetached(ctx, RunSpec{Name: "agentry-cafe0000-proxy"}); err != nil {
		t.Errorf("unscripted RunDetached = %v", err)
	}
}

func TestFakeForeground(t *testing.T) {
	ctx := context.Background()

	t.Run("unscripted container exits clean", func(t *testing.T) {
		f := &Fake{}
		code, err := f.RunForeground(ctx, RunSpec{Name: "x"}, nil, io.Discard, io.Discard)
		if code != 0 || err != nil {
			t.Fatalf("(%d, %v)", code, err)
		}
	})
	t.Run("scripted stdio, exit code, and error", func(t *testing.T) {
		boom := errors.New("docker fell over")
		fg := &FakeForeground{Stdout: "agent says hi", ExitCode: 3, Err: boom}
		f := &Fake{Foreground: map[string]*FakeForeground{"-agent": fg}}
		var out strings.Builder
		code, err := f.RunForeground(ctx, RunSpec{Name: "agentry-x-agent"},
			strings.NewReader("the task"), &out, io.Discard)
		if code != 3 || !errors.Is(err, boom) {
			t.Fatalf("(%d, %v)", code, err)
		}
		if out.String() != "agent says hi" {
			t.Errorf("stdout = %q", out.String())
		}
		if fg.GotStdin != "the task" {
			t.Errorf("GotStdin = %q", fg.GotStdin)
		}
	})
	t.Run("Block waits for ctx and reports the kill", func(t *testing.T) {
		f := &Fake{Foreground: map[string]*FakeForeground{"-agent": {Block: true}}}
		cctx, cancel := context.WithCancel(ctx)
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		code, err := f.RunForeground(cctx, RunSpec{Name: "agentry-x-agent"}, nil, io.Discard, io.Discard)
		if code != 137 || err != nil {
			t.Fatalf("(%d, %v), want the SIGKILL code", code, err)
		}
	})
}

func TestFakeLeftoverAndLogs(t *testing.T) {
	f := &Fake{
		Leftover:   []string{"agentry-x-zombie"},
		LogsByName: map[string]string{"agentry-x-gateway": "gateway log line\n"},
	}
	ctx := context.Background()
	names, err := f.PSByLabel(ctx, "agentry.run=x")
	if err != nil || !reflect.DeepEqual(names, []string{"agentry-x-zombie"}) {
		t.Fatalf("PSByLabel = (%q, %v)", names, err)
	}
	logs, err := f.Logs(ctx, "agentry-x-gateway", 40)
	if err != nil || logs != "gateway log line\n" {
		t.Fatalf("Logs = (%q, %v)", logs, err)
	}
	if logs, _ := f.Logs(ctx, "unknown", 40); logs != "" {
		t.Errorf("unscripted Logs = %q", logs)
	}
}
