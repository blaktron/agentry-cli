package compile

import (
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// imageServerPrefix starts the name of a server the runner runs from an
// image it pins, before the server's key.
const imageServerPrefix = "mcp-image-"

// persistentImageServers are the image servers capabilities.persistent may
// name, as the manifest spells them (manifest.ImageServerNames), by the key
// of the server this build runs for each. The manifest names the service, so
// the program behind it can change without a manifest's changing.
var persistentImageServers = map[string]string{
	"google-workspace": assistant.GogKey,
	"microsoft-365":    assistant.MS365Key,
}

// PersistentImageServer is the key of the image server this build runs for
// name, an entry of capabilities.persistent.imageServers; false for a name
// it does not carry.
func PersistentImageServer(name string) (string, bool) {
	key, ok := persistentImageServers[name]
	return key, ok
}

// DeclaredImageServers are the keys of the image servers p declares.
func DeclaredImageServers(p *manifest.Persistent) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, n := range p.ImageServers {
		if key, ok := PersistentImageServer(n); ok {
			out = append(out, key)
		}
	}
	return out
}

// imageServersDeclared refuses a server run from an image to any but a
// persistent agent that declares it under capabilities.persistent.imageServers.
func imageServersDeclared(servers []spec.MCPServerSpec, mode manifest.Mode, p *manifest.Persistent) error {
	declared := DeclaredImageServers(p)
	for _, srv := range spec.ImageServers(servers) {
		if mode != manifest.ModePersistent || !slices.Contains(declared, strings.TrimPrefix(srv.Name, imageServerPrefix)) {
			return &PolicyRefusal{Reason: "the server " + srv.Name + " runs from an image the runner pins, which is granted to a persistent agent that declares it under capabilities.persistent.imageServers, and this one does not"}
		}
	}
	return nil
}
