package mcpinstall

import (
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// PythonChoice is the Python runtime a PyPI server installs and runs on.
type PythonChoice struct {
	// Image is the runtime image reference.
	Image string
	// Version is the Python that image runs; "" for a policy's own image
	// whose tag names no line.
	Version string
	// Fallback is true when Image is the policy's fallback runtime, chosen
	// because the release's requires_python excludes the default's Python.
	Fallback bool
	// Why says, for the plan and the receipt, how the runtime was chosen.
	Why string
}

// ChoosePython picks the Python runtime for a PyPI release from its
// requires_python (declared, as the registry's metadata gives it), before
// anything is installed:
//
//   - a policy whose pythonRuntime is its own image gets exactly that image,
//     whatever the range says: the operator's pin is never overridden;
//   - otherwise the newest of the default runtime's Python and the fallback's
//     that the range admits: the default when it does, the fallback when only
//     it does, and an error when neither does;
//   - a release that declares no range, or one that cannot be read, stays on
//     the default, as pip ignores such a range ("Ignoring invalid
//     Requires-Python").
//
// The error carries receipt.ReasonMCPRuntimeExcluded.
func ChoosePython(r Resolved, images policy.Images) (PythonChoice, error) {
	if images.PythonPinned() {
		return PythonChoice{
			Image: images.PythonRuntime, Version: policy.PythonVersionOf(images.PythonRuntime),
			Why: "the policy's own images.pythonRuntime, which is never replaced by a fallback",
		}, nil
	}
	def := PythonChoice{Image: images.PythonRuntime, Version: policy.PythonVersionOf(images.PythonRuntime), Why: "the default Python runtime"}
	if def.Image == "" {
		def.Image, def.Version = policy.DefaultPythonRuntime, policy.DefaultPythonVersion
	}
	if r.DeclaredRuntime == "" {
		return def, nil
	}
	if ok, valid := pep440Admits(r.DeclaredRuntime, def.Version); !valid || ok {
		return def, nil
	}
	fb := PythonChoice{Image: images.PythonFallbackImage(), Fallback: true}
	fb.Version = policy.PythonVersionOf(fb.Image)
	if fb.Version != "" {
		if ok, valid := pep440Admits(r.DeclaredRuntime, fb.Version); valid && ok {
			fb.Why = fmt.Sprintf("the fallback Python runtime: its requires_python %s excludes the default's Python %s", quoteRange(r.DeclaredRuntime), def.Version)
			return fb, nil
		}
	}
	return PythonChoice{}, receipt.WithReason(fmt.Errorf(
		"%s@%s needs Python %s (its requires_python), which neither the default runtime (Python %s) nor the fallback (Python %s) satisfies; nothing was installed. Pin a version of the server whose range one of them satisfies, or set the runner policy's images.pythonRuntime to an image with a Python it supports",
		r.Package, r.Version, quoteRange(r.DeclaredRuntime), def.Version, fb.Version),
		receipt.ReasonMCPRuntimeExcluded, "", r.SourceID())
}

// AssignRuntimes records, on each resolved PyPI server, whether it installs
// and runs on the fallback Python (Resolved.PythonFallback), and refuses a
// server no runtime of the policy can install. A server the store already
// holds keeps the Python it was installed on: its virtual environment belongs
// to that line. It is idempotent.
func AssignRuntimes(resolved []Resolved, images policy.Images) error {
	for i := range resolved {
		r := &resolved[i]
		if r.Source != "pypi" {
			continue
		}
		if r.InstalledAt != "" {
			if r.PythonFallback && images.PythonPinned() {
				return installFailed(r, receipt.InstallPinChanged, fmt.Errorf("server store for %s holds %s@%s installed on the fallback Python runtime, but the policy now sets images.pythonRuntime to its own image: remove the profile to install it again on that image",
					r.ID, r.Package, r.Version))
			}
			continue
		}
		c, err := ChoosePython(*r, images)
		if err != nil {
			return err
		}
		r.PythonFallback = c.Fallback
	}
	return nil
}

// Runtime names the Python a resolved PyPI server runs on under images, as
// the plan and the receipt say it ("Python 3.14.10"); "" for any other
// server. A policy's own image whose tag names no Python line gives "Python
// (the policy's own image)".
func (r Resolved) Runtime(images policy.Images) string {
	if r.Source != "pypi" {
		return ""
	}
	if v := policy.PythonVersionOf(r.Image(images)); v != "" {
		return "Python " + v
	}
	return "Python (the policy's own image)"
}
