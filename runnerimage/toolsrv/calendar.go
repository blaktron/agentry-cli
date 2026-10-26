package toolsrv

// The calendar feeds' tools: a persistent Assistant reads the person's
// calendars from the iCalendar (ICS) addresses they keep, each under a name
// they chose. An address is a secret (whoever holds it reads the calendar),
// so it reaches this server alone, in a variable of its environment
// (CalendarFeed.URLEnv): the model names a feed and is never given its
// address, which is masked in every result as a stored secret is. A fetch
// reaches the feed's own host on 443 and nothing else, through the gateway's
// proxy, which allows those hosts alone; a redirect to another host is
// refused. A feed is read only: nothing here writes to a calendar.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/ical"
)

// The calendar tools.
const (
	ToolCalendarFeeds    = "calendar_feeds"
	ToolCalendarEvents   = "calendar_events"
	ToolCalendarFreeTime = "calendar_free_time"
)

// Bounds on the calendar tools.
const (
	// MaxCalendarFeedBytes is the largest feed read.
	MaxCalendarFeedBytes = 10 << 20
	// CalendarFetchTimeout bounds one feed's fetch.
	CalendarFetchTimeout = 30 * time.Second
	// CalendarCacheFor is how long a fetched feed is answered from before
	// it is fetched again.
	CalendarCacheFor = 5 * time.Minute
	// maxCalendarWindow is the longest window one call reads.
	maxCalendarWindow = 366 * 24 * time.Hour
	// maxCalendarOccurrences is the most events one answer lists.
	maxCalendarOccurrences = 400
	// maxCalendarNotes bounds an event's notes in an answer.
	maxCalendarNotes = 600
	// MaxCalendarFeeds is the most feeds a spec may name.
	MaxCalendarFeeds = 20
)

// Calendar is the calendar feeds' grant: the feeds a persistent Assistant
// has on, and the person's time zone, in which times are answered.
type Calendar struct {
	Feeds []CalendarFeed `json:"feeds,omitempty"`
	// TimeZone is the IANA name of the person's zone; empty is UTC.
	TimeZone string `json:"timeZone,omitempty"`
}

// CalendarFeed is one feed: the name the person gave it, the host its
// address names, and the variable of this server's environment that holds
// the address.
type CalendarFeed struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	URLEnv string `json:"urlEnv"`
}

var (
	calendarFeedEnvRE  = regexp.MustCompile(`^AGENTRY_CALENDAR_FEED_[1-9][0-9]?$`)
	calendarFeedHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// enabled says the grant names a feed.
func (c Calendar) enabled() bool { return len(c.Feeds) > 0 }

// urlEnvs are the variables holding the feeds' addresses.
func (c Calendar) urlEnvs() []string {
	out := make([]string, 0, len(c.Feeds))
	for _, f := range c.Feeds {
		out = append(out, f.URLEnv)
	}
	return out
}

// validateCalendar holds the grant to well-formed feeds: a name, a host
// name, a variable of the feeds' form, none twice, and a zone this server
// knows.
func validateCalendar(spec *Spec) error {
	c := spec.Calendar
	if len(c.Feeds) > MaxCalendarFeeds {
		return fmt.Errorf("calendar: %d feeds, at most %d", len(c.Feeds), MaxCalendarFeeds)
	}
	names, envs := map[string]bool{}, map[string]bool{}
	for _, f := range c.Feeds {
		key := strings.ToLower(f.Name)
		switch {
		case strings.TrimSpace(f.Name) == "" || len(f.Name) > 64:
			return fmt.Errorf("calendar: feed name %q must be 1 to 64 bytes", f.Name)
		case !calendarFeedHostRE.MatchString(f.Host) || strings.Trim(f.Host[strings.LastIndexByte(f.Host, '.')+1:], "0123456789") == "":
			return fmt.Errorf("calendar: feed %q: host %q is not a host name", f.Name, f.Host)
		case !calendarFeedEnvRE.MatchString(f.URLEnv):
			return fmt.Errorf("calendar: feed %q: urlEnv %q is not a calendar feed variable", f.Name, f.URLEnv)
		case names[key]:
			return fmt.Errorf("calendar: feed %q is named twice", f.Name)
		case envs[f.URLEnv]:
			return fmt.Errorf("calendar: urlEnv %q is used twice", f.URLEnv)
		}
		names[key], envs[f.URLEnv] = true, true
	}
	if c.TimeZone != "" {
		if _, err := time.LoadLocation(c.TimeZone); err != nil || c.TimeZone == "Local" {
			return fmt.Errorf("calendar: timeZone %q is not an IANA zone", c.TimeZone)
		}
	}
	return nil
}

// calendarCache holds each feed's last fetch for CalendarCacheFor.
type calendarCache struct {
	mu      sync.Mutex
	entries map[string]calendarEntry
}

type calendarEntry struct {
	cal *ical.Calendar
	at  time.Time
}

// calendarLocation is the zone answers are given in: the call's, else the
// grant's, else UTC.
func (s *server) calendarLocation(name string) (*time.Location, error) {
	if name == "" {
		name = s.spec.Calendar.TimeZone
	}
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, fmt.Errorf("time_zone %q is not an IANA zone, such as Europe/Paris", name)
	}
	return loc, nil
}

