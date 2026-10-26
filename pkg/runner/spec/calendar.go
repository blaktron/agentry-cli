package spec

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A calendar feed is an iCalendar (ICS) address the person gives the
// Assistant under a name they choose: Google Calendar's secret address in
// iCal format, the ICS link Outlook gives a published calendar, an iCloud
// public calendar or any other. Whoever holds the address reads the
// calendar, so it is a secret: the person's keychain keeps it, the segment
// reads it, and the plan names only the feed, the host the address is on
// and the variable of the tools server's environment that holds it. The
// tools server alone is given it, and reaches the feed's host and no other.

// MaxCalendarFeeds bounds the Assistant's calendar feeds.
const MaxCalendarFeeds = 20

// MaxCalendarFeedName bounds a feed's name, in bytes.
const MaxCalendarFeedName = 64

// MaxCalendarFeedURL bounds a feed's address, in bytes.
const MaxCalendarFeedURL = 4096

// CalendarGrant is a persistent Assistant's calendar feeds that its roster
// has on (CapSummary.Calendar).
type CalendarGrant struct {
	Feeds []CalendarFeed `json:"feeds"`
	// TimeZone is the person's IANA zone, in which the tools answer.
	TimeZone string `json:"timeZone,omitempty"`
}

// CalendarFeed is one calendar feed as a plan names it. Its address is the
// segment's, never the plan's.
type CalendarFeed struct {
	Name string `json:"name"`
	// Host is the host the address names, in lower case: the one place the
	// tools server may fetch the feed from.
	Host string `json:"host"`
	// URLEnv is the variable of the tools server's environment that holds
	// the address (CalendarFeedEnv).
	URLEnv string `json:"urlEnv"`
}

// CalendarFeedEnv is the tools server's variable for the i-th feed's
// address (from 0). Its prefix is one a stored secret may not take.
func CalendarFeedEnv(i int) string { return "AGENTRY_CALENDAR_FEED_" + strconv.Itoa(i+1) }

var (
	calendarHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	calendarEnvRE  = regexp.MustCompile(`^AGENTRY_CALENDAR_FEED_[1-9][0-9]?$`)
)

// calendarHost says host is a host name in lower case: labels of letters,
// digits and hyphens, the last not all digits, so no address is one, and no
// name of the loopback (RFC 6761), which in a container is the container.
func calendarHost(host string) bool {
	if !calendarHostRE.MatchString(host) || strings.HasSuffix(host, ".localhost") {
		return false
	}
	tld := host[strings.LastIndexByte(host, '.')+1:]
	return strings.Trim(tld, "0123456789") != ""
}

// CheckCalendarFeedName holds a feed's name to printable text the person
// can tell apart: 1 to 64 bytes, no control or formatting character, no
// line break, and none of the characters a roster id or a list would read
// as its own.
func CheckCalendarFeedName(name string) error {
	switch {
	case strings.TrimSpace(name) != name || name == "":
		return errors.New("a calendar feed's name is 1 to 64 characters with no space at either end")
	case len(name) > MaxCalendarFeedName || !utf8.ValidString(name):
		return fmt.Errorf("a calendar feed's name is at most %d bytes of text", MaxCalendarFeedName)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`:,;"\/`+"`", r) {
			return fmt.Errorf("a calendar feed's name may not hold %q", r)
		}
	}
	return nil
}

// CalendarFeedAddress reads the address the person gives: https, or
// webcal (which calendar apps use for the same https address), with a host
// name and no user name, password or port but 443. It returns the address
// as fetched (webcal rewritten to https) and its host in lower case. An
// error never repeats the address, which is a secret.
func CalendarFeedAddress(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("the calendar feed's address is empty")
	}
	if len(raw) > MaxCalendarFeedURL {
		return "", "", fmt.Errorf("the calendar feed's address is longer than %d bytes", MaxCalendarFeedURL)
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return "", "", errors.New("the calendar feed's address holds a space or a control character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", errors.New("the calendar feed's address is not a web address")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "webcal", "webcals":
		u.Scheme = "https"
	case "http":
		return "", "", errors.New("the calendar feed's address is http: only https is read, so a feed's events cannot be read or changed on the way (most services give the same address with https)")
	default:
		return "", "", errors.New("the calendar feed's address is not an https or webcal address")
	}
	u.Scheme = "https"
	if u.User != nil {
		return "", "", errors.New("the calendar feed's address may not hold a user name or password")
	}
	if p := u.Port(); p != "" && p != "443" {
		return "", "", errors.New("the calendar feed's address names a port: only the https port, 443, is reached")
	}
	host := strings.ToLower(u.Hostname())
	if !calendarHost(host) {
		return "", "", errors.New("the calendar feed's address does not name a host by its name")
	}
	u.Host = host
	u.Fragment = ""
	return u.String(), host, nil
}

// CheckCalendarGrant holds a grant to well-formed feeds: names, hosts and
// variables of the feeds' form, none twice.
func CheckCalendarGrant(g *CalendarGrant) error {
	if g == nil {
		return nil
	}
	if len(g.Feeds) == 0 {
		return errors.New("a calendar grant names no feed")
	}
	if len(g.Feeds) > MaxCalendarFeeds {
		return fmt.Errorf("%d calendar feeds, at most %d", len(g.Feeds), MaxCalendarFeeds)
	}
	names, envs := map[string]bool{}, map[string]bool{}
	for _, f := range g.Feeds {
		if err := CheckCalendarFeedName(f.Name); err != nil {
			return err
		}
		if !calendarHost(f.Host) {
			return fmt.Errorf("calendar feed %q: %q is not a host name", f.Name, f.Host)
		}
		if !calendarEnvRE.MatchString(f.URLEnv) {
			return fmt.Errorf("calendar feed %q: %q is not a calendar feed variable", f.Name, f.URLEnv)
		}
		key := strings.ToLower(f.Name)
		if names[key] || envs[f.URLEnv] {
			return fmt.Errorf("calendar feed %q is named twice", f.Name)
		}
		names[key], envs[f.URLEnv] = true, true
	}
	return nil
}

// Hosts are the feeds' hosts, each once.
func (g *CalendarGrant) Hosts() []string {
	if g == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range g.Feeds {
		if !seen[f.Host] {
			seen[f.Host] = true
			out = append(out, f.Host)
		}
	}
	return out
}
