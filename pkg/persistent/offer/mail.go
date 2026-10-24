package offer

import (
	"errors"
	"regexp"
	"slices"
)

// MailProvider is one service whose mail and calendar a built-in reaches
// through servers it offers. The person enables the providers they use: a
// disabled provider's servers are not on the agent's roster and go to no
// segment, and what it was set up with (its sign-in, its volume) is kept, so
// enabling it again needs no new set-up.
type MailProvider struct {
	// Key names it in the agent's configuration (google, microsoft,
	// fastmail).
	Key string
	// Name is how it is shown ("Google").
	Name string
	// ImageServers are the keys of the built-in's image servers that are
	// this provider's.
	ImageServers []string
}

var mailProviderKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckMailProviders refuses a built-in's mail providers when one has no
// key of the right shape or no server, when two share a key, or when a
// server is not one of servers or is two providers'.
func CheckMailProviders(providers []MailProvider, servers []ImageServer) error {
	keys := map[string]bool{}
	owner := map[string]string{}
	for _, p := range providers {
		if !mailProviderKeyRE.MatchString(p.Key) {
			return errors.New("the mail provider key " + p.Key + " is not a short lower-case word")
		}
		if keys[p.Key] {
			return errors.New("the mail provider " + p.Key + " is offered twice")
		}
		keys[p.Key] = true
		if p.Name == "" || len(p.ImageServers) == 0 {
			return errors.New("the mail provider " + p.Key + " has no name or no server")
		}
		for _, k := range p.ImageServers {
			if !slices.ContainsFunc(servers, func(s ImageServer) bool { return s.Key == k }) {
				return errors.New("the mail provider " + p.Key + " names the image server " + k + ", which is not offered")
			}
			if o, ok := owner[k]; ok {
				return errors.New("the image server " + k + " is both " + o + "'s and " + p.Key + "'s")
			}
			owner[k] = p.Key
		}
	}
	return nil
}