// calendarFeedsFor are the feeds a call names: one by its name, or all.
func (s *server) calendarFeedsFor(name string) ([]CalendarFeed, error) {
	if name == "" {
		return s.spec.Calendar.Feeds, nil
	}
	for _, f := range s.spec.Calendar.Feeds {
		if strings.EqualFold(f.Name, name) {
			return []CalendarFeed{f}, nil
		}
	}
	names := make([]string, 0, len(s.spec.Calendar.Feeds))
	for _, f := range s.spec.Calendar.Feeds {
		names = append(names, f.Name)
	}
	return nil, fmt.Errorf("no calendar feed is named %q (the feeds: %s)", name, strings.Join(names, ", "))
}

// calendarNow is the clock the calendar tools read; tests set it.
var calendarNow = time.Now

// calendarTransport is how a feed is fetched; tests replace it.
var calendarTransport = func() http.RoundTripper { return fetchTransport() }

// fetchCalendar returns a feed's calendar, from the cache while it is
// fresh, and when it was fetched. An error never names the address.
func (s *server) fetchCalendar(f CalendarFeed) (*ical.Calendar, time.Time, error) {
	s.calendars.mu.Lock()
	if e, ok := s.calendars.entries[f.Name]; ok && calendarNow().Sub(e.at) < CalendarCacheFor {
		s.calendars.mu.Unlock()
		return e.cal, e.at, nil
	}
	s.calendars.mu.Unlock()

	raw := os.Getenv(f.URLEnv)
	if raw == "" {
		return nil, time.Time{}, errors.New("its address is not set in this session; set it again in the Assistant's configuration")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), f.Host) || (u.Port() != "" && u.Port() != "443") {
		return nil, time.Time{}, errors.New("its address is not an https address on " + f.Host + "; set it again in the Assistant's configuration")
	}
	client := &http.Client{
		Timeout:   CalendarFetchTimeout,
		Transport: calendarTransport(),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), f.Host) || (r.URL.Port() != "" && r.URL.Port() != "443") {
				return fmt.Errorf("it redirected to %s, another host, which is refused", r.URL.Hostname())
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, time.Time{}, errors.New("its address could not be requested")
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, time.Time{}, fmt.Errorf("the fetch failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
		resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, time.Time{}, fmt.Errorf("%s answered HTTP %d: the address may have been reset or unpublished; the person can publish it again and set it in the Assistant's configuration", f.Host, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, time.Time{}, fmt.Errorf("%s answered HTTP %d", f.Host, resp.StatusCode)
	}
	cal, err := ical.Parse(resp.Body, MaxCalendarFeedBytes)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("it could not be read as a calendar: %v", err)
	}
	at := calendarNow()
	s.calendars.mu.Lock()
	if s.calendars.entries == nil {
		s.calendars.entries = map[string]calendarEntry{}
	}
	s.calendars.entries[f.Name] = calendarEntry{cal: cal, at: at}
	s.calendars.mu.Unlock()
	return cal, at, nil
}

