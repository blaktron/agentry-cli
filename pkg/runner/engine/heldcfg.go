package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// cfgBinds holds the run's cfg folder to the containers of the gateway's
// catalog: beneath it, a container the gateway launches may bind only a
// path an entry of the run's rendered catalog binds, and only the container
// of an entry that binds it (its image, run by the image's own entrypoint
// with the entry's command). The folder holds what no container of the
// gateway's needs (the harness's files, the model sign-in's token, the
// gateway's own secrets file) and what one of them needs and no other may
// bind (a connection's token, the target hosts' sign-ins, the browser's
// secrets, the share's verdicts and the text read out of its documents),
// so a bind of the folder, or of any path in it, by any other container is
// refused, whatever networks that container joins.
//
// roots and writable are the gateway's read roots and writable paths, cfg
// the run's cfg folder and catalog the rendered catalog, each in the
// gateway's spelling. It returns the read roots with cfg replaced by the
// paths the catalog binds beneath it, the writable paths less any beneath
// cfg the catalog does not bind, and the paths the catalog binds held to
// the entries that bind them. A run with no catalog binds nothing beneath
// cfg.
func cfgBinds(roots, writable []string, cfg string, catalog []byte) ([]string, []string, []sockfilter.HeldRoot, error) {
	if cfg == "" {
		return roots, writable, nil, nil
	}
	cfg = path.Clean(cfg)
	var doc struct {
		Registry map[string]struct {
			Image   string   `yaml:"image"`
			Command []string `yaml:"command"`
			Volumes []string `yaml:"volumes"`
		} `yaml:"registry"`
	}
	if err := yaml.Unmarshal(catalog, &doc); err != nil {
		return nil, nil, nil, fmt.Errorf("read the gateway's catalog: %w", err)
	}
	names := make([]string, 0, len(doc.Registry))
	for name := range doc.Registry {
		names = append(names, name)
	}
	sort.Strings(names)
	holders := map[string][]sockfilter.Holder{}
	for _, name := range names {
		e := doc.Registry[name]
		// The container the entry makes is given these and no others.
		h := sockfilter.Holder{Image: e.Image, Cmd: e.Command, Binds: entryBinds(e.Volumes)}
		for _, v := range e.Volumes {
			src, _, _ := strings.Cut(v, ":")
			src = path.Clean(src)
			if src != cfg && !strings.HasPrefix(src, cfg+"/") {
				continue
			}
			if !slices.ContainsFunc(holders[src], func(o sockfilter.Holder) bool {
				return o.Image == h.Image && slices.Equal(o.Cmd, h.Cmd) && slices.Equal(o.Binds, h.Binds)
			}) {
				holders[src] = append(holders[src], h)
			}
		}
	}
	out := make([]string, 0, len(roots)+len(holders))
	for _, r := range roots {
		if path.Clean(r) != cfg {
			out = append(out, r)
		}
	}
	held := make([]sockfilter.HeldRoot, 0, len(holders))
	for src, by := range holders {
		held = append(held, sockfilter.HeldRoot{Root: src, By: by})
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Root < held[j].Root })
	for _, h := range held {
		if !slices.Contains(out, h.Root) {
			out = append(out, h.Root)
		}
	}
	var write []string
	for _, w := range writable {
		c := path.Clean(w)
		if (c == cfg || strings.HasPrefix(c, cfg+"/")) && holders[c] == nil {
			continue
		}
		write = append(write, w)
	}
	return out, write, held, nil
}

// volumeHolders are the catalog entries that mount each of the named volumes
// names: the docker filter holds each to them alone. A volume no entry
// mounts is held to none, so nothing may mount it.
func volumeHolders(catalog []byte, names []string) ([]sockfilter.HeldRoot, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var doc struct {
		Registry map[string]struct {
			Image   string   `yaml:"image"`
			Command []string `yaml:"command"`
			Volumes []string `yaml:"volumes"`
		} `yaml:"registry"`
	}
	if err := yaml.Unmarshal(catalog, &doc); err != nil {
		return nil, fmt.Errorf("read the gateway's catalog: %w", err)
	}
	entries := slices.Sorted(maps.Keys(doc.Registry))
	out := make([]sockfilter.HeldRoot, 0, len(names))
	for _, name := range slices.Sorted(slices.Values(names)) {
		h := sockfilter.HeldRoot{Root: name, By: []sockfilter.Holder{}}
		for _, entry := range entries {
			e := doc.Registry[entry]
			binds := entryBinds(e.Volumes)
			if slices.ContainsFunc(binds, func(b sockfilter.HolderBind) bool { return b.Source == name }) {
				h.By = append(h.By, sockfilter.Holder{Image: e.Image, Cmd: e.Command, Binds: binds})
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// runVolumeHolders is volumeHolders for this run: its rendered catalog and
// the volumes of its image servers.
func runVolumeHolders(rd *runDirs, plan *spec.Plan) ([]sockfilter.HeldRoot, error) {
	var names []string
	for _, srv := range spec.ImageServers(plan.MCPServers) {
		for _, v := range srv.Volumes {
			name, _, _ := strings.Cut(v, ":")
			names = append(names, name)
		}
	}
	if len(names) == 0 || rd.cfg == "" {
		return nil, nil
	}
	catalog, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "catalog.yaml")) //nolint:gosec // our own rendered config
	if err != nil {
		return nil, Classify(ExitGateway, fmt.Errorf("read the gateway's catalog: %w", err))
	}
	held, err := volumeHolders(catalog, names)
	if err != nil {
		return nil, Classify(ExitGateway, err)
	}
	return held, nil
}

// entryBinds are a catalog entry's volumes as the docker filter compares
// them with a create: source and target, the options left out. A volume
// that names no target (an anonymous one) is not a bind of the entry's and
// is left out; the gateway's own spelling of the rest is kept.
func entryBinds(volumes []string) []sockfilter.HolderBind {
	binds := make([]sockfilter.HolderBind, 0, len(volumes))
	for _, v := range volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 {
			continue
		}
		src := parts[0]
		if strings.HasPrefix(src, "/") {
			src = path.Clean(src)
		}
		binds = append(binds, sockfilter.HolderBind{Source: src, Target: path.Clean(parts[1])})
	}
	return binds
}

// runCfgBinds is cfgBinds for this run: its gateway's bind lists and its
// rendered catalog.
func runCfgBinds(rd *runDirs, roots, writable []string) ([]string, []string, []sockfilter.HeldRoot, error) {
	if rd.cfg == "" {
		return roots, writable, nil, nil
	}
	catalog, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "catalog.yaml")) //nolint:gosec // our own rendered config
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, Classify(ExitGateway, fmt.Errorf("read the gateway's catalog: %w", err))
	}
	roots, writable, held, err := cfgBinds(roots, writable, rd.gatewayPath(rd.cfg), catalog)
	if err != nil {
		return nil, nil, nil, Classify(ExitGateway, err)
	}
	return roots, writable, held, nil
}
