package ical

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// vtimezone is a feed's own VTIMEZONE: its TZID, the IANA name some
// writers add (X-LIC-LOCATION), and its STANDARD and DAYLIGHT observances.
type vtimezone struct {
	id          string
	location    string
	observances []observance
}

type observance struct {
	daylight   bool
	offsetTo   int
	offsetFrom int
	hasTo      bool
}

func setObservanceProperty(o *observance, p property) {
	switch p.Name {
	case "TZOFFSETTO":
		if s, ok := parseOffset(p.Value); ok {
			o.offsetTo, o.hasTo = s, true
		}
	case "TZOFFSETFROM":
		if s, ok := parseOffset(p.Value); ok {
			o.offsetFrom = s
		}
	}
}

// parseOffset reads a UTC offset, +HHMM[SS] or -HHMM[SS], in seconds.
func parseOffset(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if len(v) != 5 && len(v) != 7 || (v[0] != '+' && v[0] != '-') {
		return 0, false
	}
	h, err1 := strconv.Atoi(v[1:3])
	m, err2 := strconv.Atoi(v[3:5])
	s := 0
	var err3 error
	if len(v) == 7 {
		s, err3 = strconv.Atoi(v[5:7])
	}
	if err1 != nil || err2 != nil || err3 != nil || h > 23 || m > 59 || s > 59 {
		return 0, false
	}
	n := h*3600 + m*60 + s
	if v[0] == '-' {
		n = -n
	}
	return n, true
}

// zoneResolution says how a TZID was understood.
type zoneResolution int

const (
	zoneExact zoneResolution = iota
	// zoneApprox is a feed's own zone matched to an IANA zone by its
	// offsets alone, or held at its standard offset: times near a
	// daylight-saving change may be an hour out.
	zoneApprox
	// zoneUnknown is a TZID nothing explains: times are read in the
	// fallback zone.
	zoneUnknown
)

// location resolves a TZID: an IANA name (also as the tail of a
// /vendor/.../Area/City path), a Windows zone name, which Outlook and
// Exchange write, or the feed's own VTIMEZONE.
func (c *Calendar) location(tzid string, fallback *time.Location) (*time.Location, zoneResolution) {
	tzid = strings.Trim(strings.TrimSpace(tzid), `"`)
	if tzid == "" {
		return fallback, zoneUnknown
	}
	if z, ok := c.resolved[tzid]; ok {
		if z.how == zoneUnknown {
			return fallback, zoneUnknown
		}
		return z.loc, z.how
	}
	loc, how := c.resolve(tzid, fallback)
	if c.resolved == nil {
		c.resolved = map[string]resolvedZone{}
	}
	c.resolved[tzid] = resolvedZone{loc, how}
	return loc, how
}

// resolvedZone is a TZID as location understood it, kept so each is worked
// out once.
type resolvedZone struct {
	loc *time.Location
	how zoneResolution
}

func (c *Calendar) resolve(tzid string, fallback *time.Location) (*time.Location, zoneResolution) {
	if loc, ok := ianaLocation(tzid); ok {
		return loc, zoneExact
	}
	if name, ok := windowsZones[tzid]; ok {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc, zoneExact
		}
	}
	if tz := c.zones[tzid]; tz != nil {
		if loc, ok := ianaLocation(tz.location); ok {
			return loc, zoneExact
		}
		if loc, ok := tz.approximate(); ok {
			return loc, zoneApprox
		}
	}
	return fallback, zoneUnknown
}

func ianaLocation(name string) (*time.Location, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil, false
	}
	if strings.EqualFold(name, "UTC") || strings.EqualFold(name, "GMT") || strings.EqualFold(name, "Etc/UTC") {
		return time.UTC, true
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc, true
	}
	// /mozilla.org/20070129_1/Europe/Berlin and the like.
	if parts := strings.Split(strings.Trim(name, "/"), "/"); len(parts) >= 2 {
		for i := len(parts) - 2; i >= 0; i-- {
			if loc, err := time.LoadLocation(strings.Join(parts[i:], "/")); err == nil {
				return loc, true
			}
		}
	}
	return nil, false
}