// calendarService names the service a feed's host belongs to, or "".
func calendarService(host string) string {
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

func calendarFeedsDescription() string {
	return "List the person's calendar feeds by name: the service each comes from, and when this session last fetched it. " +
		"The feeds are read only: you can read what is on them, never add, move or accept an event. Their addresses are never shown to you."
}

func calendarEventsDescription() string {
	return "Read the events on the person's calendar feeds that fall between from and to, with repeating events expanded, in the person's time zone. " +
		"Read only. A feed is fetched at most every " + fmt.Sprint(int(CalendarCacheFor/time.Minute)) + " minutes, and its publisher may update it later than the calendar itself: " +
		"each answer says when every feed was fetched, so say how current it is when it matters."
}

func calendarFreeTimeDescription() string {
	return "Find the free time on the person's calendar feeds between from and to: the spans no busy event covers, each at least min_minutes long, " +
		"within the hours day_start to day_end of each day (09:00 to 17:00 unless given), weekdays only unless include_weekends is true. " +
		"An event marked free (transparent) does not count as busy. Read only."
}

func calendarFeedProp() map[string]any {
	return strProp("one feed's name, as calendar_feeds lists it; every feed when omitted")
}

func calendarWindowProps() map[string]any {
	return map[string]any{
		"feed":      calendarFeedProp(),
		"from":      strProp("start of the window: a date (2026-10-27, the start of that day), a local date and time (2026-10-27T09:00) or an RFC 3339 time; the start of today when omitted"),
		"to":        strProp("end of the window, in the same forms (a date means the start of that day); 7 days after from when omitted; at most 366 days after from"),
		"time_zone": strProp("an IANA zone to answer in, such as Europe/Paris; the person's own when omitted"),
	}
}

func calendarEventsSchema() map[string]any {
	p := calendarWindowProps()
	p["query"] = strProp("only events whose title, location or notes contain this text (case does not matter)")
	return objSchema(p)
}

func calendarFreeTimeSchema() map[string]any {
	p := calendarWindowProps()
	p["min_minutes"] = intProp("the shortest free span to list, in minutes (default 30)")
	p["day_start"] = strProp("start of each day's hours, HH:MM (default 09:00)")
	p["day_end"] = strProp("end of each day's hours, HH:MM (default 17:00)")
	p["include_weekends"] = boolProp("include Saturdays and Sundays (default false)")
	return objSchema(p)
}

// parseCalendarTime reads a date, a local date and time, or an RFC 3339
// time.
func parseCalendarTime(v string, loc *time.Location) (time.Time, error) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.In(loc), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-10-27), a local date and time (2026-10-27T09:00) or an RFC 3339 time", v)
}

// calendarWindow is a call's window, checked.
func calendarWindow(from, to string, loc *time.Location) (time.Time, time.Time, error) {
	now := calendarNow().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if from != "" {
		t, err := parseCalendarTime(from, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from: %v", err)
		}
		start = t
	}
	end := start.AddDate(0, 0, 7)
	if to != "" {
		t, err := parseCalendarTime(to, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to: %v", err)
		}
		end = t
	}
	switch {
	case !end.After(start):
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	case end.Sub(start) > maxCalendarWindow:
		return time.Time{}, time.Time{}, errors.New("the window is longer than 366 days; ask for a shorter one")
	}
	return start, end, nil
}

// feedRead is one feed's part of an answer.
type feedRead struct {
	feed     CalendarFeed
	at       time.Time
	ex       ical.Expansion
	busyOnly bool
	err      error
}

