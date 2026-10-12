package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// The gateway image's VOLUME /misc is mounted as an anonymous volume that
// carries the run's labels, so it is not the unlabelled volume every run
// used to leave behind (agentry-cli#458). Teardown removes the gateway by
// RemoveForce, which takes its anonymous volumes (-v, pinned in dockerx's
// TestCLIStopAndRemoveForce), and then every volume the run label finds:
// the gateway's /misc as well, had its container gone some other way.
func TestTheGatewaysVolumeIsTheRunsAndGoesWithIt(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.LeftoverVolumes = []string{"0a1b2c-misc"}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gw, ok := f.FindRun("-gateway")
	if !ok {
		t.Fatal("gateway run not recorded")
	}
	if len(gw.Volumes) != 1 || gw.Volumes[0].Target != gatewayVolumeDir {
		t.Fatalf("gateway volumes = %+v, want one at %s", gw.Volumes, gatewayVolumeDir)
	}
	labels := gw.Volumes[0].Labels
	if labels[runLabel] != "cafe0000" {
		t.Errorf("the gateway's volume labels %v lack the run label", labels)
	}
	if _, ok := owner.Identity(); ok && labels[owner.LabelHost] == "" {
		t.Errorf("the gateway's volume labels %v lack the owner labels the run's other objects carry", labels)
	}
	ops := f.Ops()
	removed, listed, gone := slices.Index(ops, "RemoveForce:agentry-cafe0000-gateway"), slices.Index(ops, "VolumesByLabel:agentry.run=cafe0000"), slices.Index(ops, "VolumeRemove:0a1b2c-misc")
	if removed < 0 || listed < removed || gone < listed {
		t.Errorf("want the gateway removed, then the run's volumes listed by label and removed: %v", ops)
	}
}

// A dead run's labelled volumes go with its containers, beside its session
// volume by name; a live sibling's are never listed (agentry-cli#458).
func TestSweepRemovesADeadRunsLabelledVolumes(t *testing.T) {
	dead := deadOwner(t)
	f := &dockerx.Fake{
		Containers: []dockerx.Labeled{
			{Name: "agentry-dead0001-gateway", Labels: withRun(dead, "dead0001")},
			{Name: "agentry-live0002-gateway", Labels: withRun(owner.Labels(), "live0002")},
		},
		LeftoverVolumes: []string{"9f8e7d-misc"},
	}
	SweepAbandoned(context.Background(), f, nil)
	ops := f.Ops()
	for _, want := range []string{"VolumesByLabel:agentry.run=dead0001", "VolumeRemove:agentry-dead0001-session", "VolumeRemove:9f8e7d-misc"} {
		if !slices.Contains(ops, want) {
			t.Errorf("missing %s in %v", want, ops)
		}
	}
	if slices.Contains(ops, "VolumesByLabel:agentry.run=live0002") {
		t.Errorf("the sweep listed a live run's volumes: %v", ops)
	}
	if slices.Index(ops, "VolumeRemove:9f8e7d-misc") < slices.Index(ops, "RemoveForce:agentry-dead0001-gateway") {
		t.Errorf("a volume removed before the container that uses it: %v", ops)
	}
}
