package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/hostpin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// ImageOneShot is one command of a persistent built-in's image server,
// run outside a segment on the server's own volume: its sign-in's steps.
type ImageOneShot struct {
	// Image is the server's pinned image, run by its own entrypoint and as
	// its own user.
	Image string
	Argv  []string
	// Volume is the server's volume, mounted at Target.
	Volume, Target string
	// Env is passed by name through the client's environment, never in
	// argv (dockerx.RunSpec.Env).
	Env   map[string]string
	Stdin io.Reader
	// Egress are the hosts (host:port) the command may reach, through a
	// CONNECT proxy of its own that reaches them alone at the addresses this
	// machine resolves for them; empty runs it with no network.
	Egress []string
	// ProxyImage is the runner image, whose proxy role is that proxy.
	ProxyImage string
	Runtime    string
	Limits     policy.Limits
}

// imageOneShotProxyAlias is the proxy's name on the one-shot's network.
const imageOneShotProxyAlias = "agentry-oneshot-proxy"

// RunImageOneShot runs o and returns what it wrote to stdout, and its exit
// code. Every container it starts is hardened as a run's are, with a
// read-only root and a small /tmp, and is removed, with the proxy's
// network, whatever the outcome.
func RunImageOneShot(ctx context.Context, d dockerx.Client, o ImageOneShot) (stdout, stderr string, code int, err error) {
	if o.Image == "" || len(o.Argv) == 0 || o.Volume == "" || !strings.HasPrefix(o.Target, "/") {
		return "", "", 0, errors.New("an image one-shot names its image, its command and its volume")
	}
	runID, err := NewRunID()
	if err != nil {
		return "", "", 0, err
	}
	labels := runLabels(runID)
	keep := context.WithoutCancel(ctx)
	name := containerName(runID, "image-oneshot")
	s := dockerx.RunSpec{
		Name:        name,
		Image:       o.Image,
		Argv:        o.Argv,
		Network:     "none",
		Env:         o.Env,
		Mounts:      []dockerx.Mount{{Source: o.Volume, Target: o.Target}},
		Labels:      labels,
		AutoRemove:  true,
		Runtime:     o.Runtime,
		Interactive: o.Stdin != nil,
	}
	if len(o.Egress) > 0 {
		network, proxy, err := startOneShotProxy(ctx, d, runID, o, labels)
		defer func() {
			_ = d.RemoveForce(keep, name)
			if proxy != "" {
				_ = d.RemoveForce(keep, proxy)
			}
			if network != "" {
				_ = d.NetworkRemove(keep, network)
			}
		}()
		if err != nil {
			return "", "", 0, err
		}
		s.Network = network
		u := fmt.Sprintf("http://%s:%d", imageOneShotProxyAlias, spec.ProxyPort)
		env := map[string]string{}
		for k, v := range o.Env {
			env[k] = v
		}
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			env[k] = u
		}
		env["NO_PROXY"], env["no_proxy"] = "", ""
		s.Env = env
	} else {
		defer func() { _ = d.RemoveForce(keep, name) }()
	}
	s = readOnlyRoot(hardened(s, o.Limits.WithDefaults()), "/tmp:size=16m,mode=1777")
	var out, errOut bytes.Buffer
	code, err = dockerx.RunForegroundUnder(ctx, d, s, "image one-shot", o.Stdin, &out, &errOut)
	return out.String(), errOut.String(), code, err
}

// startOneShotProxy starts the one-shot's proxy on an internal network of
// its own, reaching o.Egress alone, its hosts pinned at this machine's
// addresses (an engine's second network may bring no name server), and
// joined to the engine's default network as its way out.
func startOneShotProxy(ctx context.Context, d dockerx.Client, runID string, o ImageOneShot, labels map[string]string) (network, proxy string, err error) {
	if o.ProxyImage == "" {
		return "", "", errors.New("an image one-shot that reaches a host needs the runner image's proxy")
	}
	network = "agentry-" + runID + "-oneshot-net"
	if err := d.NetworkCreate(ctx, network, true, labels); err != nil {
		return "", "", fmt.Errorf("create the one-shot's network: %w", err)
	}
	proxy = containerName(runID, "oneshot-proxy")
	ps := dockerx.RunSpec{
		Name:       proxy,
		Image:      o.ProxyImage,
		Argv:       []string{"proxy", "--allow", strings.Join(o.Egress, ","), "--public-only", "--addr", fmt.Sprintf(":%d", spec.ProxyPort)},
		Network:    network,
		Alias:      imageOneShotProxyAlias,
		Labels:     labels,
		ExtraHosts: hostpin.Pins(ctx, o.Egress, lookupModelHost),
	}
	ps = readOnlyRoot(hardened(ps, o.Limits.WithDefaults()), "/tmp")
	if err := d.RunDetached(ctx, ps); err != nil {
		return network, proxy, fmt.Errorf("start the one-shot's proxy: %w", err)
	}
	if err := d.NetworkConnect(ctx, "bridge", proxy); err != nil {
		return network, proxy, fmt.Errorf("connect the one-shot's proxy to the bridge: %w", err)
	}
	return network, proxy, nil
}
