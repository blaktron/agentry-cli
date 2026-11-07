package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// sessionStore makes the run's session store: a per-run named volume,
// labelled like every run object and removed with it, not a host directory
// the harness could write to world-wide. An interactive session keeps its
// harness's store there, and so does an autonomous harness that can be
// resumed (spec.HarnessSpec.Resume). The volume is then given to the harness
// image's own user (giveSessionStore).
func sessionStore(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID, owner string) error {
	if err := d.VolumeCreate(ctx, rd.session, runLabels(runID)); err != nil {
		return Classify(ExitCLI, fmt.Errorf("create session volume: %w", err))
	}
	td.add(func(c context.Context) { _ = d.VolumeRemove(c, rd.session) })
	return giveSessionStore(ctx, d, p, rd, td, runID, owner)
}

// giveSessionStore hands the session store to owner, the harness image's
// own user: a fresh volume is root-owned wherever it is mounted and the
// harness runs unprivileged, so the store is given to it first — as root, in
// that image, on no network, once. An empty owner leaves the volume as docker
// made it.
func giveSessionStore(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID, owner string) error {
	if owner == "" {
		return nil
	}
	initName := containerName(runID, "session-init")
	td.add(func(c context.Context) { _ = d.RemoveForce(c, initName) })
	// The one-shot that hands the store to the harness user keeps
	// CHOWN, the one capability its chown needs, and nothing else.
	initSpec := hardened(dockerx.RunSpec{
		Name:       initName,
		Image:      p.Plan.Harness.Agent.Image,
		Entrypoint: "sh",
		Argv:       []string{"-c", `chown -- "$1" "$2"`, "sh", owner, spec.SessionMount},
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: rd.session, Target: spec.SessionMount}},
		Labels:     runLabels(runID),
		AutoRemove: true,
		Runtime:    p.Plan.Runtime(),
	}, p.limits())
	initSpec.CapAdd = []string{"CHOWN"}
	code, err := dockerx.RunForegroundUnder(ctx, d, initSpec, "session store's one-shot", nil, io.Discard, p.Stderr)
	if err != nil || code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d", code)
		}
		return Classify(ExitCLI, fmt.Errorf("give the session store to the harness user %q: %w", owner, err))
	}
	return nil
}
