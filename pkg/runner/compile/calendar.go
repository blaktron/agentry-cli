package compile

import (
	"fmt"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Calendar feeds (Inputs.Calendar): the calendar tools read the feeds the
// agent's roster has on, each feed's address a secret of the tools
// container's, whose network reaches the feeds' hosts and nothing else. Like
// the coding toolchain it is a part of the persistent grant class, gated by
// the person's agreement for this agent, so it is given only to a persistent
// agent whose manifest declares capabilities.persistent.calendars, beside its
// folders, and the manifest declares no egress of its own.

// calendarGrant checks the Assistant's calendar grant and sets it on caps.
func calendarGrant(in Inputs, mode manifest.Mode, caps *spec.CapSummary) error {
	if mode != manifest.ModePersistent || !in.Manifest.Capabilities.Persistent.Declares("calendars") {
		return &PolicyRefusal{Reason: "calendar feeds are granted to a persistent agent that declares capabilities.persistent.calendars, and this one does not"}
	}
	if in.AgentFiles == nil {
		return &PolicyRefusal{Reason: "calendar feeds come with the agent's folders"}
	}
	if caps.EgressMode != "none" || caps.Coder != nil {
		return &PolicyRefusal{Reason: "calendar feeds are the tools server's only network: the agent declares no egress of its own beside them"}
	}
	if err := spec.CheckCalendarGrant(in.Calendar); err != nil {
		return &PolicyRefusal{Reason: err.Error()}
	}
	if z := in.Calendar.TimeZone; z != "" {
		if _, err := time.LoadLocation(z); err != nil || z == "Local" {
			return &PolicyRefusal{Reason: fmt.Sprintf("the calendar's time zone %q is not an IANA zone", z)}
		}
	}
	g := *in.Calendar
	g.Feeds = append([]spec.CalendarFeed(nil), in.Calendar.Feeds...)
	caps.Calendar = &g
	return nil
}

// calendarEnforcement is the plan's line for the calendar feeds; beside
// the web tools, the feeds are reached over their network.
func calendarEnforcement(g *spec.CalendarGrant, besideWeb bool) string {
	var named []string
	signedIn := false
	for _, f := range g.Feeds {
		named = append(named, fmt.Sprintf("%s (%s)", f.Name, f.Host))
		signedIn = signedIn || f.SessionEnv != ""
	}
	signIn := ""
	if signedIn {
		signIn = "a feed signed in through a browser has that sign-in's cookies from the gateway's secrets file too, which the tools server sends to that feed's host alone and masks like the address; "
	}
	reach := "the tools container's network reaches the feeds' hosts on 443 through the gateway's proxy and nothing else, and the tools server fetches a feed by https from its own host, refusing a redirect to any other; "
	if besideWeb {
		reach = "the tools container is on the web tools' network, which reaches the public web, and the tools server fetches a feed by https from its own host with no proxy, connecting only to a public address on 443 and refusing a redirect to any other; "
	}
	return "the person's calendar feeds " + strings.Join(named, ", ") + ", whose addresses they keep in their keychain and agreed to give this agent (its roster's calendar entries), are read by the tools server's calendar tools, read only: " +
		"each address comes from the gateway's secrets file, which the engine writes for the segment and removes at its end, into the tools container alone, the plan holding the names and hosts alone; " +
		reach +
		signIn +
		"the model names a feed and is never given its address, which is masked in every tool result and refused in a file the agent writes"
}
