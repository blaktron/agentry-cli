package drafter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The Drafter's rules on a browser: a website the person names is tested
// through a declared browser (capabilities.browser), which the runner gives
// the run, so a draft never brings a browser of its own, as a command or as
// an MCP server.

// browserWords are the names, or parts of a package's name or a remote
// server's address, that bring a browser of their own: the browsers and
// the libraries that drive one, and any name with "browser" in it, which
// covers the hosted browser services (Browserless, Browserbase,
// browser-use, Hyperbrowser). The match is by name alone, so a browser
// server whose name and address carry none of them is not caught here, and
// the guide is what steers the Drafter away from one.
var browserWords = []string{"browser", "chromium", "chrome", "firefox", "playwright", "puppeteer", "selenium", "webdriver", "chromedriver", "geckodriver"}

// hostedBrowsers are hosted browser services whose names carry none of
// browserWords, as a package's name or a remote server's host shows them.
var hostedBrowsers = []string{"steel.dev", "steel-dev", "steel-mcp"}

// drivesABrowser says a declared command or a package, by its whole name
// (a path, a scope), is a browser or drives one.
func drivesABrowser(command string) bool {
	name := strings.ToLower(command)
	for _, w := range browserWords {
		if strings.Contains(name, w) {
			return true
		}
	}
	for _, w := range hostedBrowsers {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// browserServerProblems is the Drafter's rule on a declared MCP server that
// is a browser of its own: a package that drives one (a Playwright,
// Puppeteer or Selenium server) or a hosted browser service, by its package
// or by its remote address. One line each: the runner gives a declared
// browser, held to the sites the person names, with no key and no account
// at a vendor, where a package's browser would reach whatever its egress
// allows and a hosted one opens the person's site from the vendor's
// machines. Nil when there is none, or the manifest does not parse
// (validate says so).
func browserServerProblems(raw json.RawMessage) []string {
	var m struct {
		Capabilities struct {
			MCPServers []struct {
				Name string `json:"name"`
				NPM  string `json:"npm"`
				PyPI string `json:"pypi"`
				URL  string `json:"url"`
			} `json:"mcpServers"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	const instead = "for a site the person names, ask for it with an intake question of type url, list its id in capabilities.browser.sites, and work it with the browser tools the runner gives, which need no key and no account; leave the server out"
	var out []string
	for _, s := range m.Capabilities.MCPServers {
		switch {
		case s.NPM != "" || s.PyPI != "":
			pkg := s.NPM
			if pkg == "" {
				pkg = s.PyPI
			}
			if drivesABrowser(pkg) {
				out = append(out, fmt.Sprintf("capabilities.mcpServers: %s (%s): a browser of its own; %s", s.Name, pkg, instead))
			}
		case s.URL != "":
			if hostsABrowser(s.URL) {
				out = append(out, fmt.Sprintf("capabilities.mcpServers: %s: a browser run by its vendor, which opens the site from the vendor's machines and needs an account there; %s", s.URL, instead))
			}
		}
	}
	return out
}

// hostsABrowser says a remote server's address, by its host and its path,
// is a hosted browser service's. An address that does not parse is
// validate's to report.
func hostsABrowser(address string) bool {
	u, err := url.Parse(address)
	if err != nil {
		return false
	}
	return drivesABrowser(u.Hostname() + u.EscapedPath())
}

// sameCallLimit is how many identical tool calls in a row docker-agent, the
// default harness, takes before it ends the run as a degenerate loop: "press
// Tab ten times" in a browser QuickAgent's instructions ends its run.
const sameCallLimit = 5

// pageFetch is a fetch inside a page script, as instructions write one for
// browser_evaluate.
var pageFetch = regexp.MustCompile(`\bfetch\(`)

// fetchTimeout is a page fetch given a deadline.
var fetchTimeout = regexp.MustCompile(`AbortSignal|AbortController`)

// pressTimes is an instruction to press a key, or tab, a number of times.
var pressTimes = regexp.MustCompile(`(?i)\b(press(es|ing)?|tab(s|bing)?)\b[^.\n]{0,60}?\b(\d+|five|six|seven|eight|nine|ten|eleven|twelve|fifteen|twenty)\s+times\b`)

// readsBetween is a sentence that has the run read the page as it presses,
// so its presses are not identical calls in a row.
var readsBetween = regexp.MustCompile(`(?i)\bread|browser_evaluate|browser_snapshot|activeElement`)

// spelled are the counts pressTimes reads in words.
var spelled = map[string]int{"five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "fifteen": 15, "twenty": 20}

// takesABrowser says the manifest declares a browser line.
func takesABrowser(raw json.RawMessage) bool {
	var m struct {
		Capabilities struct {
			Browser *json.RawMessage `json:"browser"`
		} `json:"capabilities"`
	}
	return json.Unmarshal(raw, &m) == nil && m.Capabilities.Browser != nil
}

// browserBudgetProblems is the Drafter's rule on a browser QuickAgent's
// tool-call budget, one line each: the two ways a browser run runs out that
// the words of its instructions show. A fetch in a page script with no deadline can hold
// one browser_evaluate until the run's time is gone, and a key pressed
// sameCallLimit times or more in a row is that many identical calls, which
// ends the run; a sentence that also has the run read the page between
// presses is not. The guide's other budget rules (one evaluate a page, the
// report written early, only the site opened) are judgement, not words, and
// stay in the guide. Nil for a QuickAgent with no browser, or instructions
// that do neither.
func browserBudgetProblems(raw json.RawMessage, instructions string) []string {
	if !takesABrowser(raw) {
		return nil
	}
	var out []string
	if pageFetch.MatchString(instructions) && !fetchTimeout.MatchString(instructions) {
		out = append(out, `the instructions: a page script fetches with no timeout, which can hold browser_evaluate until the run's time runs out; give each request signal: AbortSignal.timeout(8000) and credentials: "omit", send a few at a time, cap how many, and report one with no answer as slow (manifest/browser.md, "Fitting the run's budget")`)
	}
	for _, s := range instructionSentences(instructions) {
		m := pressTimes.FindStringSubmatchIndex(s)
		if m == nil || negated.MatchString(s[:m[0]]) || readsBetween.MatchString(s) {
			continue
		}
		word := strings.ToLower(s[m[8]:m[9]])
		n, err := strconv.Atoi(word)
		if err != nil {
			n = spelled[word]
		}
		if n >= sameCallLimit {
			out = append(out, fmt.Sprintf("the instructions: %q is %d identical calls, and the run ends at %d in a row; have the run read the focus with browser_evaluate after each key press (manifest/browser.md, \"Fitting the run's budget\")",
				strings.TrimSpace(s[m[0]:m[1]]), n, sameCallLimit))
			break
		}
	}
	return out
}

// noteBrowserBudgetProblems says in the notes that the finished draft's
// instructions still risk running out before the report.
func (d *Draft) noteBrowserBudgetProblems() {
	probs := browserBudgetProblems(d.Manifest, d.Instructions)
	if len(probs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("## Browser steps to fix before a run\n\nThis draft's instructions can end a run before it writes its report:\n\n")
	for _, p := range probs {
		b.WriteString("- " + p + "\n")
	}
	d.composeNotes(b.String())
}
