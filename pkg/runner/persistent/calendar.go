package persistent

// The Assistant's calendar feeds: an iCalendar (ICS) address the person
// gives it under a name they choose, each a roster entry calendar:<name>.
// Whoever holds the address reads the calendar, so it is kept in the
// system keychain under calendar-feed:<agent>:<name>, never in config.yaml,
// which records the name, the host the address is on and when it was last
// set. While the entry is on, the segment gives the address to the tools
// server's calendar tools alone, which fetch it from that host and no other
// (spec.CalendarGrant); the agent names the feed and is never shown it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/schedule"
	"github.com/blaktron/agentry-cli/runnerimage/ical"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// KindCalendar is a calendar feed's roster entry.
const KindCalendar EntryKind = "calendar"

// calendarPrefix begins a calendar feed's roster id.
const calendarPrefix = "calendar:"

// calendarFeedsVersion is the version of config.yaml that added
// CalendarFeeds.
const calendarFeedsVersion = 10

// CalendarFeed is one of the Assistant's calendar feeds, as config.yaml
// keeps it.
type CalendarFeed struct {
	Name string `yaml:"name" json:"name"`
	// Host is the host the address names, in lower case.
	Host string `yaml:"host" json:"host"`
	// Set is when the address was last set, so setting it again reaches the
	// next turn.
	Set time.Time `yaml:"set" json:"set"`
}

// CalendarEntryID is a calendar feed's roster id.
func CalendarEntryID(name string) string { return calendarPrefix + name }

// CalendarKeychainAccount is the keychain account a calendar feed's address
// is kept under.
func CalendarKeychainAccount(agent, name string) string {
	return "calendar-feed:" + agent + ":" + name
}

// CalendarService names the service a feed's host belongs to, or "".
func CalendarService(host string) string {
	h := strings.ToLower(host)
	switch {
	case h == "calendar.google.com" || h == "www.google.com":
		return "Google Calendar"
	case h == "outlook.office365.com" || h == "outlook.office.com" || h == "outlook.live.com":
		return "Outlook"
	case strings.HasSuffix(h, ".icloud.com"):
		return "iCloud"
	case strings.HasSuffix(h, ".fastmail.com"):
		return "Fastmail"
	}
	return ""
}

// from is how a sentence names where the feed comes from.
func (f CalendarFeed) from() string {
	if s := CalendarService(f.Host); s != "" {
		return s + " (" + f.Host + ")"
	}
	return f.Host
}

// calendarEntries are the Assistant's calendar feeds' entries.
func calendarEntries(c Config) []RosterEntry {
	out := make([]RosterEntry, 0, len(c.CalendarFeeds))
	for _, f := range c.CalendarFeeds {
		out = append(out, RosterEntry{ID: CalendarEntryID(f.Name), Kind: KindCalendar, Name: f.Name + " (calendar feed)", GrantedBy: GrantedByYou, Line: calendarLine(f)})
	}
	return out
}

// calendarLine is a calendar feed's consent sentence: what the agent reads,
// and where the address is.
func calendarLine(f CalendarFeed) string {
	return "Reads your calendar feed " + f.Name + " from " + f.from() + ", read only: the titles, times, places, people and notes of the events it publishes, " +
		"which it can tell you, keep in its memory and use in what it writes; its tools fetch the feed from " + f.Host + " alone, " +
		"and the address stays in your keychain: the agent is never shown it"
}

// findCalendarFeed is the index of the feed named name in feeds, case not
// mattering, or -1.
func findCalendarFeed(feeds []CalendarFeed, name string) int {
	return slices.IndexFunc(feeds, func(f CalendarFeed) bool { return strings.EqualFold(f.Name, name) })
}

// addCalendarFeed records a calendar feed in c, on: a new one, or one whose
// address was set again. The address is kept in the keychain first.
func addCalendarFeed(a Agent, c *Config, f CalendarFeed) error {
	if !a.Builtin.ReadsCalendars() {
		return fmt.Errorf("the %s agent has no calendar feeds: the Assistant alone reads them", a.Name)
	}
	if err := spec.CheckCalendarFeedName(f.Name); err != nil {
		return err
	}
	if _, host, err := spec.CalendarFeedAddress("https://" + f.Host + "/"); err != nil || host != f.Host {
		return fmt.Errorf("calendar feed %s: %q is not a host name", f.Name, f.Host)
	}
	next := slices.Clone(c.CalendarFeeds)
	if i := findCalendarFeed(next, f.Name); i >= 0 {
		// Set again under its own name, as it was first given.
		f.Name = next[i].Name
		next[i] = f
	} else {
		if len(next) >= spec.MaxCalendarFeeds {
			return fmt.Errorf("the %s agent has %d calendar feeds, the most it keeps: remove one first", a.Name, spec.MaxCalendarFeeds)
		}
		next = append(next, f)
	}
	c.CalendarFeeds = next
	delete(c.Switches, CalendarEntryID(f.Name))
	return nil
}