// readFeeds expands each named feed over the window.
func (s *server) readFeeds(feeds []CalendarFeed, from, to time.Time, loc *time.Location) []feedRead {
	out := make([]feedRead, 0, len(feeds))
	for _, f := range feeds {
		cal, at, err := s.fetchCalendar(f)
		r := feedRead{feed: f, at: at, err: err}
		if err == nil {
			r.ex = cal.Expand(from, to, loc)
			r.busyOnly = cal.BusyOnly()
		}
		out = append(out, r)
	}
	return out
}

// feedsHeader says when each feed was fetched, which could not be read, and
// what was read inexactly.
func feedsHeader(reads []feedRead, loc *time.Location) (string, bool) {
	var b strings.Builder
	ok := false
	for _, r := range reads {
		if r.err != nil {
			fmt.Fprintf(&b, "Feed %s could not be read: %v\n", r.feed.Name, r.err)
			continue
		}
		ok = true
		fmt.Fprintf(&b, "Feed %s: fetched %s", r.feed.Name, r.at.In(loc).Format("Mon 2 Jan 15:04"))
		if r.busyOnly {
			b.WriteString("; it is published as free/busy only, so its events have no titles or details")
		}
		b.WriteString("\n")
		for _, n := range r.ex.Notes {
			fmt.Fprintf(&b, "Feed %s: %s\n", r.feed.Name, n)
		}
	}
	fmt.Fprintf(&b, "Times are in %s.\n", loc)
	return b.String(), ok
}

