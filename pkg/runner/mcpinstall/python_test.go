package mcpinstall

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func defaultImages() policy.Images { return policy.Default().Images }

func pypiWith(requires string) Resolved {
	return Resolved{Source: "pypi", Package: "markitdown-mcp", Version: "0.0.1a4", ID: "pypi-markitdown-mcp", BinName: "markitdown-mcp", DeclaredRuntime: requires}
}

// The runtime is chosen from the release's requires_python: the default
// Python when its range admits it, the fallback when only that one does, a
// refusal when neither does, and the default when the range says nothing
// usable.
func TestChoosePythonFollowsTheReleasesRange(t *testing.T) {
	images := defaultImages()
	for _, c := range []struct {
		name, requires string
		fallback       bool
		refused        bool
	}{
		{"admits the default", ">=3.10", false, false},
		{"admits the default and more", ">=3.12,<4", false, false},
		{"capped below the default", ">=3.10,<3.15", true, false},
		{"capped to the fallback's line", "~=3.14.0", true, false},
		{"the fallback's patch is excluded", ">=3.10,<3.14.5", false, true},
		{"neither admits it (too old)", ">=3.10,<3.14", false, true},
		{"neither admits it (too new)", ">=3.16", false, true},
		{"no range declared", "", false, false},
		{"an unreadable range", "python at least 3.10", false, false},
		{"an unreadable clause", ">=3.10,~3.x", false, false},
	} {
		got, err := ChoosePython(pypiWith(c.requires), images)
		if c.refused {
			if err == nil {
				t.Errorf("%s (%q): chose %+v, want a refusal", c.name, c.requires, got)
				continue
			}
			if reason := receipt.ReasonOf(err); reason == nil || reason.Code != receipt.ReasonMCPRuntimeExcluded || len(reason.Names) != 1 || reason.Names[0] != "pypi:markitdown-mcp" {
				t.Errorf("%s: reason = %+v", c.name, reason)
			}
			if !strings.Contains(err.Error(), "nothing was installed") || !strings.Contains(err.Error(), policy.DefaultPythonVersion) || !strings.Contains(err.Error(), policy.DefaultPythonFallbackVersion) {
				t.Errorf("%s: the refusal does not say what it ruled out: %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s (%q): %v", c.name, c.requires, err)
			continue
		}
		wantImage, wantVersion := policy.DefaultPythonRuntime, policy.DefaultPythonVersion
		if c.fallback {
			wantImage, wantVersion = policy.DefaultPythonFallbackRuntime, policy.DefaultPythonFallbackVersion
		}
		if got.Fallback != c.fallback || got.Image != wantImage || got.Version != wantVersion {
			t.Errorf("%s (%q): chose %+v, want image %s (%s)", c.name, c.requires, got, wantImage, wantVersion)
		}
	}
}

// A policy that sets pythonRuntime to its own image gets exactly that image:
// no range moves it to the fallback and none refuses the server.
func TestAPolicyThatPinsItsPythonNeverFallsBack(t *testing.T) {
	images := defaultImages()
	images.PythonRuntime = "registry.example/python:3.13-slim@sha256:" + strings.Repeat("a", 64)
	for _, requires := range []string{"", ">=3.10", ">=3.10,<3.15", "<3.5"} {
		got, err := ChoosePython(pypiWith(requires), images)
		if err != nil {
			t.Fatalf("%q: %v", requires, err)
		}
		if got.Fallback || got.Image != images.PythonRuntime {
			t.Errorf("%q: chose %+v, want exactly the policy's image", requires, got)
		}
	}
}

// The fallback is the policy's to name, and its Python is read from the
// image's tag.
func TestAPolicyCanNameItsOwnFallback(t *testing.T) {
	images := defaultImages()
	images.PythonFallbackRuntime = "registry.example/python:3.13-slim@sha256:" + strings.Repeat("b", 64)
	got, err := ChoosePython(pypiWith(">=3.10,<3.14"), images)
	if err != nil || !got.Fallback || got.Image != images.PythonFallbackRuntime || got.Version != "3.13" {
		t.Fatalf("chose %+v (%v), want the policy's fallback", got, err)
	}
	// The default fallback (3.14) is not consulted once the policy names its own.
	if _, err := ChoosePython(pypiWith("~=3.14.0"), images); err == nil {
		t.Fatal("a range only the CLI's own fallback admits was accepted")
	}
}

func TestRuntimeNamesThePythonAServerRunsOn(t *testing.T) {
	images := defaultImages()
	r := pypiWith(">=3.10")
	if got := r.Runtime(images); got != "Python "+policy.DefaultPythonVersion {
		t.Errorf("default: %q", got)
	}
	r.PythonFallback = true
	if got := r.Runtime(images); got != "Python "+policy.DefaultPythonFallbackVersion {
		t.Errorf("fallback: %q", got)
	}
	if got := (Resolved{Source: "npm"}).Runtime(images); got != "" {
		t.Errorf("npm: %q", got)
	}
	images.PythonRuntime = "registry.example/python:custom@sha256:" + strings.Repeat("c", 64)
	if got := (Resolved{Source: "pypi"}).Runtime(images); got != "Python (the policy's own image)" {
		t.Errorf("a tag without a line: %q", got)
	}
}

// A server installed on the fallback keeps it when its store is reused, and a
// policy that has since pinned its own Python refuses it instead of running a
// virtual environment on the wrong interpreter.
func TestAssignRuntimesKeepsWhatTheStoreHolds(t *testing.T) {
	images := defaultImages()
	installed := pypiWith(">=3.10,<3.15")
	installed.InstalledAt, installed.PythonFallback = "2026-10-30T00:00:00Z", true
	fresh := pypiWith(">=3.10,<3.15")
	npm := Resolved{Source: "npm", Package: "x", Version: "1"}
	set := []Resolved{installed, fresh, npm}
	if err := AssignRuntimes(set, images); err != nil {
		t.Fatal(err)
	}
	if !set[0].PythonFallback || !set[1].PythonFallback || set[2].PythonFallback {
		t.Fatalf("assigned %v %v %v", set[0].PythonFallback, set[1].PythonFallback, set[2].PythonFallback)
	}
	// Idempotent.
	if err := AssignRuntimes(set, images); err != nil || !set[1].PythonFallback {
		t.Fatalf("second assignment: %v", err)
	}
	images.PythonRuntime = "registry.example/python:3.13@sha256:" + strings.Repeat("d", 64)
	err := AssignRuntimes([]Resolved{installed}, images)
	if err == nil || !strings.Contains(err.Error(), "remove the profile") {
		t.Fatalf("a fallback install under a pinned policy: %v", err)
	}
	if reason := receipt.ReasonOf(err); reason == nil || reason.Code != receipt.ReasonMCPInstallFailed || reason.Detail != receipt.InstallPinChanged {
		t.Fatalf("reason = %+v", reason)
	}
	// A server installed before the choice was recorded was installed on the default.
	legacy := pypiWith(">=3.10")
	legacy.InstalledAt = "2026-09-01T00:00:00Z"
	if err := AssignRuntimes([]Resolved{legacy}, defaultImages()); err != nil || legacy.PythonFallback {
		t.Fatalf("legacy record: %v %v", err, legacy.PythonFallback)
	}
}

// End to end through the install: the runtime is decided before any container
// starts, the builder runs in the fallback image, the record keeps the choice,
// a reuse touches nothing and keeps it, and the plan's spec names the Python.
func TestEnsureInstalledInstallsOnTheFallbackWhenTheRangeExcludesTheDefault(t *testing.T) {
	images := defaultImages()
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	store := t.TempDir()
	fake := withRunner(&dockerx.Fake{ExistingImages: map[string]bool{images.PythonFallbackImage(): true}})
	res := []Resolved{pypiWith(">=3.10,<3.15")}
	if err := AssignRuntimes(res, images); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	run, ok := fake.FindRun("agentry-mcpinstall-pypi-")
	if !ok || run.Image != images.PythonFallbackImage() {
		t.Fatalf("the builder ran in %q, want the fallback %q", run.Image, images.PythonFallbackImage())
	}
	rec, err := LoadMeta(store, res[0].ID)
	if err != nil || rec == nil || !rec.PythonFallback {
		t.Fatalf("the record does not keep the choice: %+v (%v)", rec, err)
	}
	if b, _ := os.ReadFile(filepath.Join(store, res[0].ID+".json")); !strings.Contains(string(b), `"pythonFallback": true`) {
		t.Fatalf("record on disk: %s", b)
	}
	specs := Specs(res, []manifest.MCPServer{{PyPI: "markitdown-mcp", Version: "0.0.1a4"}}, images)
	if len(specs) != 1 || specs[0].Image != images.PythonFallbackImage() || specs[0].Runtime != "Python "+policy.DefaultPythonFallbackVersion || !specs[0].RuntimeFallback {
		t.Fatalf("spec = %+v", specs)
	}

	// Reuse: a fresh resolution that did not choose the fallback still runs on it.
	again := []Resolved{pypiWith("")}
	ops := len(fake.Ops())
	if err := EnsureInstalled(t.Context(), fake, again, store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fake.Ops()) != ops || !again[0].PythonFallback || again[0].Image(images) != images.PythonFallbackImage() {
		t.Fatalf("reuse: ops %v, fallback %v", fake.Ops()[ops:], again[0].PythonFallback)
	}
}

// A server no runtime can install is refused before anything runs or is pulled.
func TestEnsureInstalledRefusesAServerNoRuntimeCanInstallBeforeTouchingDocker(t *testing.T) {
	images := defaultImages()
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	fake := &dockerx.Fake{}
	res := []Resolved{pypiWith("<3.14")}
	err := EnsureInstalled(t.Context(), fake, res, t.TempDir(), regs, images, policy.DockerPolicy{}, io.Discard)
	if err == nil || receipt.ReasonOf(err) == nil || receipt.ReasonOf(err).Code != receipt.ReasonMCPRuntimeExcluded {
		t.Fatalf("err = %v", err)
	}
	if ops := fake.Ops(); len(ops) != 0 {
		t.Fatalf("docker was touched before the refusal: %v", ops)
	}
}

// An ordinary server stays on the default runtime and says so.
func TestEnsureInstalledKeepsAnOrdinaryServerOnTheDefault(t *testing.T) {
	images := defaultImages()
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	store := t.TempDir()
	fake := withRunner(&dockerx.Fake{ExistingImages: map[string]bool{images.PythonRuntime: true}})
	res := []Resolved{pypiWith(">=3.10")}
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if run, _ := fake.FindRun("agentry-mcpinstall-pypi-"); run.Image != images.PythonRuntime {
		t.Fatalf("the builder ran in %q", run.Image)
	}
	if b, _ := os.ReadFile(filepath.Join(store, res[0].ID+".json")); strings.Contains(string(b), "pythonFallback") {
		t.Fatalf("record names a fallback: %s", b)
	}
	if got := Specs(res, nil, images)[0]; got.Runtime != "Python "+policy.DefaultPythonVersion || got.RuntimeFallback {
		t.Fatalf("spec = %+v", got)
	}
}
