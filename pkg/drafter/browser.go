package drafter

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The Drafter's rules on a browser (plan browser-testing.md M8;
// agentry-cli#916): a website the person names is tested through a declared
// browser (capabilities.browser), which the runner gives the run, so a draft
// never brings a browser of its own, as a command or as an MCP server.

// browserWords are the names, or parts of a package's name, that bring a
// browser of their own.
var browserWords = []string{"chromium", "chrome", "firefox", "playwright", "puppeteer", "selenium", "webdriver", "chromedriver", "geckodriver"}

// drivesABrowser says a declared command or a package, by its whole name
// (a path, a scope), is a browser or drives one.
func drivesABrowser(command string) bool {
	name := strings.ToLower(command)
	for _, w := range browserWords {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// browserServerProblems is the Drafter's rule on a declared MCP server that
// is a browser of its own (a Playwright, Puppeteer or Selenium package), one
// line each: the runner gives a declared browser, held to the sites the
// person names, where a package server's browser would reach whatever its
// egress allows. Nil when there is none, or the manifest does not parse
// (validate says so).
func browserServerProblems(raw json.RawMessage) []string {
	var m struct {
		Capabilities struct {
			MCPServers []struct {
				Name string `json:"name"`
				NPM  string `json:"npm"`
				PyPI string `json:"pypi"`
			} `json:"mcpServers"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []string
	for _, s := range m.Capabilities.MCPServers {
		pkg := s.NPM
		if pkg == "" {
			pkg = s.PyPI
		}
		if pkg == "" || !drivesABrowser(pkg) {
			continue
		}
		out = append(out, fmt.Sprintf("capabilities.mcpServers: %s (%s): a browser of its own; for a site the person names, ask for it with an intake question of type url, list its id in capabilities.browser.sites, and work it with the browser tools the runner gives; leave the server out",
			s.Name, pkg))
	}
	return out
}
