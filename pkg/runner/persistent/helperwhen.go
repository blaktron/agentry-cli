package persistent

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// When is when a schedule a helper tool makes runs.
type When struct {
	// Kind is once, daily, weekdays, weekly or monthly.
	Kind string `json:"kind"`
	// Date is a once schedule's day (YYYY-MM-DD).
	Date string `json:"date,omitempty"`
	// Time is HH:MM, 24-hour.
	Time string `json:"time"`
	// Days are a weekly schedule's (mon … sun).
	Days     []string `json:"days,omitempty"`
	MonthDay int      `json:"monthDay,omitempty"`
}

var (
	whenHHMM = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	whenYMD  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Args are the agentry schedule add flags that say w.
func (w When) Args() ([]string, error) {
	if !whenHHMM.MatchString(w.Time) {
		return nil, fmt.Errorf("the time %q is not HH:MM", w.Time)
	}
	switch w.Kind {
	case "once":
		if !whenYMD.MatchString(w.Date) {
			return nil, fmt.Errorf("the date %q is not YYYY-MM-DD", w.Date)
		}
		return []string{"--once", w.Date + " " + w.Time}, nil
	case "daily":
		return []string{"--daily", w.Time}, nil
	case "weekdays":
		return []string{"--weekdays", w.Time}, nil
	case "weekly":
		days, err := schedule.ParseDays(strings.Join(w.Days, ","))
		if err != nil {
			return nil, err
		}
		return []string{"--weekly", strings.Join(days, ","), "--at", w.Time}, nil
	case "monthly":
		if w.MonthDay < 1 || w.MonthDay > 31 {
			return nil, fmt.Errorf("the day of the month %d is not 1 to 31", w.MonthDay)
		}
		return []string{"--monthly", strconv.Itoa(w.MonthDay), "--at", w.Time}, nil
	}
	return nil, fmt.Errorf("unknown repeat %q", w.Kind)
}

// Describe is when w runs, in a phrase.
func (w When) Describe() string {
	switch w.Kind {
	case "once":
		return "once, on " + w.Date + " at " + w.Time
	case "daily":
		return "every day at " + w.Time
	case "weekdays":
		return "every weekday at " + w.Time
	case "weekly":
		return "every " + strings.Join(w.Days, ", ") + " at " + w.Time
	case "monthly":
		return fmt.Sprintf("on day %d of every month at %s", w.MonthDay, w.Time)
	}
	return w.Kind + " at " + w.Time
}

// The ways a schedule's time can be said: "every weekday at 09:00",
// "every day at 7pm", "every monday and thursday at 18:30", "monthly on the
// 3rd at 08:00", "once on 2026-11-02 at 10:00", "tomorrow at 9".
var (
	whenTime     = regexp.MustCompile(`(?:\bat\s+)?\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b`)
	whenDate     = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	whenMonthDay = regexp.MustCompile(`\b(?:day\s+)?(\d{1,2})(?:st|nd|rd|th)?\b`)
	whenDays     = map[string]string{
		"monday": "mon", "mondays": "mon", "mon": "mon", "tuesday": "tue", "tuesdays": "tue", "tue": "tue", "tues": "tue",
		"wednesday": "wed", "wednesdays": "wed", "wed": "wed", "thursday": "thu", "thursdays": "thu", "thu": "thu", "thurs": "thu",
		"friday": "fri", "fridays": "fri", "fri": "fri", "saturday": "sat", "saturdays": "sat", "sat": "sat",
		"sunday": "sun", "sundays": "sun", "sun": "sun",
	}
	dayOrder = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

const whenForms = `say when as one of: "every day at 09:00", "every weekday at 09:00", "every monday and thursday at 18:30", "monthly on the 3rd at 08:00", "once on 2026-11-02 at 10:00", "tomorrow at 9am"`

// ParseWhen reads when a schedule runs, in the words whenForms lists.
func ParseWhen(s string, now time.Time) (When, error) {
	t := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(s, ",", " , ")), " "))
	if t == "" {
		return When{}, errors.New(whenForms)
	}
	w := When{}
	rest := t
	if d := whenDate.FindStringSubmatch(t); d != nil {
		w.Kind, w.Date = "once", d[1]
		rest = strings.Replace(rest, d[1], " ", 1)
	} else if strings.Contains(t, "tomorrow") {
		w.Kind, w.Date = "once", now.AddDate(0, 0, 1).Format("2006-01-02")
	} else if strings.Contains(t, "today") {
		w.Kind, w.Date = "once", now.Format("2006-01-02")
	}
	if w.Kind == "" {
		switch {
		case strings.Contains(t, "weekday"):
			w.Kind = "weekdays"
		case strings.Contains(t, "month"):
			w.Kind = "monthly"
		case strings.Contains(t, "every day") || strings.Contains(t, "daily") || strings.Contains(t, "each day"):
			w.Kind = "daily"
		default:
			for _, word := range strings.Fields(t) {
				if d, ok := whenDays[word]; ok && !slices.Contains(w.Days, d) {
					w.Days = append(w.Days, d)
				}
			}
			if len(w.Days) == 0 {
				return When{}, errors.New(whenForms)
			}
			w.Kind = "weekly"
			slices.SortFunc(w.Days, func(a, b string) int { return slices.Index(dayOrder, a) - slices.Index(dayOrder, b) })
		}
	}
	// The time is the one after "at", else the last number that reads as one.
	timeText := rest
	if i := strings.LastIndex(rest, " at "); i >= 0 {
		timeText = rest[i:]
	}
	if w.Kind == "monthly" {
		i := strings.LastIndex(rest, " at ")
		if i < 0 {
			return When{}, errors.New("a monthly schedule needs its day and its time, as in \"monthly on the 3rd at 08:00\"")
		}
		md := whenMonthDay.FindStringSubmatch(rest[:i])
		if md == nil {
			return When{}, errors.New("a monthly schedule needs its day and its time, as in \"monthly on the 3rd at 08:00\"")
		}
		w.MonthDay, _ = strconv.Atoi(md[1])
		if w.MonthDay < 1 || w.MonthDay > 31 {
			return When{}, errors.New("the day of the month is 1 to 31")
		}
	}
	m := whenTime.FindAllStringSubmatch(timeText, -1)
	if len(m) == 0 {
		return When{}, errors.New("say the time it runs, as in \"at 09:00\"; " + whenForms)
	}
	tm := m[len(m)-1]
	h, _ := strconv.Atoi(tm[1])
	mi := 0
	if tm[2] != "" {
		mi, _ = strconv.Atoi(tm[2])
	}
	if tm[3] != "" && (h < 1 || h > 12) {
		return When{}, fmt.Errorf("%s is not a time of day", quoted(tm[0]))
	}
	switch tm[3] {
	case "pm":
		if h < 12 {
			h += 12
		}
	case "am":
		if h == 12 {
			h = 0
		}
	}
	if h > 23 || mi > 59 {
		return When{}, fmt.Errorf("%s is not a time of day", quoted(tm[0]))
	}
	w.Time = fmt.Sprintf("%02d:%02d", h, mi)
	if _, err := w.Args(); err != nil {
		return When{}, err
	}
	return w, nil
}

