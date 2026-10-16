// Package gateway owns the "installed on first agent use" half:
// making sure the pinned docker-mcp-gateway and harness images are present
// and the local runner image is built, before any run needs them.
// Only the containerized gateway is supported: the CLI-plugin auto-install
// path is rejected for its supply-chain surface.
package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// EnsureInstalled verifies the docker engine answers, pulls the pinned gateway
// + harness images if absent, and builds the local runner image from the
// embedded context if absent. Called from the install step of `agentry run`:
// after this returns nil, a run needs no further network access for
// infrastructure. harnessImage says whether this run needs the pinned harness
// image at all: an adapter that drives the person's own installation runs no
// container, so pulling docker-agent for it would be a download nobody asked
// for.
func EnsureInstalled(ctx context.Context, d dockerx.Client, pol *policy.Policy,
	runnerTag, imageCtxDir string, logw io.Writer, harnessImage bool,
) error {
	if err := d.Available(ctx); err != nil {
		return err
	}
	if err := EnsureRuntime(ctx, d, pol); err != nil {
		return err
	}
	images := []string{pol.Images.Gateway}
	if harnessImage {
		images = append(images, pol.Images.HarnessDockerAgent)
	}
	for _, ref := range images {
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
	// binary, so the image's own code needs no registry at all. Its two bases
	// do: they come from our registry (ghcr.io/blaktron), GO_IMAGE from the
	// policy's images.goBuilder and RUNTIME_IMAGE from the Dockerfile's pinned
	// default, so first use needs ghcr.io for them and nothing else: the
	// Dockerfile names no external frontend. A new embedded context derives a
	// new tag, which is what triggers rebuilds. The log names the builder base
	// it hands the build, digest and all, so a run's own output shows what the
	// image was built from.
	_, _ = fmt.Fprintf(logw, "building %s from embedded context with GO_IMAGE=%s (first use)\n", runnerTag, pol.Images.GoBuilder)
	if err := runnerimage.Materialize(imageCtxDir); err != nil {
		return fmt.Errorf("materialize runner image context: %w", err)
	}
	if err := d.Build(ctx, imageCtxDir, runnerTag, map[string]string{"GO_IMAGE": pol.Images.GoBuilder}); err != nil {
		// The context's go.mod names the CLI's Go to the patch, and the Go
		// images refuse a newer go line under GOTOOLCHAIN=local: say which
		// setting to change rather than leave it in docker's output.
		if strings.Contains(err.Error(), "go.mod requires go >=") {
			return fmt.Errorf("build runner image: images.goBuilder %s is older than the Go the runner needs; point it at a builder of that Go or newer, or remove it for the default: %w", pol.Images.GoBuilder, err)
		}
		return fmt.Errorf("build runner image: %w", err)
	}
	return nil
}

// EnsureInstalledFor is EnsureInstalled for one plan: the harness image
// prepared is the one the plan's adapter runs and no other, none for a host
// harness. A published image is pulled by its pinned ref; one pinned by the
// artifact it is built from is built on this machine (harnessbuild), refused
// when this build cannot build it.
func EnsureInstalledFor(ctx context.Context, d dockerx.Client, pol *policy.Policy,
	runnerTag, imageCtxDir string, logw io.Writer, plan *spec.Plan,
) error {
	h := plan.Harness
	if h.Host != nil || h.Agent.Image == "" || h.Agent.Image == pol.Images.HarnessDockerAgent {
		return EnsureInstalled(ctx, d, pol, runnerTag, imageCtxDir, logw, h.Host == nil && h.Agent.Image != "")
	}
	pin := policy.HarnessImage{Ref: h.Agent.Image}
	if h.ImagePin != nil && h.ImagePin.By == policy.PinArtifact {
		pin.Artifact = h.ImagePin.Value
	}
	if err := checkHarnessImage(h.Adapter, pin); err != nil {
		return err
	}
	if err := EnsureInstalled(ctx, d, pol, runnerTag, imageCtxDir, logw, false); err != nil {
		return err
	}
	return EnsureHarnessImage(ctx, d, h.Adapter, pin, logw)
}

// checkHarnessImage refuses an image pinned by the artifact it is built
// from when this build cannot build it: such an image is never taken by
// its tag, since only the build that makes it can say it holds that
// artifact, so the receipt's pin could not be true.
func checkHarnessImage(adapterName string, h policy.HarnessImage) error {
	if h.Artifact != "" && !harnessbuild.Builds(adapterName) {
		return fmt.Errorf("the %s harness image %s is built on this computer from its vendor's artifact (%s), and this build of agentry can neither build it nor check one already there", adapterName, h.Ref, h.Artifact)
	}
	return nil
}

// EnsureHarnessImage makes adapterName's harness image h present: pulled
// by its pinned ref, which names a digest, or built here from the artifact
// it is pinned by. Setup prepares the default adapter's image with
// it, a run its plan's (EnsureInstalledFor).
func EnsureHarnessImage(ctx context.Context, d dockerx.Client, adapterName string, h policy.HarnessImage, logw io.Writer) error {
	if err := checkHarnessImage(adapterName, h); err != nil {
		return err
	}
	if h.Artifact != "" {
		return harnessbuild.Builder{Docker: d, Log: logw}.Ensure(ctx, adapterName, h)
	}
	ok, err := d.ImageExists(ctx, h.Ref)
	if err != nil || ok {
		return err
	}
	_, _ = fmt.Fprintf(logw, "pulling %s (first use)\n", h.Ref)
	if err := d.Pull(ctx, h.Ref); err != nil {
		return fmt.Errorf("pull %s: %w", h.Ref, err)
	}
	return nil
}

// EnsureRuntime refuses, before anything is pulled or started, a policy whose
// docker.runtime the daemon has not registered: a run that quietly landed on
// runc after asking for runsc would carry a receipt claiming an isolation it
// did not have. The error names the runtime the policy wants and the ones the
// daemon has. The gateway's own runtime (docker.gatewayRuntime) is held to the
// same rule, and by what it runs as well as its name: it must be
// docker.runtime's binary registered again with --host-uds=open and nothing
// else, since the receipt says the gateway ran under it. An unset runtime asks
// nothing. On podman a runtime is refused outright (PodmanRuntimeRefusal).
func EnsureRuntime(ctx context.Context, d dockerx.Client, pol *policy.Policy) error {
	if pol.Docker.Runtime == "" && pol.Docker.GatewayRuntime == "" {
		return nil
	}
	e, err := dockerx.DescribeEngine(ctx, d)
	if err != nil {
		return err
	}
	if e.Kind == dockerx.KindPodman {
		return PodmanRuntimeRefusal(pol)
	}
	table, err := d.Runtimes(ctx)
	if err != nil {
		return err
	}
	for _, w := range []struct{ key, want, how string }{
		{"docker.runtime", pol.Docker.Runtime, "install it and register it in the daemon's runtimes"},
		{"docker.gatewayRuntime", pol.Docker.GatewayRuntime, "register " + pol.Docker.Runtime + " again under that name with runtimeArgs [\"" + dockerx.HostUDSOpen + "\"]"},
	} {
		if _, ok := table[w.want]; w.want == "" || ok {
			continue
		}
		return fmt.Errorf("%s %q is not registered on this docker daemon (it has: %s): %s, or unset %s in the policy — the runner never falls back to another runtime",
			w.key, w.want, strings.Join(dockerx.SortedRuntimeNames(table), ", "), w.how, w.key)
	}
	if gw := pol.Docker.GatewayRuntime; gw != "" {
		if err := dockerx.CheckHostUDSRegistration(table, pol.Docker.Runtime, gw); err != nil {
			return fmt.Errorf("docker.gatewayRuntime %s, or unset docker.gatewayRuntime in the policy — the receipt would claim a sandbox the gateway did not have", err)
		}
	}
	return nil
}

// PodmanRuntimeRefusal is why a policy's runtime is refused on podman. The
// gateway creates the tool and MCP server containers through podman's
// Docker-compatible API, which takes a container's runtime and drops it: they
// would run under podman's default while the receipt named the policy's.
func PodmanRuntimeRefusal(pol *policy.Policy) error {
	key, want := "docker.runtime", pol.Docker.Runtime
	if want == "" {
		key, want = "docker.gatewayRuntime", pol.Docker.GatewayRuntime
	}
	return fmt.Errorf("%s %q is not held on podman: the containers the gateway creates go through podman's Docker-compatible API, which does not apply a container's runtime, so they would run under podman's default while the receipt named %q: unset docker.runtime and docker.gatewayRuntime in the policy to run on podman, or run on Docker Engine",
		key, want, want)
}

// ExecPackagesFile is where the exec image build records the resolved
// versions of the catalogued packages it installed ("name-version" per
// line, `apk list --installed`), read back into the receipt.
const ExecPackagesFile = "/etc/agentry/exec-packages"

// EnsureExecImage builds the per-QuickAgent exec image for a plan when absent:
// `FROM <runnerImage>`, the operator's catalogued apk packages installed (from
// the policy's mirror when one is named) with their resolved versions recorded
// into the image, git's safe.directory when git is declared, then one exec-form
// RUN of the runner binary's `minimize` role, so the resulting image holds only
// the declared commands (and the runner binary that serves the tools). The
// context is a generated Dockerfile under ctxDir. Returns the resolved package
// versions, read from the image whether it was built now or found; nil when no
// exec is declared.
//
// An Unsafe Mode exec image (img.Unsafe) is built from the unsafe base instead
// and rebuilt when its last build left tools uninstalled; its result lists what
// installed, then what did not, each of those marked
// spec.ExecNotInstalledPrefix.
func EnsureExecImage(ctx context.Context, d dockerx.Client, runnerImage string, img spec.ExecImageSpec, ctxDir string, logw io.Writer) ([]string, error) {
	if img.Tag == "" {
		return nil, nil
	}
	if img.Unsafe != nil {
		return ensureUnsafeExecImage(ctx, d, runnerImage, img, ctxDir, logw)
	}
	ok, err := d.ImageExists(ctx, img.Tag)
	if err != nil {
		return nil, err
	}
	if !ok {
		dir := filepath.Join(ctxDir, "exec-"+img.Tag[strings.LastIndex(img.Tag, ":")+1:])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("exec image context: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(execDockerfile(runnerImage, img)), 0o600); err != nil {
			return nil, fmt.Errorf("exec image context: %w", err)
		}
		_, _ = fmt.Fprintf(logw, "building %s (runner image + apk %s, minimized to %s)\n", img.Tag, strings.Join(img.Packages, " "), strings.Join(img.Commands, ", "))
		if err := d.Build(ctx, dir, img.Tag, nil); err != nil {
			return nil, fmt.Errorf("build exec image %s: %w", img.Tag, err)
		}
	}
	if len(img.Packages) == 0 {
		return nil, nil
	}
	b, err := d.ImageFile(ctx, img.Tag, ExecPackagesFile)
	if err != nil {
		return nil, fmt.Errorf("read the exec image's package record: %w", err)
	}
	var resolved []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			resolved = append(resolved, line)
		}
	}
	sort.Strings(resolved)
	return resolved, nil
}

