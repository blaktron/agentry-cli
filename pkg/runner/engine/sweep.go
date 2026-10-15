package engine

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// SweepAbandoned removes, before a run builds anything, the containers an
// earlier run on this host left behind when its process died without tearing
// down — a SIGKILL, an OOM kill, a power cut: the paths no defer and no
// recover reaches. A container qualifies only when its owner labels prove
// its creator gone (owner.Dead): created from this process's boot, pid
// namespace and uid, by a pid that is no longer the process that started it.
// A sibling run's containers are never touched, however old; nor is anything
// created before the owner labels existed, by another user, from another pid
// namespace, or anywhere off Linux (no labels are written there). For each
// removed run container the run's networks and volumes go too, after the
// containers: its session volume by name and every volume carrying its run
// label, the gateway's /misc among them.
//
// Best effort and bounded: the sweep never fails a run, and it tells the
// person what it removed and what it could not. Run and StartSession call
// it, then sweep the dead runs' files (sweepAbandonedRun); exported so
// `agentry run` can sweep before its MCP server installs, whose fixed
// container names a leftover installer would collide with.
// Not covered: a run network or volume whose containers were already gone
// when the process died has nothing left to name it (a labelled volume's
// owner labels are not read here).
func SweepAbandoned(ctx context.Context, d dockerx.Client, stderr io.Writer) {
	_ = sweepAbandoned(ctx, d, stderr)
}

// sweepAbandonedRun is the sweep a run or a session starts with: the
// containers, networks and volumes of runs whose process died
// (SweepAbandoned), then the files they left in this install's run
// directories (sweepRunDirs), which a dead run's containers also name when
// its directory predates the run lock.
func sweepAbandonedRun(ctx context.Context, d dockerx.Client, runRoot string, stderr io.Writer) {
	sweepRunDirs(runRoot, sweepAbandoned(ctx, d, stderr), stderr)
}

// sweepAbandoned is SweepAbandoned, returning the ids of the runs whose
// containers it removed.
func sweepAbandoned(ctx context.Context, d dockerx.Client, stderr io.Writer) map[string]bool {
	id, ok := owner.Identity()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	found, err := d.PSLabels(ctx, owner.LabelHost+"="+id, []string{owner.LabelHost, owner.LabelPID, owner.LabelStart, runLabel})
	if err != nil {
		return nil
	}
	var removed, failed []string
	runs := map[string]bool{}
	for _, c := range found {
		if !owner.Dead(c.Labels) {
			continue
		}
		if err := d.RemoveForce(ctx, c.Name); err != nil {
			failed = append(failed, c.Name)
			continue
		}
		removed = append(removed, c.Name)
		if r := c.Labels[runLabel]; r != "" {
			runs[r] = true
		}
	}
	for r := range runs {
		for _, n := range runNetworks(ctx, d, r) {
			_ = d.NetworkRemove(ctx, n)
		}
		for _, v := range runVolumes(ctx, d, r) {
			_ = d.VolumeRemove(ctx, v)
		}
	}
	if stderr == nil {
		return runs
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		_, _ = fmt.Fprintf(stderr, "removed %d container(s) a run whose process had died left behind: %s\n", len(removed), strings.Join(removed, ", "))
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		_, _ = fmt.Fprintf(stderr, "warning: could not remove %d container(s) a run whose process had died left behind: %s (docker rm -fv <name>)\n", len(failed), strings.Join(failed, ", "))
	}
	return runs
}

// runVolumes is every volume of run r: its session volume by name, and
// every volume carrying its run label, which finds the gateway's /misc
// when its container went without -v. The name stands when docker
// cannot list.
func runVolumes(ctx context.Context, d dockerx.Client, r string) []string {
	names := []string{sessionVolumeName(r)}
	labelled, err := d.VolumesByLabel(ctx, runLabel+"="+r)
	if err != nil {
		return names
	}
	for _, v := range labelled {
		if !slices.Contains(names, v) {
			names = append(names, v)
		}
	}
	return names
}

// runNetworks is every network of run r: those its id names (the run's own,
// the llm-proxy's egress under a runtime, the web tools', and the relays'
// pair), and every network carrying its run label, which finds those a step
// names itself, such as an input source's. The names stand when docker
// cannot list.
func runNetworks(ctx context.Context, d dockerx.Client, r string) []string {
	internal, out := relayNetworks(r)
	names := []string{networkName(r), proxyEgressNetwork(r), webNetwork(r), internal, out}
	labelled, err := d.NetworksByLabel(ctx, runLabel+"="+r)
	if err != nil {
		return names
	}
	for _, n := range labelled {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}
