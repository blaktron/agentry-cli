package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ExportVolume writes the files of the named volume to w as a tar archive,
// in a one-shot of image (the runner image): no network, the volume
// read-only, DAC_READ_SEARCH alone so every file of it is read, under the
// runtime and the limits a run's containers have. The one-shot carries a
// run's labels of its own, so a sweep removes it if this process dies.
func ExportVolume(ctx context.Context, d dockerx.Client, image, volume, runtime string, lim policy.Limits, w io.Writer) error {
	runID, err := NewRunID()
	if err != nil {
		return err
	}
	name := containerName(runID, "volume-export")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      image,
		Entrypoint: "tar",
		Argv:       []string{"-c", "-f", "-", "-C", "/volume", "."},
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: volume, Target: "/volume", ReadOnly: true}},
		Labels:     runLabels(runID),
		AutoRemove: true,
		Runtime:    runtime,
	}, lim.WithDefaults())
	s.CapAdd = []string{"DAC_READ_SEARCH"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "volume's export", nil, w, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the export of the volume %s exited %d: %s", volume, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}
