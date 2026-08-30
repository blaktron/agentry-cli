// Package gateway owns the "installed on first agent use" half of RQ2:
// making sure the pinned docker-mcp-gateway and harness images are present
// and the local runner image (§R7) is built, before any run needs them.
// Containerized gateway only in v1 — RUNNER-DEVIATIONS DR-3 records why
// the CLI-plugin auto-install path was rejected (supply-chain surface).
package gateway

import (
	"context"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// EnsureInstalled verifies the docker engine answers, pulls the pinned
// gateway + harness images if absent, and builds the local runner image
// from the embedded context if absent. Called from the install step of
// `agentry run` (RQ5): after this returns nil, a run needs no further
// network access for infrastructure.
func EnsureInstalled(ctx context.Context, d dockerx.Client, pol *policy.Policy,
	runnerTag, imageCtxDir string, logw io.Writer,
) error {
	if err := d.Available(ctx); err != nil {
		return err
	}
	for _, ref := range []string{pol.Images.Gateway, pol.Images.HarnessDockerAgent} {
		ok, err := d.ImageExists(ctx, ref)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		_, _ = fmt.Fprintf(logw, "pulling %s (first use)\n", ref)
		if err := d.Pull(ctx, ref); err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
	}

	ok, err := d.ImageExists(ctx, runnerTag)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// The runner image is built, not pulled: its source ships inside this
	// binary (§R7), so first use needs our registry for nothing. A new
	// CLI version derives a new tag, which is what triggers rebuilds.
	_, _ = fmt.Fprintf(logw, "building %s from embedded context (first use)\n", runnerTag)
	if err := runnerimage.Materialize(imageCtxDir); err != nil {
		return fmt.Errorf("materialize runner image context: %w", err)
	}
	if err := d.Build(ctx, imageCtxDir, runnerTag, map[string]string{"GO_IMAGE": pol.Images.GoBuilder}); err != nil {
		return fmt.Errorf("build runner image: %w", err)
	}
	return nil
}
