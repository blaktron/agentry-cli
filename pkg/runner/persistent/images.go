package persistent

// The agent's image servers: servers its built-in offers that run from an
// image the CLI pins rather than a package (offer.ImageServer), the
// Assistant's Google Workspace through gog. Each is an entry of the roster,
// off until switched on and agreed to, and waits, "needs set-up", until the
// person has signed it in; each widening switch is an entry of its own, off
// by default, given only with its server. The server keeps its state on a
// volume of the agent's that the docker filter holds to that server alone,
// and its secret slots are filled from the keychain by the runner.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// KindServerSwitch is the kind of an image server's widening switch.
const KindServerSwitch EntryKind = "server-switch"

// ImageSignIn is an image server's finished sign-in: the account it answers
// as, as the server itself named it, and when.
type ImageSignIn struct {
	Account string    `yaml:"account" json:"account"`
	At      time.Time `yaml:"at" json:"at"`
}

// imageServersOf are the agent's image servers: its built-in's, but in a
// test. The roster names them by key, so the policy's image does not change
// them; a segment takes the image from its policy.
var imageServersOf = func(a Agent) []offer.ImageServer { return a.Builtin.ImageServers(policy.Images{}) }

// ImageServerEntryID is an image server's roster id.
func ImageServerEntryID(key string) string { return serverPrefix + key }

// ImageSwitchEntryID is the roster id of an image server's switch.
func ImageSwitchEntryID(key, sw string) string { return serverPrefix + key + ":" + sw }

// ImageVolumeIn is the volume of the agent's image server key in the runner
// home home.
func ImageVolumeIn(home, name, key string) string {
	return "agentry-persistent-" + name + "-" + HomeID(home) + "-" + key
}

// ImageKeyringAccount is the keychain account of an image server's own
// secret: its file keyring's password, made at its first sign-in.
func ImageKeyringAccount(agent, key string) string {
	return "agent-secret:" + agent + ":" + key + "-keyring"
}

// imageServer is the agent's image server with key key.
func imageServer(a Agent, key string) (offer.ImageServer, bool) {
	for _, s := range imageServersOf(a) {
		if s.Key == key {
			return s, true
		}
	}
	return offer.ImageServer{}, false
}

// imageLine is an image server's consent sentence: what it is, what it
// reaches, and where it keeps its state.
func imageLine(s offer.ImageServer) string {
	line := "Uses " + s.Name + ", run from the image Agentry pins, reaching " + strings.Join(s.Egress, ", ") +
		" alone, keeping its sign-in on a volume of its own that no other part of the agent reaches"
	if s.Note != "" {
		line += "; " + s.Note
	}
	return line
}

// imageEntries are the roster's entries of the agent's image servers: each
// server, off by default and waiting for its sign-in when on without one,
// then its switches, off by default.
func imageEntries(a Agent, c Config, add func(RosterEntry, bool), last func() *RosterEntry) {
	for _, s := range imageServersOf(a) {
		id := ImageServerEntryID(s.Key)
		add(RosterEntry{ID: id, Kind: KindServer, Name: s.Name, GrantedBy: GrantedByBuiltin, Line: imageLine(s)}, false)
		if e := last(); e.On {
			if _, ok := c.SignIns[s.Key]; !ok {
				e.State, e.Reason = StateNeedsSetUp, s.SignIn
			}
		}
		for _, w := range s.Switches {
			add(RosterEntry{ID: ImageSwitchEntryID(s.Key, w.Key), Kind: KindServerSwitch, Name: w.Name, GrantedBy: GrantedByBuiltin, Line: w.Line}, false)
		}
	}
}

// imageGrants are the image servers a segment is given (on, and signed in),
// each with the switches it has on, and the entries that are on but not
// given, by id, which Apply leaves out of what it names as applied.
func imageGrants(home string, a Agent, c Config, on func(id string) bool) (grants []localrun.ImageGrant, left map[string]bool) {
	left = map[string]bool{}
	for _, s := range imageServersOf(a) {
		id := ImageServerEntryID(s.Key)
		_, signedIn := c.SignIns[s.Key]
		given := on(id) && signedIn
		if on(id) && !signedIn {
			left[id] = true
		}
		var switches []string
		for _, w := range s.Switches {
			wid := ImageSwitchEntryID(s.Key, w.Key)
			if !on(wid) {
				continue
			}
			if !given || w.Requires != "" && !on(ImageSwitchEntryID(s.Key, w.Requires)) {
				left[wid] = true
				continue
			}
			switches = append(switches, w.Key)
		}
		if given {
			grants = append(grants, localrun.ImageGrant{Key: s.Key, Switches: switches, Volume: ImageVolumeIn(home, a.Name, s.Key)})
		}
	}
	return grants, left
}

// imageSecrets are the secret slots of the image servers a segment is
// given, filled from the keychain: each server's file keyring password.
func imageSecrets(keys Secrets, a Agent, grants []localrun.ImageGrant) (map[string]string, error) {
	var out map[string]string
	for _, g := range grants {
		s, ok := imageServer(a, g.Key)
		if !ok || len(s.Secrets) == 0 {
			continue
		}
		if keys == nil {
			return nil, errors.New("the agent has a server on that keeps its sign-in under a password in the keychain, and this client cannot read the keychain")
		}
		v, err := keys.Get(ImageKeyringAccount(a.Name, s.Key))
		if err != nil {
			return nil, fmt.Errorf("%s keeps its sign-in under a password the keychain no longer has: %w (sign it in again)", s.Name, err)
		}
		if out == nil {
			out = map[string]string{}
		}
		for _, slot := range s.Secrets {
			out[slot] = v
		}
	}
	return out, nil
}

// imageVolumes are the volumes of the agent's image servers in home, which
// Reset removes.
func imageVolumes(home string, a Agent) []string {
	var out []string
	for _, s := range imageServersOf(a) {
		out = append(out, ImageVolumeIn(home, a.Name, s.Key))
	}
	slices.Sort(out)
	return out
}