// execDockerfile renders the exec image build. Every RUN is in exec form
// only after busybox may be gone; the apk and git steps run while the
// shell still exists.
func execDockerfile(runnerImage string, img spec.ExecImageSpec) string {
	var b strings.Builder
	b.WriteString("# generated by agentry: the runner image with the operator's catalogued packages, minimized to the declared exec commands\n")
	b.WriteString("FROM " + runnerImage + "\n")
	b.WriteString(imagelabel.Line(imagelabel.Exec))
	if len(img.Packages) > 0 {
		if img.APKMirror != "" {
			// The mirror serves the image's own Alpine release; the
			// release is read from the image rather than assumed.
			m := strings.TrimRight(img.APKMirror, "/")
			b.WriteString("RUN v=$(cut -d. -f1,2 /etc/alpine-release) && printf '%s/v%s/main\\n%s/v%s/community\\n' " +
				shellQuote(m) + " \"$v\" " + shellQuote(m) + " \"$v\" > /etc/apk/repositories\n")
		}
		names := make([]string, 0, len(img.Packages))
		var install []string
		for _, p := range img.Packages {
			names = append(names, strings.SplitN(p, "=", 2)[0])
			// Unpinned busybox is the runner image's own (every base
			// command's): asking apk for it again would
			// make the build fetch the indexes for nothing, and fail
			// where they cannot be fetched.
			if p != "busybox" {
				install = append(install, p)
			}
		}
		b.WriteString("RUN ")
		if len(install) > 0 {
			b.WriteString("apk add --no-cache " + strings.Join(install, " ") + " && ")
		}
		b.WriteString("mkdir -p " + path.Dir(ExecPackagesFile) +
			" && apk list --installed " + strings.Join(names, " ") + " | cut -d' ' -f1 > " + ExecPackagesFile + "\n")
	}
	if slices.Contains(img.Commands, "git") {
		// A bind-mounted workspace owned by the host user, read by root
		// inside the container, is "dubious ownership" to git. The run's
		// mounts are the only places a repository can be — the workspace
		// and anything under the inputs folder — so exactly those paths
		// are trusted (git's `/dir/*` form covers subdirectories). Named
		// in the receipt.
		for _, dir := range []string{spec.WorkspaceMount, spec.WorkspaceMount + "/*", spec.InputsMount + "/*"} {
			b.WriteString("RUN git config --system --add safe.directory " + shellQuote(dir) + "\n")
		}
	}
	quoted := make([]string, 0, len(img.Commands))
	for _, c := range slices.Sorted(slices.Values(img.Commands)) {
		quoted = append(quoted, strconv.Quote(c))
	}
	b.WriteString("RUN [\"/usr/local/bin/agentry-runner\", \"minimize\", " + strings.Join(quoted, ", ") + "]\n")
	return b.String()
}

// shellQuote single-quotes a value for a Dockerfile RUN line.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
