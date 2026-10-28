package persistent

// The Assistant's calendar feeds: an iCalendar (ICS) address the person
// gives it under a name they choose, each a roster entry calendar:<name>.
// Whoever holds the address reads the calendar, so it is kept in the
// system keychain under calendar-feed:<agent>:<name>, never in config.yaml,
// which records the name, the host the address is on and when it was last
// set. While the entry is on, the segment gives the address to the tools
// server's calendar tools alone, which fetch it from that host and no other
// (spec.CalendarGrant); the agent names the feed and is never shown it.
//
// Some addresses answer only a signed-in browser: a Google Workspace
// calendar whose secret address its administrator turned off redirects to
// Google's sign-in page. Such a feed is signed in through a browser window
// (SignInCalendarFeed), and the cookies that browser would send to the
// feed's host are kept in the keychain beside the address, under
// calendar-session:<agent>:<name>:<n>, split so each entry fits a Windows
// credential. The tools server sends them to that host alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/browsersignin"
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
// CalendarFeeds, and calendarSignInsVersion the one that added their
// SignedIn.
const (
	calendarFeedsVersion   = 10
	calendarSignInsVersion = 11
)

// CalendarFeed is one of the Assistant's calendar feeds, as config.yaml
// keeps it.
type CalendarFeed struct {
	Name string `yaml:"name" json:"name"`
	// Host is the host the address names, in lower case.
	Host string `yaml:"host" json:"host"`
	// Set is when the address was last set, so setting it again reaches the
	// next turn.
	Set time.Time `yaml:"set" json:"set"`
	// SignedIn says a browser sign-in is kept for the feed in the keychain
	// (CalendarSessionAccount).
	SignedIn bool `yaml:"signedIn,omitempty" json:"signedIn,omitempty"`
}

// CalendarEntryID is a calendar feed's roster id.
func CalendarEntryID(name string) string { return calendarPrefix + name }

// CalendarKeychainAccount is the keychain account a calendar feed's address
// is kept under.
func CalendarKeychainAccount(agent, name string) string {
	return "calendar-feed:" + agent + ":" + name
}

// CalendarSessionAccount is the keychain account of the n-th part (from 1)
// of a calendar feed's browser sign-in.
func CalendarSessionAccount(agent, name string, n int) string {
	return "calendar-session:" + agent + ":" + name + ":" + strconv.Itoa(n)
}

// calendarSessionPart is the most one keychain entry of a sign-in holds,
// under the 2,560 bytes a Windows credential holds; calendarSessionParts
// is the most entries one takes.
const (
	calendarSessionPart  = 2000
	calendarSessionParts = 8
)

// KeepCalendarSession keeps a feed's browser sign-in (SignInCalendarFeed's
// session, a list of cookies) in the keychain, in as many parts as it needs, and removes any part a
// longer one before it left.
func KeepCalendarSession(keys Secrets, agent, name, session string) error {
	if err := CheckSecretValue(session); err != nil {
		return fmt.Errorf("the sign-in cannot be kept in the keychain: %w", err)
	}
	if len(session) > calendarSessionPart*calendarSessionParts {
		return fmt.Errorf("the sign-in is longer than the %d KB kept for one", calendarSessionPart*calendarSessionParts/1000)
	}
	n := 0
	for rest := session; rest != ""; {
		part := rest[:min(len(rest), calendarSessionPart)]
		rest = rest[len(part):]
		n++
		if err := keys.Set(CalendarSessionAccount(agent, name, n), part); err != nil {
			return err
		}
	}
	for i := n + 1; i <= calendarSessionParts; i++ {
		if err := keys.Delete(CalendarSessionAccount(agent, name, i)); err != nil {
			return err
		}
	}
	return nil
}

// CalendarSession reads a feed's browser sign-in back from the keychain.
func CalendarSession(keys Secrets, agent, name string) (string, error) {
	var b strings.Builder
	for i := 1; i <= calendarSessionParts; i++ {
		v, err := keys.Get(CalendarSessionAccount(agent, name, i))
		if err != nil {
			if i == 1 {
				return "", err
			}
			break
		}
		b.WriteString(v)
		if len(v) < calendarSessionPart {
			break
		}
	}
	return b.String(), nil
}

// ForgetCalendarSession removes a feed's browser sign-in from the keychain.
func ForgetCalendarSession(keys Secrets, agent, name string) error {
	for i := 1; i <= calendarSessionParts; i++ {
		if err := keys.Delete(CalendarSessionAccount(agent, name, i)); err != nil {
			return err
		}
	}
	return nil
}

// ForgetCalendarFeed removes a feed's address and its browser sign-in from
// the keychain.
func ForgetCalendarFeed(keys Secrets, agent, name string) error {
	if err := keys.Delete(CalendarKeychainAccount(agent, name)); err != nil {
		return err
	}
	return ForgetCalendarSession(keys, agent, name)
}