// DocsBase is the documentation site docs_read reads from, and nothing
// else.
const DocsBase = "https://docs.secureagentry.net"

// DocsText is a docs page's text: its article (or its main, or its body),
// with the links within the docs written as their paths, so a next
// docs_read can follow them.
func DocsText(body []byte, page string) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	root := findElement(doc, "article")
	if root == nil {
		root = findElement(doc, "main")
	}
	if root == nil {
		root = findElement(doc, "body")
	}
	if root == nil {
		return ""
	}
	base, _ := url.Parse(DocsBase + page)
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(strings.Join(strings.Fields(n.Data), " "))
			if strings.HasSuffix(n.Data, " ") || strings.HasSuffix(n.Data, "\n") {
				b.WriteString(" ")
			}
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "header", "footer", "svg", "button", "form":
				return
			case "p", "div", "section", "li", "tr", "pre", "h1", "h2", "h3", "h4", "h5", "h6", "br", "table", "ul", "ol", "dl", "dt", "dd", "blockquote":
				b.WriteString("\n")
				if strings.HasPrefix(n.Data, "h") && len(n.Data) == 2 {
					b.WriteString(strings.Repeat("#", int(n.Data[1]-'0')) + " ")
				}
				if n.Data == "li" {
					b.WriteString("- ")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key != "href" || base == nil {
					continue
				}
				if u, err := base.Parse(a.Val); err == nil && u.Host == base.Host && u.Path != base.Path {
					fmt.Fprintf(&b, " (%s)", u.Path)
				}
			}
		}
	}
	walk(root)
	var out []string
	for _, l := range strings.Split(b.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "-" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// findElement is the first element named tag beneath n.
func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, tag); f != nil {
			return f
		}
	}
	return nil
}
