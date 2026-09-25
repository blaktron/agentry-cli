package sockfilter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Ownership lookups ask the daemon for an object's labels. Every error
// refuses: nothing is forwarded on a lookup that did not answer.

// ours says whether an object is this run's gateway's: made through this
// filter, which alone sets OwnerLabel.
func (f *Filter) ours(labels map[string]string) bool {
	return labels[f.cfg.OwnerLabel] == f.cfg.RunID
}

// container is what the filter needs to know of one.
type container struct {
	id      string // the daemon's full id, which requests are forwarded by
	imageID string // the image it runs, by id: a name can be re-pointed, this cannot
}

func (f *Filter) ownContainer(ctx context.Context, id string) (container, error) {
	var c struct {
		ID      string `json:"Id"`
		ImageID string `json:"Image"`
		Config  struct {
			Labels map[string]string
		}
	}
	code, err := f.inspect(ctx, "/containers/"+url.PathEscape(id)+"/json", &c)
	if err != nil {
		return container{}, err
	}
	if code == http.StatusNotFound {
		return container{}, notFound{"container", id}
	}
	if !f.ours(c.Config.Labels) {
		return container{}, deny("container %s is not this run's", short(id))
	}
	return container{id: c.ID, imageID: c.ImageID}, nil
}

// ownNetwork resolves a network by name, id or id prefix: every later rule
// is applied to its name (a shared network named by its id is still the
// shared network), and it is forwarded by its id. sharedOK admits the
// shared ones (never for removal).
func (f *Filter) ownNetwork(ctx context.Context, id string, sharedOK bool) (network, error) {
	var n struct {
		ID     string `json:"Id"`
		Name   string
		Labels map[string]string
	}
	code, err := f.inspect(ctx, "/networks/"+url.PathEscape(id), &n)
	if err != nil {
		return network{}, err
	}
	if code == http.StatusNotFound {
		return network{}, notFound{"network", id}
	}
	if n.ID == "" {
		n.ID = n.Name
	}
	got := network{id: n.ID, name: n.Name}
	if f.shared(n.Name) {
		// The runner's network for this run, and only if it says so: a
		// network another gateway made under the name is not it.
		if n.Labels[f.cfg.Label] != f.cfg.RunID {
			return network{}, deny("network %s is not this run's", n.Name)
		}
		if !sharedOK {
			return network{}, deny("network %s is the runner's for this run and not the gateway's to remove", n.Name)
		}
		return got, nil
	}
	if !f.ours(n.Labels) {
		return network{}, deny("network %s is not this run's", n.Name)
	}
	return got, nil
}

// network is a resolved network: rules apply to its name, and requests are
// forwarded by its id, so a name cannot be re-pointed between the two.
type network struct{ id, name string }

func (f *Filter) ownVolume(ctx context.Context, name string) error {
	var v struct{ Labels map[string]string }
	code, err := f.inspect(ctx, "/volumes/"+url.PathEscape(name), &v)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return notFound{"volume", name}
	}
	if !f.ours(v.Labels) {
		return deny("volume %s is not this run's", short(name))
	}
	return nil
}

// runNetwork translates a network name the gateway uses to the daemon's.
func (f *Filter) runNetwork(name string) string {
	if n, ok := f.cfg.NetworkNames[name]; ok {
		return n
	}
	return name
}

// gatewayNetwork translates the daemon's network name back to the
// gateway's.
func (f *Filter) gatewayNetwork(name string) string {
	for g, n := range f.cfg.NetworkNames {
		if n == name {
			return g
		}
	}
	return name
}

// shared says whether a network name is one the host's runs share.
func (f *Filter) shared(name string) bool {
	for _, s := range f.cfg.SharedNetworks {
		if s == name {
			return true
		}
	}
	_, egress := f.cfg.EgressNetworks[name]
	return egress
}

// egressAllowed says whether a container of this image may join a
// network: an egress network takes only its pinned relays.
func (f *Filter) egressAllowed(ctx context.Context, network, imageID string) error {
	pins, ok := f.cfg.EgressNetworks[network]
	if !ok {
		return nil
	}
	pinned, err := f.pinned(ctx, imageID, pins)
	if err != nil {
		return err
	}
	if !pinned {
		return deny("network %s reaches past the host and takes the pinned relays only, not image %s", network, short(strings.TrimPrefix(imageID, "sha256:")))
	}
	return nil
}

// isRelay says whether an image, by id, is one of any egress network's
// relays.
func (f *Filter) isRelay(ctx context.Context, imageID string) (bool, error) {
	var pins []string
	for _, p := range f.cfg.EgressNetworks {
		pins = append(pins, p...)
	}
	if len(pins) == 0 {
		return false, nil
	}
	return f.pinned(ctx, imageID, pins)
}

// resolveImage returns the id of the image a reference names now; the
// filter judges and forwards that id, never the name, which a registry or
// a pull can re-point after the check.
func (f *Filter) resolveImage(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", deny("a container names no image")
	}
	var img struct {
		ID string `json:"Id"`
	}
	// An image reference keeps its slashes: the daemon's route takes the
	// rest of the path as the name.
	code, err := f.inspect(ctx, (&url.URL{Path: "/images/" + ref + "/json"}).EscapedPath(), &img)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound || img.ID == "" {
		return "", notFound{"image", ref}
	}
	return img.ID, nil
}

// pinned says whether an image, by id, is one of the pinned digests: the
// daemon's record of the digests an image was pulled by is
// content-addressed, and an id names one image for good.
func (f *Filter) pinned(ctx context.Context, imageID string, pins []string) (bool, error) {
	if imageID == "" {
		return false, nil
	}
	var img struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	code, err := f.inspect(ctx, (&url.URL{Path: "/images/" + imageID + "/json"}).EscapedPath(), &img)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound || img.ID != imageID {
		return false, nil
	}
	for _, d := range img.RepoDigests {
		for _, p := range pins {
			if d == p || d == "docker.io/"+p {
				return true, nil
			}
		}
	}
	return false, nil
}

func (f *Filter) inspect(ctx context.Context, p string, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+p, nil)
	if err != nil {
		return 0, deny("inspect: %v", err)
	}
	res, err := f.upstream.Do(req)
	if err != nil {
		return 0, deny("inspect %s: %v", p, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return res.StatusCode, nil
	}
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, deny("inspect %s: daemon answered %d", p, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(into); err != nil {
		return res.StatusCode, deny("inspect %s: %v", p, err)
	}
	return res.StatusCode, nil
}