// removeCalendarFeed takes a calendar feed out of c, by name, and returns
// it.
func removeCalendarFeed(a Agent, c *Config, name string) (CalendarFeed, error) {
	i := findCalendarFeed(c.CalendarFeeds, name)
	if i < 0 {
		return CalendarFeed{}, fmt.Errorf("the %s agent has no calendar feed %q", a.Name, name)
	}
	f := c.CalendarFeeds[i]
	c.CalendarFeeds = slices.Delete(slices.Clone(c.CalendarFeeds), i, i+1)
	delete(c.Switches, CalendarEntryID(f.Name))
	delete(c.Consented, CalendarEntryID(f.Name))
	return f, nil
}

// readCalendarFeeds reads the addresses of the feeds the roster has on from
// keys: every one must be there, an address the segment can carry and
// mask, on the host the configuration records.
func readCalendarFeeds(keys Secrets, a Agent, c Config, on func(string) bool) ([]localrun.CalendarFeed, error) {
	var out []localrun.CalendarFeed
	for _, f := range c.CalendarFeeds {
		if !on(CalendarEntryID(f.Name)) {
			continue
		}
		if keys == nil {
			return nil, errors.New("the Assistant has a calendar feed on, and this client cannot read the keychain")
		}
		again := fmt.Sprintf("set it again: agentry agent configure %s --calendar-feed %q", a.Name, f.Name)
		v, err := keys.Get(CalendarKeychainAccount(a.Name, f.Name))
		if err != nil {
			return nil, fmt.Errorf("the %s agent's calendar feed %s: %w (%s)", a.Name, f.Name, err, again)
		}
		if err := CheckSecretValue(v); err != nil {
			return nil, fmt.Errorf("the %s agent's calendar feed %s in the keychain: %w (%s)", a.Name, f.Name, err, again)
		}
		addr, host, err := spec.CalendarFeedAddress(v)
		if err != nil || host != f.Host {
			return nil, fmt.Errorf("the %s agent's calendar feed %s in the keychain is not an address on %s (%s)", a.Name, f.Name, f.Host, again)
		}
		out = append(out, localrun.CalendarFeed{Name: f.Name, Host: f.Host, URL: addr})
	}
	return out, nil
}

// calendarZone is the person's zone for the calendar tools: this
// computer's, or UTC when it names none the tools could load.
func calendarZone() string {
	z := schedule.LocalZone()
	if _, err := time.LoadLocation(z); err != nil || z == "Local" {
		return ""
	}
	return z
}

// CalendarFeedState is one calendar feed as a client shows it: never its
// address.
type CalendarFeedState struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Service names the service the host belongs to, or is empty.
	Service string    `json:"service,omitempty"`
	On      bool      `json:"on"`
	Set     time.Time `json:"set"`
	// Entry is the feed's roster id.
	Entry string `json:"entry"`
}

// CalendarFeeds are the agent's calendar feeds as a client shows them.
func CalendarFeeds(a Agent, c Config) []CalendarFeedState {
	if !a.Builtin.ReadsCalendars() {
		return nil
	}
	r := BuildRoster(a, c, nil)
	out := make([]CalendarFeedState, 0, len(c.CalendarFeeds))
	for _, f := range c.CalendarFeeds {
		e, _ := r.Entry(CalendarEntryID(f.Name))
		out = append(out, CalendarFeedState{Name: f.Name, Host: f.Host, Service: CalendarService(f.Host), On: e.On, Set: f.Set, Entry: e.ID})
	}
	return out
}

// ErrCalendarFeedUnreadable says a calendar feed's address answered with
// something that is not a calendar this build can read, or not at all.
var ErrCalendarFeedUnreadable = errors.New("the calendar feed could not be read")

// CalendarFeedReadable fetches a calendar feed's address once, as the tools
// server would, and reads it: it is how an address is checked before it is
// kept. It returns the calendar's own name, when it gives one, and its
// events' count. No error repeats the address. A test replaces it.
var CalendarFeedReadable = func(ctx context.Context, address string) (string, int, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", 0, fmt.Errorf("%w: its address is not a web address", ErrCalendarFeedUnreadable)
	}
	host := u.Hostname()
	ctx, cancel := context.WithTimeout(ctx, toolsrv.CalendarFetchTimeout)
	defer cancel()
	client := &http.Client{
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), host) || (r.URL.Port() != "" && r.URL.Port() != "443") {
				return fmt.Errorf("it redirected to %s, another host, which the Assistant would refuse", r.URL.Hostname())
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", 0, fmt.Errorf("%w: its address could not be requested", ErrCalendarFeedUnreadable)
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", 0, fmt.Errorf("%w: the fetch from %s failed: %v", ErrCalendarFeedUnreadable, host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return "", 0, fmt.Errorf("%w: %s answered HTTP %d (check the address is the calendar's published or secret iCal address, and that the calendar is still published)", ErrCalendarFeedUnreadable, host, resp.StatusCode)
	}
	cal, err := ical.Parse(resp.Body, toolsrv.MaxCalendarFeedBytes)
	if err != nil {
		return "", 0, fmt.Errorf("%w: what %s answered is not a calendar: %v", ErrCalendarFeedUnreadable, host, err)
	}
	return cal.Name, len(cal.Events), nil
}
