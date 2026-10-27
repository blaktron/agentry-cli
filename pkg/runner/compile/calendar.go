package compile

import (
	"fmt"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The Assistant's calendar feeds (Inputs.Calendar): the calendar tools
// read the feeds its roster has on, each feed's address a secret of the
// tools container's, whose network reaches the feeds' hosts and nothing
// else. Like the Coding agent's grants it is a part of the persistent grant
// class, gated by the person's agreement for this agent, so it is given to
// the persistent Assistant alone, beside its folders, and its manifest
// declares no egress of its own.

// calendarGrant checks the Assistant's calendar grant and sets it on caps.
func calendarGrant(in Inputs, mode manifest.Mode, caps *spec.CapSummary) error {
	if mode != manifest.ModePersistent || in.Source != "builtin" {
		return &PolicyRefusal{Reason: "calendar feeds are a persistent built-in's alone"}
	}
	if in.Manifest.Namespace != assistant.Namespace || in.Manifest.Name != assistant.Name {
		return &PolicyRefusal{Reason: fmt.Sprintf("calendar feeds are %s/%s's alone", assistant.Namespace, assistant.Name)}
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

// calendarEnforcement is the plan's line for the calendar feeds.
func calendarEnforcement(g *spec.CalendarGrant) string {
	var named []string
	for _, f := range g.Feeds {
		named = append(named, fmt.Sprintf("%s (%s)", f.Name, f.Host))
	}
	return "the person's calendar feeds " + strings.Join(named, ", ") + ", whose addresses they keep in their keychain and agreed to give this agent (its roster's calendar entries), are read by the tools server's calendar tools, read only: " +
		"each address comes from the gateway's secrets file, which the engine writes for the segment and removes at its end, into the tools container alone, the plan holding the names and hosts alone; " +
		"the tools container's network reaches the feeds' hosts on 443 through the gateway's proxy and nothing else, and the tools server fetches a feed by https from its own host, refusing a redirect to any other; " +
		"the model names a feed and is never given its address, which is masked in every tool result and refused in a file the agent writes"
}
