package offer

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ImageServer is a server a built-in offers that is no npm or PyPI package
// and no remote address: a program in an image the CLI pins, started with
// a fixed command. It is a built-in's alone, never a QuickAgent's, since
// the registry's review of a release does not reach an image. It keeps its
// own state on a volume of the agent's that reaches this server alone, and
// it waits, off, until the person has signed it in.
type ImageServer struct {
	// Key names it: its roster entry is server:<Key>, its volume ends
	// -<Key>.
	Key string
	// Name is how the roster names it ("Google Workspace (gog)").
	Name string
	// Image is the pinned image (repository@sha256:…), the policy's.
	Image string
	// Command is what the image's own entrypoint is given, before the
	// switches' arguments.
	Command []string
	// Env is its plain configuration.
	Env map[string]string
	// Egress are the hosts it reaches, each host or host:port, and no other.
	Egress []string
	// Secrets are the secret slots it is given, filled by the runner from
	// the keychain (Slots), never the person's stored secrets.
	Secrets []string
	// VolumeTarget is where its volume is mounted.
	VolumeTarget string
	// Switches widen it, each an entry of the roster off by default and
	// agreed to on its own.
	Switches []ImageSwitch
	// Note is what its consent sentence adds after what it reaches.
	Note string
	// SignIn is what to do before it can be given: until the person has
	// signed it in, it waits.
	SignIn string
	// AccountEnv is the variable the server is told the signed-in account
	// in; empty for none.
	AccountEnv string
}

// ImageSwitch is one widening of an image server: arguments added to its
// command while the switch is on, with the server.
type ImageSwitch struct {
	// Key names it: its roster entry is server:<server key>:<Key>.
	Key string
	// Name is how the roster names it ("Allow changes").
	Name string
	// Line is its consent sentence.
	Line string
	// Args are added to the server's command, in the switches' order.
	Args []string
	// Requires is the key of another switch of the server this one is
	// given with alone; empty for none.
	Requires string
	// SignedFor says the switch is given only when the server's sign-in
	// asked for what it needs (a scope a read-only sign-in leaves out).
	SignedFor bool
}

var imageKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// imageRefRE is an image pinned by digest: a repository and a sha256.
var imageRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// Check refuses an image server a built-in may not offer: one with no key,
// name or sign-in, an image not pinned by digest, no command, a host it
// reaches that is not a host name, a volume target that is not an absolute
// path, a switch that is not one or that requires one the server lacks.
func (s ImageServer) Check() error {
	if !imageKeyRE.MatchString(s.Key) || s.Name == "" {
		return fmt.Errorf("an offered image server has a key of lower-case letters, digits and hyphens and a name (%q, %q)", s.Key, s.Name)
	}
	if !imageRefRE.MatchString(s.Image) {
		return fmt.Errorf("%s: its image %q is not pinned by digest", s.Name, s.Image)
	}
	if len(s.Command) == 0 {
		return fmt.Errorf("%s names no command", s.Name)
	}
	if len(s.Egress) == 0 {
		return fmt.Errorf("%s names the hosts it reaches", s.Name)
	}
	for _, h := range s.Egress {
		name, port, hasPort := strings.Cut(h, ":")
		if strings.Contains(h, "*") || !hostRE.MatchString(name) || numericTLD(name) || hasPort && port != "443" {
			return fmt.Errorf("%s: its egress %q is not a host name on 443", s.Name, h)
		}
	}
	if !strings.HasPrefix(s.VolumeTarget, "/") || strings.Contains(s.VolumeTarget, ":") {
		return fmt.Errorf("%s: its volume target %q is not an absolute path", s.Name, s.VolumeTarget)
	}
	if s.SignIn == "" {
		return fmt.Errorf("%s says nothing of how it is signed in", s.Name)
	}
	keys := map[string]bool{}
	for _, w := range s.Switches {
		if !imageKeyRE.MatchString(w.Key) || w.Name == "" || w.Line == "" || len(w.Args) == 0 || keys[w.Key] {
			return fmt.Errorf("%s: its switch %q is not one (a key, a name, a sentence and the arguments it adds, once each)", s.Name, w.Key)
		}
		keys[w.Key] = true
	}
	for _, w := range s.Switches {
		if w.Requires != "" && (!keys[w.Requires] || w.Requires == w.Key) {
			return fmt.Errorf("%s: its switch %s requires %q, which it does not have", s.Name, w.Key, w.Requires)
		}
	}
	return nil
}

// Args is the server's command with the arguments of the switches on (by
// key) added, in the switches' order; a switch whose requirement is off is
// left out.
func (s ImageServer) Args(on func(key string) bool) []string {
	out := slices.Clone(s.Command)
	for _, w := range s.Switches {
		if on(w.Key) && (w.Requires == "" || on(w.Requires)) {
			out = append(out, w.Args...)
		}
	}
	return out
}

// CheckImageServers refuses a built-in's image servers when one is
// refused, or when two share a key.
func CheckImageServers(servers []ImageServer) error {
	seen := map[string]bool{}
	for _, s := range servers {
		if err := s.Check(); err != nil {
			return err
		}
		if seen[s.Key] {
			return errors.New("the image server " + s.Key + " is offered twice")
		}
		seen[s.Key] = true
	}
	return nil
}
