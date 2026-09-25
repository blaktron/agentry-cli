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

// The helpers these tests were written against moved to pkg/runner/localrun
// (agentry-cli#318), where the desktop and the built-in run share them; these
// keep the tests reading as they did, over the moved code.

func ensurePlan(errOut io.Writer, home string, m *manifest.Manifest, meta *installdir.Meta, installDir, workspaceDir string,
	pol *policy.Policy, polSHA, adapterName string, serverSpecs []spec.MCPServerSpec, mode manifest.Mode,
) (*spec.Plan, string, error) {
	return localrun.EnsurePlan(localrun.PlanInputs{
		Home: home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspaceDir,
		Policy: pol, PolicySHA: polSHA, Adapter: adapterName, Servers: serverSpecs, Mode: mode,
		CompiledBy: version.Build(), Log: errOut,
	})
}

func localTreeDigest(dir string) (string, error)                   { return localrun.TreeDigest(dir) }
func manifestFromWorkspace(dir string) (*manifest.Manifest, error) { return localrun.ManifestFrom(dir) }
func cachedPlanVersion(b []byte) string                            { return localrun.PlanVersionOf(b) }
