// Package browser holds what the runner knows of the browser server, the
// gateway-launched container a release that declares capabilities.browser
// gets (agentry-notes plans/browser-testing.md §7, D3, D7, D8, D9;
// agentry-cli#915): Microsoft's @playwright/mcp on the agentry-browser
// image, driving a headless Chromium whose only route out is the run's
// browse proxy. The server is the vendor's; this package is its name, its
// tool allowlist, where its files are mounted and the arguments it starts
// with, so the compiler, the engine and the docker filter agree on them.
package browser

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ServerName is the browser server's name in the gateway's catalog. A
// declared MCP server may not take it.
const ServerName = "browser"

// The tools the gateway passes, as @playwright/mcp 0.0.83 names them
// (plan §7, D7). The filter is an allowlist, so a tool a later release of
// the server adds stays out until it is reviewed and named here. Never
// browser_run_code_unsafe or browser_file_upload, and never the network,
// storage, config, devtools or vision capabilities' tools.
var (
	coreTools = []string{
		"browser_navigate", "browser_navigate_back", "browser_snapshot", "browser_find",
		"browser_click", "browser_type", "browser_fill_form", "browser_select_option",
		"browser_hover", "browser_drag", "browser_drop", "browser_press_key",
		"browser_wait_for", "browser_tabs", "browser_resize", "browser_emulate_media",
		"browser_handle_dialog", "browser_console_messages", "browser_network_requests",
		"browser_network_request", "browser_take_screenshot", "browser_evaluate", "browser_close",
	}
	// testingTools come with --caps testing.
	testingTools = []string{
		"browser_verify_element_visible", "browser_verify_list_visible",
		"browser_verify_text_visible", "browser_verify_value", "browser_generate_locator",
	}
	// pdfTools come with --caps pdf.
	pdfTools = []string{"browser_pdf_save"}
)

// ToolEvaluate runs script in the page. It passes, except for a release
// whose browser may type secrets (§7.1, D9): with it the agent could read
// a typed value back transformed, past the server's exact-match redaction.
const ToolEvaluate = "browser_evaluate"

// Tools is the allowlist for a browser, sorted: every core, testing and
// pdf tool, without browser_evaluate when the line declares secrets.
func Tools(secrets bool) []string {
	out := slices.Concat(coreTools, testingTools, pdfTools)
	if secrets {
		out = slices.DeleteFunc(out, func(t string) bool { return t == ToolEvaluate })
	}
	slices.Sort(out)
	return out
}

// Where the server's files are, in its container.
const (
	// OutMount is the server's output folder and its working directory
	// (the image's WORKDIR, plan D8): --output-dir, where a file the
	// server names itself lands, and the root it resolves a model-chosen
	// filename against. It is bound to the run's browser output folder,
	// read-write, and nothing else of the host's is.
	OutMount = "/out-browser"
	// ConfigMount is the server's --config file, read-only.
	ConfigMount = "/etc/agentry-run/browser.json"
	// SecretsMount is the --secrets dotenv file, read-only, for a line
	// that declares browser secrets (§7.1).
	SecretsMount = "/run/agentry-run/browser-secrets.env"
	// Home is the server's home, a tmpfs the docker filter gives it
	// (the root file system is read-only).
	Home = "/home/browser"
	// ImageUser is the image's own user, which the server keeps where the
	// person's machine has no uid to give it (Windows).
	ImageUser = "10001:10001"
)

// ProxyPort is the browse proxy's port; the server's one allowHosts entry
// is the proxy's run-unique name on it.
const ProxyPort = 3128

// Viewport is the page size the server opens at; browser_resize changes
// it (a phone's width, say).
const Viewport = "1280x720"

// Args are the server's arguments after the image's entrypoint (node and
// the server's cli.js): headless, an isolated profile (D14), Chromium,
// the testing and pdf capabilities, the browse proxy as its only route,
// its output folder, its config, and its secrets where the line declares
// any (§7). proxy is the proxy's host name, a placeholder in a plan.
func Args(proxy string, secrets bool) []string {
	a := []string{
		"--headless", "--isolated", "--browser", "chromium",
		"--caps", "testing,pdf",
		"--proxy-server", "http://" + proxy + ":" + strconv.Itoa(ProxyPort),
		"--output-dir", OutMount,
		"--viewport-size", Viewport,
		"--config", ConfigMount,
	}
	if secrets {
		a = append(a, "--secrets", SecretsMount)
	}
	return a
}

// Config is the server's --config file: Chromium's own sandbox on (the
// server's default passes --no-sandbox), which the docker filter's
// seccomp profile lets run under runc (plan §7, M1).
func Config() []byte {
	return []byte(`{"browser":{"launchOptions":{"chromiumSandbox":true}}}` + "\n")
}

// Site is a site the person named for a run's browser (a url question in
// capabilities.browser.sites): where the browser starts, and the host and
// port the browse proxy admits for it.
type Site struct {
	// URL is the answer, as the agent reads it in intake.json.
	URL string
	// Host is its host, lower case, and Port its port where the answer
	// gave one ("" for the scheme's own).
	Host, Port string
}

// ProxyEntry is the site as the browse proxy's --browse-sites takes it:
// host, or host:port where the answer named a port of its own.
func (s Site) ProxyEntry() string {
	if s.Port == "" {
		return s.Host
	}
	return net.JoinHostPort(s.Host, s.Port)
}

// ParseSite reads a site answer: an http or https URL with a host, no user
// information, no fragment, at most 2,048 bytes. http is allowed because a
// site under test may be a staging server without TLS (plan D2).
func ParseSite(answer string) (Site, error) {
	answer = strings.TrimSpace(answer)
	if len(answer) > 2048 {
		return Site{}, errors.New("a site's address is at most 2,048 characters")
	}
	u, err := url.Parse(answer)
	switch {
	case err != nil:
		return Site{}, fmt.Errorf("%q is not a web address: %v", answer, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return Site{}, fmt.Errorf("%q is not an http or https address", answer)
	case u.Hostname() == "":
		return Site{}, fmt.Errorf("%q names no host", answer)
	case u.User != nil:
		return Site{}, fmt.Errorf("%q carries a user name or password: give the site's address alone, and a sign-in as the QuickAgent's declared secrets", answer)
	case u.Fragment != "" || strings.Contains(answer, "#"):
		return Site{}, fmt.Errorf("%q carries a fragment (#…)", answer)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if strings.ContainsAny(host, " \t%") {
		return Site{}, fmt.Errorf("%q has a host that is not a host name or an address", answer)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Site{}, fmt.Errorf("%q has a port that is not one", answer)
		}
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			port = ""
		}
	}
	return Site{URL: u.String(), Host: host, Port: port}, nil
}