func (s *server) toolCalendarFeeds(raw json.RawMessage) (string, bool) {
	if err := unmarshalArgs(raw, &struct{}{}); err != nil {
		return "calendar_feeds: " + err.Error(), true
	}
	loc, _ := s.calendarLocation("")
	var b strings.Builder
	b.WriteString("The person's calendar feeds (read only):\n")
	s.calendars.mu.Lock()
	defer s.calendars.mu.Unlock()
	for _, f := range s.spec.Calendar.Feeds {
		fmt.Fprintf(&b, "- %s", f.Name)
		if svc := calendarService(f.Host); svc != "" {
			fmt.Fprintf(&b, " (%s)", svc)
		} else {
			fmt.Fprintf(&b, " (from %s)", f.Host)
		}
		if e, ok := s.calendars.entries[f.Name]; ok {
			fmt.Fprintf(&b, ": last fetched %s", e.at.In(loc).Format("Mon 2 Jan 15:04"))
			if e.cal.Name != "" {
				fmt.Fprintf(&b, ", the calendar %q", e.cal.Name)
			}
		} else {
			b.WriteString(": not fetched yet in this session")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "The person's time zone is %s.\n", loc)
	return s.scrubSecrets(b.String()), false
}

func (s *server) toolCalendarEvents(raw json.RawMessage) (string, bool) {
	var a struct {
		Feed     string `json:"feed"`
		From     string `json:"from"`
		To       string `json:"to"`
		TimeZone string `json:"time_zone"`
		Query    string `json:"query"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "calendar_events: " + err.Error(), true
	}
	loc, err := s.calendarLocation(a.TimeZone)
	if err != nil {
		return "calendar_events: " + err.Error(), true
	}
	from, to, err := calendarWindow(a.From, a.To, loc)
	if err != nil {
		return "calendar_events: " + err.Error(), true
	}
	feeds, err := s.calendarFeedsFor(a.Feed)
	if err != nil {
		return "calendar_events: " + err.Error(), true
	}
	reads := s.readFeeds(feeds, from, to, loc)
	head, ok := feedsHeader(reads, loc)
	if !ok {
		return s.scrubSecrets("calendar_events: " + head), true
	}
	type item struct {
		o    ical.Occurrence
		feed string
	}
	var items []item
	q := strings.ToLower(strings.TrimSpace(a.Query))
	for _, r := range reads {
		for _, o := range r.ex.Occurrences {
			if q != "" && !strings.Contains(strings.ToLower(o.Summary+"\n"+o.Location+"\n"+o.Description), q) {
				continue
			}
			items = append(items, item{o, r.feed.Name})
		}
	}
	slices.SortStableFunc(items, func(x, y item) int { return x.o.Start.Compare(y.o.Start) })
	var b strings.Builder
	b.WriteString(head)
	fmt.Fprintf(&b, "Events from %s to %s", from.Format("Mon 2 Jan 2006 15:04"), to.Format("Mon 2 Jan 2006 15:04"))
	if q != "" {
		fmt.Fprintf(&b, " matching %q", a.Query)
	}
	fmt.Fprintf(&b, ": %d.\n", len(items))
	if len(items) > maxCalendarOccurrences {
		fmt.Fprintf(&b, "Only the first %d are listed; ask for a shorter window for the rest.\n", maxCalendarOccurrences)
		items = items[:maxCalendarOccurrences]
	}
	day := ""
	multi := len(feeds) > 1
	for _, it := range items {
		o := it.o
		start, end := o.Start.In(loc), o.End.In(loc)
		if d := start.Format("Mon 2 Jan 2006"); d != day {
			day = d
			fmt.Fprintf(&b, "\n%s\n", d)
		}
		b.WriteString("- ")
		switch {
		case o.AllDay:
			days := int(o.End.Sub(o.Start).Round(time.Hour) / (24 * time.Hour))
			if days > 1 {
				fmt.Fprintf(&b, "all day, %d days to %s", days, end.AddDate(0, 0, -1).Format("Mon 2 Jan"))
			} else {
				b.WriteString("all day")
			}
		case end.Equal(start):
			b.WriteString(start.Format("15:04"))
		case end.Format("2006-01-02") != start.Format("2006-01-02"):
			fmt.Fprintf(&b, "%s to %s", start.Format("15:04"), end.Format("Mon 2 Jan 15:04"))
		default:
			fmt.Fprintf(&b, "%s–%s", start.Format("15:04"), end.Format("15:04"))
		}
		title := o.Summary
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&b, " %s", flat(title))
		var tags []string
		if multi {
			tags = append(tags, it.feed)
		}
		if o.Recurring {
			tags = append(tags, "repeats")
		}
		if o.Status == "TENTATIVE" {
			tags = append(tags, "tentative")
		}
		if !o.Busy {
			tags = append(tags, "shown as free")
		}
		if len(tags) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(tags, ", "))
		}
		b.WriteString("\n")
		if o.Location != "" {
			fmt.Fprintf(&b, "  Where: %s\n", flat(o.Location))
		}
		if o.Organizer != "" {
			fmt.Fprintf(&b, "  Organizer: %s\n", flat(o.Organizer))
		}
		if o.URL != "" {
			fmt.Fprintf(&b, "  Link: %s\n", flat(o.URL))
		}
		if notes := strings.TrimSpace(o.Description); notes != "" {
			if len(notes) > maxCalendarNotes {
				notes = strings.ToValidUTF8(notes[:maxCalendarNotes], "") + "…"
			}
			fmt.Fprintf(&b, "  Notes: %s\n", strings.ReplaceAll(notes, "\n", "\n    "))
		}
	}
	return s.scrubSecrets(b.String()), false
}

// flat is s with its line breaks and runs of space as single spaces.
func flat(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseClock reads HH:MM.
func parseClock(v string, def int) (int, error) {
	if v == "" {
		return def, nil
	}
	t, err := time.Parse("15:04", strings.TrimSpace(v))
	if err != nil {
		if strings.TrimSpace(v) == "24:00" {
			return 24 * 60, nil
		}
		return 0, fmt.Errorf("%q is not a time of day, HH:MM", v)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (s *server) toolCalendarFreeTime(raw json.RawMessage) (string, bool) {
	var a struct {
		Feed            string `json:"feed"`
		From            string `json:"from"`
		To              string `json:"to"`
		TimeZone        string `json:"time_zone"`
		MinMinutes      int    `json:"min_minutes"`
		DayStart        string `json:"day_start"`
		DayEnd          string `json:"day_end"`
		IncludeWeekends bool   `json:"include_weekends"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "calendar_free_time: " + err.Error(), true
	}
	loc, err := s.calendarLocation(a.TimeZone)
	if err != nil {
		return "calendar_free_time: " + err.Error(), true
	}
	from, to, err := calendarWindow(a.From, a.To, loc)
	if err != nil {
		return "calendar_free_time: " + err.Error(), true
	}
	if a.MinMinutes == 0 {
		a.MinMinutes = 30
	}
	if a.MinMinutes < 1 || a.MinMinutes > 24*60 {
		return "calendar_free_time: min_minutes must be 1 to 1440", true
	}
	dayStart, err := parseClock(a.DayStart, 9*60)
	if err != nil {
		return "calendar_free_time: day_start: " + err.Error(), true
	}
	dayEnd, err := parseClock(a.DayEnd, 17*60)
	if err != nil {
		return "calendar_free_time: day_end: " + err.Error(), true
	}
	if dayEnd <= dayStart {
		return "calendar_free_time: day_end must be after day_start", true
	}
	feeds, err := s.calendarFeedsFor(a.Feed)
	if err != nil {
		return "calendar_free_time: " + err.Error(), true
	}
	reads := s.readFeeds(feeds, from, to, loc)
	head, ok := feedsHeader(reads, loc)
	if !ok {
		return s.scrubSecrets("calendar_free_time: " + head), true
	}
	var occs []ical.Occurrence
	for _, r := range reads {
		occs = append(occs, r.ex.Occurrences...)
	}
	var windows []ical.Slot
	first := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	for d := first; d.Before(to); d = d.AddDate(0, 0, 1) {
		if !a.IncludeWeekends && (d.Weekday() == time.Saturday || d.Weekday() == time.Sunday) {
			continue
		}
		ws := time.Date(d.Year(), d.Month(), d.Day(), 0, dayStart, 0, 0, loc)
		we := time.Date(d.Year(), d.Month(), d.Day(), 0, dayEnd, 0, 0, loc)
		if ws.Before(from) {
			ws = from
		}
		if we.After(to) {
			we = to
		}
		if we.After(ws) {
			windows = append(windows, ical.Slot{Start: ws, End: we})
		}
	}
	free := ical.FreeSlots(occs, windows, time.Duration(a.MinMinutes)*time.Minute)
	var b strings.Builder
	b.WriteString(head)
	fmt.Fprintf(&b, "Free spans of at least %d minutes between %s and %s, %s to %s each %s:",
		a.MinMinutes, from.Format("Mon 2 Jan 2006 15:04"), to.Format("Mon 2 Jan 2006 15:04"),
		fmt.Sprintf("%02d:%02d", dayStart/60, dayStart%60), fmt.Sprintf("%02d:%02d", dayEnd/60, dayEnd%60),
		map[bool]string{true: "day", false: "weekday"}[a.IncludeWeekends])
	if len(free) == 0 {
		b.WriteString(" none.\n")
		return s.scrubSecrets(b.String()), false
	}
	b.WriteString("\n")
	day := ""
	for _, f := range free {
		st, en := f.Start.In(loc), f.End.In(loc)
		if d := st.Format("Mon 2 Jan 2006"); d != day {
			day = d
			fmt.Fprintf(&b, "\n%s\n", d)
		}
		mins := int(en.Sub(st) / time.Minute)
		fmt.Fprintf(&b, "- %s–%s (%dh%02d)\n", st.Format("15:04"), en.Format("15:04"), mins/60, mins%60)
	}
	return s.scrubSecrets(b.String()), false
}
