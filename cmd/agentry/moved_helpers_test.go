package main

import (
	"io"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ensurePlan and cachedPlanVersion moved to pkg/runner/localrun
// (agentry-cli#318), where the desktop and the built-in run share them, and
// EnsurePlan's own reuse, recompile and tamper paths are tested there. The
// CLI's tests that drive it through --local's install keep these two names,
// over the moved code, rather than rewrite every call.

func ensurePlan(errOut io.Writer, home string, m *manifest.Manifest, meta *installdir.Meta, installDir, workspaceDir string,
	pol *policy.Policy, polSHA, adapterName string, serverSpecs []spec.MCPServerSpec, mode manifest.Mode,
) (*spec.Plan, string, error) {
	return localrun.EnsurePlan(localrun.PlanInputs{
		Home: home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspaceDir,
		Policy: pol, PolicySHA: polSHA, Adapter: adapterName, Servers: serverSpecs, Mode: mode,
		CompiledBy: version.Build(), Log: errOut,
	})
}

func cachedPlanVersion(b []byte) string { return localrun.PlanVersionOf(b) }
