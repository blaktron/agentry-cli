package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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
		for _, v := range e.Volumes {
			src, _, _ := strings.Cut(v, ":")
			src = path.Clean(src)
			if src != cfg && !strings.HasPrefix(src, cfg+"/") {
				continue
			}
			h := sockfilter.Holder{Image: e.Image, Cmd: e.Command}
			if !slices.ContainsFunc(holders[src], func(o sockfilter.Holder) bool {
				return o.Image == h.Image && slices.Equal(o.Cmd, h.Cmd)
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
