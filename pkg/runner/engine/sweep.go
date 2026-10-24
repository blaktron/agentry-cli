package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// A run network or volume whose containers were already gone when the
// process died has nothing left here to name it (a labelled volume's owner
// labels are not read), and off Linux no container is judged here at all;
// the run's directory lock does both, and the sweep a run or a session
// starts with (sweepAbandonedRun) removes them by it.
func SweepAbandoned(ctx context.Context, d dockerx.Client, stderr io.Writer) {
	_ = sweepAbandoned(ctx, d, stderr)
}

// sweepAbandonedRun is the sweep a run or a session starts with: the
// containers, networks and volumes of runs whose process died
// (SweepAbandoned), then the files they left in the run directories of this
// install and of every other install in the runner home (sweepRunDirs over
// runRoots), which also takes the dead runs' ids for a
// directory that holds no lock file, and the delivery folders of a process
// that died in the middle of a delivery (sweepStaleDeliveryFolders). Last,
// the containers, networks and volumes of every run whose free lock proved
// it dead and that the container sweep did not already reach. A run killed
// before any of its containers existed, or whose containers were removed
// before it died, leaves its networks with nothing else to name them. A run
// killed with its containers standing, where no owner label judges them
// (off Linux), leaves containers that hold its networks, since docker
// refuses to remove a network a container uses. So the containers carrying
// its run label go first, the gateway's helpers among them, then its
// networks and volumes. Those of a live run, whose lock is held, are never
// touched.
func sweepAbandonedRun(ctx context.Context, d dockerx.Client, runRoot, runnerHome string, stderr io.Writer) {
	byContainers := sweepAbandoned(ctx, d, stderr)
	var swept []string
	for _, root := range runRoots(runRoot, runnerHome) {
		swept = append(swept, sweepRunDirs(root, runnerHome, byContainers, stderr)...)
	}
	sweepStaleDeliveryFolders(runRoot)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var removed, failed, runs []string
	for _, r := range swept {
		if byContainers[r] {
			continue
		}
		gone, left := removeRunContainers(ctx, d, r)
		if len(gone) > 0 {
			runs = append(runs, r)
		}
		removed, failed = append(removed, gone...), append(failed, left...)
		removeRunNetworksAndVolumes(ctx, d, r)
	}
	if stderr == nil {
		return
	}
	if len(removed) > 0 {
		_, _ = fmt.Fprintf(stderr, "removed %d container(s) of %d run(s) whose process had died: %s\n", len(removed), len(runs), strings.Join(runs, ", "))
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		_, _ = fmt.Fprintf(stderr, "warning: could not remove %d container(s) a run whose process had died left behind: %s (docker rm -fv <name>)\n", len(failed), strings.Join(failed, ", "))
	}
}

// runRoots are the run roots a start sweeps: its own, then every other
// install's under the runner home (<runnerHome>/agents/<namespace>/<name>/
// <version>), since a new version of a release installs beside the old one
// and a run that died under the old one is named nowhere else. Each run in
// them is still judged by its own lock. Only real directories count: a link
// standing in an install's or its runs' place is not followed.
func runRoots(runRoot, runnerHome string) []string {
	roots := []string{runRoot}
	if runnerHome == "" {
		return roots
	}
	matches, err := filepath.Glob(filepath.Join(runnerHome, "agents", "*", "*", "*", "runs"))
	if err != nil {
		return roots
	}
	for _, runs := range matches {
		root := filepath.Dir(runs)
		if root == filepath.Clean(runRoot) || !realDir(runs) || !realDir(root) || !realDir(filepath.Dir(root)) || !realDir(filepath.Dir(filepath.Dir(root))) {
			continue
		}
		roots = append(roots, root)
	}
	return roots
}

// realDir says p is a directory and not a link to one.
func realDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// removeRunContainers removes every container carrying run r's label, for a
// run proven dead: its own and the gateway's helpers, which the docker
// filter labels. Best effort: gone names what was removed, left what docker
// would not remove.
func removeRunContainers(ctx context.Context, d dockerx.Client, r string) (gone, left []string) {
	names, err := d.PSByLabel(ctx, runLabel+"="+r)
	if err != nil {
		return nil, nil
	}
	for _, n := range names {
		if err := d.RemoveForce(ctx, n); err != nil {
			left = append(left, n)
			continue
		}
		gone = append(gone, n)
	}
	return gone, left
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
		removeRunNetworksAndVolumes(ctx, d, r)
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

// removeRunNetworksAndVolumes removes the networks and volumes of run r,
// whose containers are gone. Best effort: what docker will not remove
// stays.
func removeRunNetworksAndVolumes(ctx context.Context, d dockerx.Client, r string) {
	for _, n := range runNetworks(ctx, d, r) {
		_ = d.NetworkRemove(ctx, n)
	}
	for _, v := range runVolumes(ctx, d, r) {
		_ = d.VolumeRemove(ctx, v)
	}
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
