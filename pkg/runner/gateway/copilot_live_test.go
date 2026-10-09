package gateway

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestLiveCopilotImage builds the Copilot CLI image this build pins, from
// GitHub's real release (plan model-sign-ins.md D27; agentry-cli#741), and
// drives the real Copilot CLI in it with the runner's own driver, in the
// container the plan compiles, hardened as the engine runs an agent (read
// only root, every capability dropped, no new privileges, the home a
// noexec tmpfs) and on no network. With no network and no account, the
// session cannot be created: the test holds that Copilot CLI answered
// connect, asked the driver for the token through its callback, and then
// failed to reach GitHub to check it, which only a token handed over can
// lead to. It downloads about 120 MB, so it runs only when asked:
// AGENTRY_LIVE_COPILOT=1 on a host with docker.
func TestLiveCopilotImage(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_COPILOT") != "1" {
		t.Skip("builds the real Copilot CLI image: set AGENTRY_LIVE_COPILOT=1")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	h, ok := policy.Default().Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	if !ok {
		t.Fatal("no default copilot-image")
	}
	var log strings.Builder
	if err := EnsureHarnessImage(ctx, d, adapter.CopilotImageAdapterName, h, &log); err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	t.Logf("%s", log.String())
	if got, err := d.ImageLabel(ctx, h.Ref, harnessbuild.LabelArtifact); err != nil || got != h.Artifact {
		t.Fatalf("label %q %v", got, err)
	}

	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	res, err := ad.Compile(adapter.Input{
		Mode: manifest.ModeAutonomous, Instruction: "You are the live check.",
		Model:        policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindCopilotSignIn)},
		HarnessImage: h.Ref, GatewayURL: spec.GatewayURL(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Agent
	args := []string{"run", "-i", "--rm", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "256", "--memory", "1g", "--entrypoint", a.Entrypoint}
	if a.ReadOnlyRoot {
		args = append(args, "--read-only")
	}
	for _, tm := range a.Tmpfs {
		args = append(args, "--tmpfs", tm)
	}
	for _, e := range a.Env {
		args = append(args, "-e", e.Name+"="+e.Value)
	}
	args = append(args, a.Image)
	args = append(args, a.Argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	asked := make(chan bool, 4)
	drv, err := ad.Session(ctx, rwPair{stdout, stdin}, adapter.SessionOptions{
		ModelSignIn: true, GatewayToken: "live-check-bearer",
		HarnessFile: func(name string) ([]byte, error) { return res.Files[name], nil },
		SignInToken: func(_ context.Context, refresh bool) (adapter.SignInToken, error) {
			asked <- refresh
			return adapter.SignInToken{AccessToken: "ghu_livecheck000000000000000000", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for ev := range drv.Events() {
			t.Logf("event %s %s", ev.Kind, ev.Text)
		}
	}()
	hs, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = drv.Start(hs)
	_ = drv.Close()
	_ = cmd.Wait()
	t.Logf("start: %v\nstderr: %s", err, stderr.String())
	if err == nil || !strings.Contains(err.Error(), "GitHub token validation failed") || !strings.Contains(err.Error(), "network") {
		t.Fatalf("want session.create to fail checking the handed token on no network, got %v", err)
	}
	select {
	case refresh := <-asked:
		if refresh {
			t.Fatal("the first ask was a refresh")
		}
	default:
		t.Fatal("Copilot CLI never asked the driver for its token")
	}
}

type rwPair struct {
	r interface{ Read([]byte) (int, error) }
	w interface {
		Write([]byte) (int, error)
		Close() error
	}
}

func (p rwPair) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p rwPair) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p rwPair) Close() error                { return p.w.Close() }