// approximate finds an IANA zone whose offsets in January and July match
// the VTIMEZONE's standard and daylight offsets, or falls back to a fixed
// zone at the standard offset.
func (tz *vtimezone) approximate() (*time.Location, bool) {
	std, dst, hasStd, hasDst := 0, 0, false, false
	for _, o := range tz.observances {
		if !o.hasTo {
			continue
		}
		if o.daylight {
			dst, hasDst = o.offsetTo, true
		} else {
			std, hasStd = o.offsetTo, true
		}
	}
	if !hasStd && !hasDst {
		return nil, false
	}
	if !hasStd {
		std = dst
	}
	if !hasDst {
		dst = std
	}
	year := time.Now().Year()
	names := make([]string, 0, len(windowsZones))
	for _, n := range windowsZones {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		loc, err := time.LoadLocation(n)
		if err != nil {
			continue
		}
		_, jan := time.Date(year, 1, 15, 12, 0, 0, 0, loc).Zone()
		_, jul := time.Date(year, 7, 15, 12, 0, 0, 0, loc).Zone()
		if (jan == std && jul == dst) || (jan == dst && jul == std && std != dst) {
			return loc, true
		}
	}
	return time.FixedZone(tz.id, std), true
}

// windowsZones maps the Windows zone names Outlook and Exchange write as a
// TZID to IANA zones, as CLDR's windowsZones.xml maps them for territory
// 001.
var windowsZones = map[string]string{
	"Dateline Standard Time":          "Etc/GMT+12",
	"UTC-11":                          "Etc/GMT+11",
	"Aleutian Standard Time":          "America/Adak",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Marquesas Standard Time":         "Pacific/Marquesas",
	"Alaskan Standard Time":           "America/Anchorage",
	"UTC-09":                          "Etc/GMT+9",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"UTC-08":                          "Etc/GMT+8",
	"Pacific Standard Time":           "America/Los_Angeles",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time (Mexico)": "America/Mazatlan",
	"Mountain Standard Time":          "America/Denver",
	"Yukon Standard Time":             "America/Whitehorse",
	"Central America Standard Time":   "America/Guatemala",
	"Central Standard Time":           "America/Chicago",
	"Easter Island Standard Time":     "Pacific/Easter",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"SA Pacific Standard Time":        "America/Bogota",
	"Eastern Standard Time (Mexico)":  "America/Cancun",
	"Eastern Standard Time":           "America/New_York",
	"Haiti Standard Time":             "America/Port-au-Prince",
	"Cuba Standard Time":              "America/Havana",
	"US Eastern Standard Time":        "America/Indiana/Indianapolis",
	"Turks And Caicos Standard Time":  "America/Grand_Turk",
	"Paraguay Standard Time":          "America/Asuncion",
	"Atlantic Standard Time":          "America/Halifax",
	"Venezuela Standard Time":         "America/Caracas",
	"Central Brazilian Standard Time": "America/Cuiaba",
	"SA Western Standard Time":        "America/La_Paz",
	"Pacific SA Standard Time":        "America/Santiago",
	"Newfoundland Standard Time":      "America/St_Johns",
	"Tocantins Standard Time":         "America/Araguaina",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"SA Eastern Standard Time":        "America/Cayenne",
	"Argentina Standard Time":         "America/Buenos_Aires",
	"Greenland Standard Time":         "America/Godthab",
	"Montevideo Standard Time":        "America/Montevideo",
	"Magallanes Standard Time":        "America/Punta_Arenas",
	"Saint Pierre Standard Time":      "America/Miquelon",
	"Bahia Standard Time":             "America/Bahia",
	"UTC-02":                          "Etc/GMT+2",
	"Azores Standard Time":            "Atlantic/Azores",
	"Cape Verde Standard Time":        "Atlantic/Cape_Verde",
	"UTC":                             "Etc/UTC",
	"Coordinated Universal Time":      "Etc/UTC",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"Sao Tome Standard Time":          "Africa/Sao_Tome",
	"Morocco Standard Time":           "Africa/Casablanca",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Romance Standard Time":           "Europe/Paris",
	"Central European Standard Time":  "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"Jordan Standard Time":            "Asia/Amman",
	"GTB Standard Time":               "Europe/Bucharest",
	"Middle East Standard Time":       "Asia/Beirut",
	"Egypt Standard Time":             "Africa/Cairo",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"Syria Standard Time":             "Asia/Damascus",
	"West Bank Standard Time":         "Asia/Hebron",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"FLE Standard Time":               "Europe/Kiev",
	"Israel Standard Time":            "Asia/Jerusalem",
	"South Sudan Standard Time":       "Africa/Juba",
	"Kaliningrad Standard Time":       "Europe/Kaliningrad",
	"Sudan Standard Time":             "Africa/Khartoum",
	"Libya Standard Time":             "Africa/Tripoli",
	"Namibia Standard Time":           "Africa/Windhoek",
	"Arabic Standard Time":            "Asia/Baghdad",
	"Turkey Standard Time":            "Europe/Istanbul",
	"Arab Standard Time":              "Asia/Riyadh",
	"Belarus Standard Time":           "Europe/Minsk",
	"Russian Standard Time":           "Europe/Moscow",
	"E. Africa Standard Time":         "Africa/Nairobi",
	"Volgograd Standard Time":         "Europe/Volgograd",
	"Iran Standard Time":              "Asia/Tehran",
	"Arabian Standard Time":           "Asia/Dubai",
	"Astrakhan Standard Time":         "Europe/Astrakhan",
	"Azerbaijan Standard Time":        "Asia/Baku",
	"Russia Time Zone 3":              "Europe/Samara",
	"Mauritius Standard Time":         "Indian/Mauritius",
	"Saratov Standard Time":           "Europe/Saratov",
	"Georgian Standard Time":          "Asia/Tbilisi",
	"Caucasus Standard Time":          "Asia/Yerevan",
	"Afghanistan Standard Time":       "Asia/Kabul",
	"West Asia Standard Time":         "Asia/Tashkent",
	"Ekaterinburg Standard Time":      "Asia/Yekaterinburg",
	"Pakistan Standard Time":          "Asia/Karachi",
	"Qyzylorda Standard Time":         "Asia/Qyzylorda",
	"India Standard Time":             "Asia/Calcutta",
	"Sri Lanka Standard Time":         "Asia/Colombo",
	"Nepal Standard Time":             "Asia/Katmandu",
	"Central Asia Standard Time":      "Asia/Almaty",
	"Bangladesh Standard Time":        "Asia/Dhaka",
	"Omsk Standard Time":              "Asia/Omsk",
	"Myanmar Standard Time":           "Asia/Rangoon",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"Altai Standard Time":             "Asia/Barnaul",
	"W. Mongolia Standard Time":       "Asia/Hovd",
	"North Asia Standard Time":        "Asia/Krasnoyarsk",
	"N. Central Asia Standard Time":   "Asia/Novosibirsk",
	"Tomsk Standard Time":             "Asia/Tomsk",
	"China Standard Time":             "Asia/Shanghai",
	"North Asia East Standard Time":   "Asia/Irkutsk",
	"Singapore Standard Time":         "Asia/Singapore",
	"W. Australia Standard Time":      "Australia/Perth",
	"Taipei Standard Time":            "Asia/Taipei",
	"Ulaanbaatar Standard Time":       "Asia/Ulaanbaatar",
	"Aus Central W. Standard Time":    "Australia/Eucla",
	"Transbaikal Standard Time":       "Asia/Chita",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"North Korea Standard Time":       "Asia/Pyongyang",
	"Korea Standard Time":             "Asia/Seoul",
	"Yakutsk Standard Time":           "Asia/Yakutsk",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"AUS Central Standard Time":       "Australia/Darwin",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"West Pacific Standard Time":      "Pacific/Port_Moresby",
	"Tasmania Standard Time":          "Australia/Hobart",
	"Vladivostok Standard Time":       "Asia/Vladivostok",
	"Lord Howe Standard Time":         "Australia/Lord_Howe",
	"Bougainville Standard Time":      "Pacific/Bougainville",
	"Russia Time Zone 10":             "Asia/Srednekolymsk",
	"Magadan Standard Time":           "Asia/Magadan",
	"Norfolk Standard Time":           "Pacific/Norfolk",
	"Sakhalin Standard Time":          "Asia/Sakhalin",
	"Central Pacific Standard Time":   "Pacific/Guadalcanal",
	"Russia Time Zone 11":             "Asia/Kamchatka",
	"New Zealand Standard Time":       "Pacific/Auckland",
	"UTC+12":                          "Etc/GMT-12",
	"Fiji Standard Time":              "Pacific/Fiji",
	"Chatham Islands Standard Time":   "Pacific/Chatham",
	"UTC+13":                          "Etc/GMT-13",
	"Tonga Standard Time":             "Pacific/Tongatapu",
	"Samoa Standard Time":             "Pacific/Apia",
	"Line Islands Standard Time":      "Pacific/Kiritimati",
}
