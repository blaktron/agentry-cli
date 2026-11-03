package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

// The live messenger test. Everything is real: the runner image built here,
// the account's internal network, the relay and the role under the
// policy's runtime (AGENTRY_LIVE_RUNTIME, e.g. runsc), and Telegram's Bot
// API reached through the relay alone. With no token it starts the role with
// one Telegram does not know and requires Telegram's own refusal, which
// only a request that crossed the relay and its TLS gets back. With
// AGENTRY_LIVE_TELEGRAM_TOKEN set to a bot's token it requires the bot's
// ready line, then holds the connection for AGENTRY_LIVE_MESSENGER_HOLD (a
// duration, default a minute) and requires it still connected. It runs
// only when asked: AGENTRY_LIVE_MESSENGER=1 on a host with docker and
// network.
func TestLiveMessengerRoleReachesTelegramThroughItsRelay(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_MESSENGER") != "1" {
		t.Skip("set AGENTRY_LIVE_MESSENGER=1 to run the live messenger test")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	work := t.TempDir()
	pol := policy.Default()
	pol.Docker.Runtime = os.Getenv("AGENTRY_LIVE_RUNTIME")
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureRunnerImage(ctx, d, pol, runnerTag, filepath.Join(work, "img"), os.Stderr); err != nil {
		t.Fatal(err)
	}
	k, _ := messenger.Lookup(messenger.KindTelegram)
	token := os.Getenv("AGENTRY_LIVE_TELEGRAM_TOKEN")
	p := MessengerParams{Kind: k, Image: runnerTag, Policy: pol, Dir: filepath.Join(work, "msg"), Log: os.Stderr}
	if token == "" {
		p.Token = "123456789:AAthisIsNotATokenTelegramKnows_abcdefg"
		_, err := StartMessenger(ctx, d, p)
		if err == nil || !strings.Contains(err.Error(), messenger.TokenRefused) {
			t.Fatalf("StartMessenger with a token Telegram does not know = %v, want Telegram's refusal", err)
		}
		if strings.Contains(err.Error(), p.Token) {
			t.Fatal("the error repeats the token")
		}
		t.Logf("Telegram refused the token through the relay: %v", err)
		return
	}
	p.Token = token
	l, err := StartMessenger(ctx, d, p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.WithoutCancel(ctx))
	t.Logf("connected as %s (%s)", l.Ready().Name, l.Ready().Account)
	hold := time.Minute
	if v := os.Getenv("AGENTRY_LIVE_MESSENGER_HOLD"); v != "" {
		if hold, err = time.ParseDuration(v); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(hold)
	for {
		select {
		case f, ok := <-l.Frames():
			if !ok {
				t.Fatal("the role ended")
			}
			if f.State != nil && !f.State.Connected {
				t.Logf("the connection dropped: %s", f.State.Error)
			}
			if f.Inbound != nil || f.Pairing != nil || f.Dropped != nil {
				t.Logf("a frame: %+v", f)
			}
		case <-deadline:
			return
		}
	}
}
