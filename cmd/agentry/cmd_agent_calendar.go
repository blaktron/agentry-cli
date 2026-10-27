package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// agentCalendarLong is configure's help for calendar feeds.
const agentCalendarLong = "\n\n--calendar-feed <name> gives the Assistant a calendar to read, under a name\n" +
	"you choose: the calendar's iCal (ICS) address, read from the terminal without\n" +
	"echoing it, or from stdin. Google Calendar gives it as \"Secret address in iCal\n" +
	"format\" in a calendar's settings; Outlook (Microsoft 365 and Outlook.com) as\n" +
	"the ICS link of Settings > Calendar > Shared calendars > Publish a calendar;\n" +
	"iCloud as a public calendar's webcal:// link. Whoever holds the address reads\n" +
	"the calendar, so it is kept in your system keychain, and the agent is never\n" +
	"shown it. The address is fetched and read once before it is kept. A feed is\n" +
	"read only: the Assistant reads what is on it and never changes it. Giving a\n" +
	"name again sets its address again; --no-calendar-feed <name> removes it. Both\n" +
	"are refused while the agent runs. A feed is a roster entry, switched with\n" +
	"agentry agent grant assistant calendar:<name> on|off.\n\n" +
	"  agentry agent configure assistant --calendar-feed Work < work-address.txt"

// configureCalendarFeed keeps a calendar feed's address under name (on), or
// removes the feed (off).
func configureCalendarFeed(cmd *cobra.Command, a persistent.Agent, home, name string, on, yes bool) error {
	if !a.Builtin.ReadsCalendars() {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no calendar feeds: the Assistant alone reads them", a.Name))
	}
	if held, _ := persistent.Held(home, a.Name); held {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running: stop it first (agentry agent stop %s)", a.Name, a.Name))
	}
	c, err := persistent.LoadConfig(home, a.Name)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if !on {
		var feed *persistent.CalendarFeed
		for _, f := range c.CalendarFeeds {
			if strings.EqualFold(f.Name, name) {
				feed = &f
				break
			}
		}
		if feed == nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no calendar feed %q", a.Name, name))
		}
		r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveCalendarFeed: feed.Name}, false)
		if err != nil {
			return err
		}
		if err := agentSecrets.Delete(persistent.CalendarKeychainAccount(a.Name, feed.Name)); err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		printChanged(cmd.OutOrStdout(), a, r, running)
		return nil
	}
	if err := spec.CheckCalendarFeedName(name); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	// A name set again keeps the one it was first given.
	for _, f := range c.CalendarFeeds {
		if strings.EqualFold(f.Name, name) {
			name = f.Name
		}
	}
	raw, err := readSecretValue(cmd, "Calendar feed address (iCal / ICS)", "the calendar feed's address")
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	address, host, err := spec.CalendarFeedAddress(raw)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	calName, events, err := persistent.CalendarFeedReadable(cmd.Context(), address)
	if err != nil {
		if errors.Is(err, persistent.ErrCalendarFeedUnreadable) {
			return engine.Classify(engine.ExitUsage, err)
		}
		return engine.Classify(engine.ExitCLI, err)
	}
	account := persistent.CalendarKeychainAccount(a.Name, name)
	previous, _ := agentSecrets.Get(account)
	if err := agentSecrets.Set(account, address); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	r, running, err := configureAgent(cmd, home, a, persistent.Change{SetCalendarFeed: &persistent.CalendarFeed{Name: name, Host: host, Set: time.Now().UTC()}}, yes)
	if err != nil {
		if previous != "" {
			_ = agentSecrets.Set(account, previous)
		} else {
			_ = agentSecrets.Delete(account)
		}
		return err
	}
	printCalendarRead(cmd.OutOrStdout(), name, host, calName, events)
	printChanged(cmd.OutOrStdout(), a, r, running)
	return nil
}

// printCalendarRead says what the check read of the feed.
func printCalendarRead(w io.Writer, name, host, calName string, events int) {
	from := host
	if s := persistent.CalendarService(host); s != "" {
		from = s
	}
	what := "a calendar"
	if calName != "" {
		what = fmt.Sprintf("the calendar %q", calName)
	}
	_, _ = fmt.Fprintf(w, "Read %s from %s (%d events), and kept its address in your system keychain as the feed %s.\n", what, from, events, name)
}