// CalendarService names the service a feed's host belongs to, or "".
func CalendarService(host string) string { return toolsrv.CalendarService(host) }

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
	kept := "and the address stays in your keychain: the agent is never shown it"
	if f.SignedIn {
		kept = "signed in as you: with the cookies of your sign-in to " + f.Host + " that the browser window kept, which are your account's sign-in to that site, not a grant for this calendar alone, " +
			"and which its tools send to " + f.Host + " and nowhere else; the address and the cookies stay in your keychain: the agent is never shown them"
	}
	return "Reads your calendar feed " + f.Name + " from " + f.from() + ", read only: the titles, times, places, people and notes of the events it publishes, " +
		"which it can tell you, keep in its memory and use in what it writes; its tools fetch the feed from " + f.Host + " alone, " + kept
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
		feed := localrun.CalendarFeed{Name: f.Name, Host: f.Host, URL: addr}
		if f.SignedIn {
			signIn := fmt.Sprintf("sign in again: agentry agent configure %s --calendar-feed %q --calendar-sign-in", a.Name, f.Name)
			session, err := CalendarSession(keys, a.Name, f.Name)
			if err != nil {
				return nil, fmt.Errorf("the %s agent's calendar feed %s: its sign-in: %w (%s)", a.Name, f.Name, err, signIn)
			}
			if err := CheckSecretValue(session); err != nil {
				return nil, fmt.Errorf("the %s agent's calendar feed %s: its sign-in in the keychain: %w (%s)", a.Name, f.Name, err, signIn)
			}
			feed.Session = session
		}
		out = append(out, feed)
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
	// SignedIn says a browser sign-in is kept for the feed.
	SignedIn bool `json:"signedIn,omitempty"`
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
		out = append(out, CalendarFeedState{Name: f.Name, Host: f.Host, Service: CalendarService(f.Host), On: e.On, Set: f.Set, SignedIn: f.SignedIn, Entry: e.ID})
	}
	return out
}

// ErrCalendarFeedUnreadable says a calendar feed's address answered with
// something that is not a calendar this build can read, or not at all.
var ErrCalendarFeedUnreadable = errors.New("the calendar feed could not be read")

// ErrCalendarFeedSignIn says a calendar feed's address sent the fetch to a
// sign-in page: it answers a signed-in browser alone, and is read after a
// browser sign-in (SignInCalendarFeed). It wraps ErrCalendarFeedUnreadable.
var ErrCalendarFeedSignIn = fmt.Errorf("%w: it asks for a sign-in", ErrCalendarFeedUnreadable)

// calendarCheckTransport is CalendarFeedReadable's transport: the default,
// but in a test.
var calendarCheckTransport http.RoundTripper

// CalendarFeedReadable fetches a calendar feed's address once, as the tools
// server would, and reads it: it is how an address is checked before it is
// kept. It returns the calendar's own name, when it gives one, and its
// events' count. No error repeats the address. A test replaces it.
var CalendarFeedReadable = func(ctx context.Context, address string) (string, int, error) {
	return CalendarFeedReadableWith(ctx, address, "")
}

// CalendarFeedReadableWith is CalendarFeedReadable, sending a browser
// sign-in (SignInCalendarFeed's session, or empty for none) with the fetch.
// A redirect to a sign-in page is ErrCalendarFeedSignIn. No error repeats
// the address or the sign-in.
func CalendarFeedReadableWith(ctx context.Context, address, session string) (string, int, error) {
	header := ""
	if session != "" {
		h, err := toolsrv.CalendarSessionHeader(session)
		if err != nil {
			return "", 0, fmt.Errorf("%w: its sign-in: %v", ErrCalendarFeedUnreadable, err)
		}
		header = h
	}
	return calendarFeedReadable(ctx, address, header)
}

// calendarFeedReadable fetches and reads address, sending header as its
// Cookie header when it is set.
func calendarFeedReadable(ctx context.Context, address, header string) (string, int, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", 0, fmt.Errorf("%w: its address is not a web address", ErrCalendarFeedUnreadable)
	}
	host := u.Hostname()
	ctx, cancel := context.WithTimeout(ctx, toolsrv.CalendarFetchTimeout)
	defer cancel()
	client := &http.Client{
		Transport: calendarCheckTransport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), host) || (r.URL.Port() != "" && r.URL.Port() != "443") {
				if toolsrv.CalendarSignInHost(r.URL.Hostname()) {
					return fmt.Errorf("%w at %s", ErrCalendarFeedSignIn, r.URL.Hostname())
				}
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
	if header != "" {
		req.Header.Set("Cookie", header)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, ErrCalendarFeedSignIn) {
			return "", 0, err
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

// CalendarSignInTimeout bounds a browser sign-in.
const CalendarSignInTimeout = 10 * time.Minute

// calendarSignIn is SignInCalendarFeed's browser: the real one, but in a
// test.
var calendarSignIn = browsersignin.Run

// SignInCalendarFeed opens a browser window at a calendar feed's address,
// where the person signs in to the account the calendar belongs to, and,
// once the window has closed, returns the browser's sign-in (the cookies it
// sends to the address, as KeepCalendarSession keeps them) if the address
// answers with a calendar, with that calendar's name and events' count.
// progress, when set, is told what it waits for. No error repeats the
// address or the sign-in.
func SignInCalendarFeed(ctx context.Context, address string, progress func(string)) (session, calName string, events int, err error) {
	ctx, cancel := context.WithTimeout(ctx, CalendarSignInTimeout)
	defer cancel()
	var lastErr error
	cookies, err := calendarSignIn(ctx, browsersignin.Options{
		URL:      address,
		Progress: progress,
		Ready: func(ctx context.Context, header string) (bool, error) {
			n, c, err := calendarFeedReadable(ctx, address, header)
			if err != nil {
				// Not signed in, or signed in to an account that cannot
				// read this calendar.
				lastErr = err
				return false, nil
			}
			calName, events = n, c
			return true, nil
		},
	})
	if err != nil {
		if errors.Is(err, browsersignin.ErrClosed) && lastErr != nil && !errors.Is(lastErr, ErrCalendarFeedSignIn) {
			return "", "", 0, fmt.Errorf("%w; signed in, the calendar still could not be read: %v", err, lastErr)
		}
		return "", "", 0, err
	}
	kept := make([]toolsrv.CalendarSessionCookie, 0, len(cookies))
	for _, c := range cookies {
		kept = append(kept, toolsrv.CalendarSessionCookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return "", "", 0, err
	}
	return string(b), calName, events, nil
}
