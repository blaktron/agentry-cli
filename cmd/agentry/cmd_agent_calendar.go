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
	"Some addresses answer only a signed-in browser: a Google Workspace calendar\n" +
	"whose administrator turned off secret addresses gives only its \"Public address\n" +
	"in iCal format\", which sends a fetch to Google's sign-in page. Add\n" +
	"--calendar-sign-in: a browser window (Chrome, Edge, Chromium or Brave, in a\n" +
	"profile of its own that is removed afterwards) opens at the address and you\n" +
	"sign in. Agentry does not control the window while you do. It closes by itself\n" +
	"once the calendar downloads; otherwise close it (on macOS, quit the browser)\n" +
	"when you are signed in. The cookies that browser sends to the calendar's host\n" +
	"are then kept in your keychain beside the address, sent to that host alone.\n" +
	"Set AGENTRY_BROWSER to a Chromium-family browser's program to use another.\n" +
	"The service may end the sign-in after a while; run the same command again,\n" +
	"pressing Enter at the address to keep it, to sign in again.\n\n" +
	"  agentry agent configure assistant --calendar-feed Work < work-address.txt\n" +
	"  agentry agent configure assistant --calendar-feed Work --calendar-sign-in"

// signInCalendarFeed is the browser sign-in: the real one, but in a test.
var signInCalendarFeed = persistent.SignInCalendarFeed

// configureCalendarFeed keeps a calendar feed's address under name (on), or
// removes the feed (off). With signIn, the feed is read after a sign-in in
// a browser window, which is kept beside the address; an empty address
// then keeps the feed's own.
func configureCalendarFeed(cmd *cobra.Command, a persistent.Agent, home, name string, on, signIn, yes bool) error {
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
	var feed *persistent.CalendarFeed
	for _, f := range c.CalendarFeeds {
		if strings.EqualFold(f.Name, name) {
			feed = &f
			break
		}
	}
	if !on {
		if feed == nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no calendar feed %q", a.Name, name))
		}
		r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveCalendarFeed: feed.Name}, false)
		if err != nil {
			return err
		}
		if err := persistent.ForgetCalendarFeed(agentSecrets, a.Name, feed.Name); err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		printChanged(cmd.OutOrStdout(), a, r, running)
		return nil
	}
	if err := spec.CheckCalendarFeedName(name); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	// A name set again keeps the one it was first given.
	if feed != nil {
		name = feed.Name
	}
	account := persistent.CalendarKeychainAccount(a.Name, name)
	var raw string
	if signIn && feed != nil {
		if raw, err = readSecretInput(cmd, "Calendar feed address (iCal / ICS; Enter keeps "+name+"'s own)", "the calendar feed's address", true); err != nil {
			return engine.Classify(engine.ExitUsage, err)
		}
		if raw == "" {
			if raw, err = agentSecrets.Get(account); err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the address of %s could not be read from your keychain (%v): give it again", name, err))
			}
		}
	} else if raw, err = readSecretValue(cmd, "Calendar feed address (iCal / ICS)", "the calendar feed's address"); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	address, host, err := spec.CalendarFeedAddress(raw)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	var session, calName string
	var events int
	if signIn {
		progress := func(s string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), s) }
		session, calName, events, err = signInCalendarFeed(cmd.Context(), address, progress)
		if err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the browser sign-in for %s: %w", name, err))
		}
	} else if calName, events, err = persistent.CalendarFeedReadable(cmd.Context(), address); err != nil {
		if errors.Is(err, persistent.ErrCalendarFeedSignIn) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("%w: sign in through a browser window with agentry agent configure %s --calendar-feed %q --calendar-sign-in", err, a.Name, name))
		}
		if errors.Is(err, persistent.ErrCalendarFeedUnreadable) {
			return engine.Classify(engine.ExitUsage, err)
		}
		return engine.Classify(engine.ExitCLI, err)
	}
	previous, _ := agentSecrets.Get(account)
	previousSession := ""
	if feed != nil && feed.SignedIn {
		previousSession, _ = persistent.CalendarSession(agentSecrets, a.Name, name)
	}
	restore := func() {
		if previous != "" {
			_ = agentSecrets.Set(account, previous)
		} else {
			_ = agentSecrets.Delete(account)
		}
		if previousSession != "" {
			_ = persistent.KeepCalendarSession(agentSecrets, a.Name, name, previousSession)
		} else {
			_ = persistent.ForgetCalendarSession(agentSecrets, a.Name, name)
		}
	}
	if err := agentSecrets.Set(account, address); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if signIn {
		err = persistent.KeepCalendarSession(agentSecrets, a.Name, name, session)
	} else {
		err = persistent.ForgetCalendarSession(agentSecrets, a.Name, name)
	}
	if err != nil {
		restore()
		return engine.Classify(engine.ExitCLI, err)
	}
	r, running, err := configureAgent(cmd, home, a, persistent.Change{SetCalendarFeed: &persistent.CalendarFeed{Name: name, Host: host, Set: time.Now().UTC(), SignedIn: signIn}}, yes)
	if err != nil {
		restore()
		return err
	}
	printCalendarRead(cmd.OutOrStdout(), name, host, calName, events, signIn)
	printChanged(cmd.OutOrStdout(), a, r, running)
	return nil
}

// printCalendarRead says what the check read of the feed.
func printCalendarRead(w io.Writer, name, host, calName string, events int, signedIn bool) {
	from := host
	if s := persistent.CalendarService(host); s != "" {
		from = s
	}
	what := "a calendar"
	if calName != "" {
		what = fmt.Sprintf("the calendar %q", calName)
	}
	if signedIn {
		_, _ = fmt.Fprintf(w, "Signed in and read %s from %s (%d events), and kept its address and the browser's sign-in in your system keychain as the feed %s. "+
			"The sign-in is sent to %s alone. If %s ends it later, the Assistant says so: sign in again with --calendar-sign-in.\n", what, from, events, name, host, from)
		return
	}
	_, _ = fmt.Fprintf(w, "Read %s from %s (%d events), and kept its address in your system keychain as the feed %s.\n", what, from, events, name)
}
