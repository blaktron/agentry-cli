package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The person's computer. A release that declares capabilities.desktop uses
// applications on the computer the run starts from, through Agentry Desktop.
// A run from the CLI binds to Agentry Desktop over its link socket in the
// runner home before the sandbox starts; with no Agentry Desktop answering,
// the run is refused there (exit 11, naming the application and Agentry
// Desktop), as a run whose target host was not readied is. A hosted run is
// refused before anything is installed.

// refuseHostedDesktop refuses, before anything is installed, a hosted run of
// a release that declares the person's computer: exit 11, desktop-refused,
// hosted.
func refuseHostedDesktop(plan *spec.Plan, opts runOpts) error {
	d := plan.Capabilities.Desktop
	if d == nil || !hostedFor(opts) {
		return nil
	}
	return engine.HostedDesktopRefusal(d.EntryIDs())
}

// bindDesktop binds the run to Agentry Desktop over the link socket, for a
// plan that declares the person's computer: nil for one that does not. No
// socket, or nothing answering on it, is the refusal that names Agentry
// Desktop; a refusal from the link service carries its reason.
func bindDesktop(ctx context.Context, plan *spec.Plan, home, runID, outDir string, opts runOpts) (*engine.DesktopLink, error) {
	d := plan.Capabilities.Desktop
	if d == nil {
		return nil, nil
	}
	if opts.runhostDesktop != "" {
		return relayedDesktop(opts)
	}
	return bindDesktopCap(ctx, d, plan.Ref, plan.Digest, home, runID, outDir)
}

// relayedDesktop is a runhost's run's binding: the device bound the run to
// its own Agentry Desktop before it sent the job, and the runhost's service
// relays the calls to it.
func relayedDesktop(opts runOpts) (*engine.DesktopLink, error) {
	if !opts.installed {
		return nil, engine.Classify(engine.ExitUsage, errors.New("--runhost-desktop is a remote runhost's own, with --installed"))
	}
	b, err := os.ReadFile(opts.runhostDesktop)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("the binding the runhost relays: %w", err))
	}
	var r desktop.Relayed
	if err := json.Unmarshal(b, &r); err != nil || r.Socket == "" || r.Run == "" || r.Bound.Token == "" {
		return nil, engine.Classify(engine.ExitCLI, errors.New("the binding the runhost relays is not one"))
	}
	return engine.RelayedDesktopLink(r), nil
}

// bindDesktopCap binds a run of ref to Agentry Desktop over the link socket
// in home, for the desktop line d.
func bindDesktopCap(ctx context.Context, d *spec.DesktopCap, ref, digest, home, runID, outDir string) (*engine.DesktopLink, error) {
	c := desktop.Client{Socket: desktop.SocketPath(home)}
	if fi, err := os.Lstat(c.Socket); err != nil || fi.Mode()&fs.ModeSocket == 0 {
		return nil, engine.NoDesktopRefusal(d)
	}
	req := engine.DesktopBindRequest(d, runID, ref, digest, outDir)
	bound, err := c.Bind(ctx, req)
	if err != nil {
		var r *desktop.Refusal
		switch {
		case errors.As(err, &r):
			return nil, engine.DesktopBindRefusal(d, r)
		case errors.Is(err, desktop.ErrNoService):
			return nil, engine.NoDesktopRefusal(d)
		}
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("bind the run to Agentry Desktop: %w", err))
	}
	return engine.BoundDesktopLink(c, runID, bound), nil
}

// desktopSummary is one phrase for the plan's desktop line.
func desktopSummary(d *spec.DesktopCap) string {
	var apps []string
	for _, s := range d.Servers {
		if s.Narrowed {
			noun := "tools"
			if len(s.Tools) == 1 {
				noun = "tool"
			}
			apps = append(apps, fmt.Sprintf("%s (%d %s)", s.Application, len(s.Tools), noun))
		} else {
			apps = append(apps, s.Application+" (every tool its server lists)")
		}
	}
	switch d.ComputerUse {
	case spec.ComputerUseApp:
		apps = append(apps, "computer use in the windows you allow")
	case spec.ComputerUseScreen:
		apps = append(apps, "computer use of your whole screen")
	}
	return joinAnd(apps) + " on your computer, through Agentry Desktop"
}

// joinAnd lists items for a sentence: "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
