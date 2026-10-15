package drafter

import (
	"encoding/json"
	"fmt"
	"net/url"
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
