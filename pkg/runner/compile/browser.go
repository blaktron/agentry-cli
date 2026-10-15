package compile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The browser. A release that declares capabilities.browser gets one more
// server in the gateway's catalog, the browser server, on its own pulled
// image: a headless Chromium whose only route out is the run's browse
// proxy, reached through one relay, and which holds the scope. Its run-time
// values are placeholders (the proxy's name, the user it runs as), so the
// plan is the release's.

// browserConfigFile is the plan's gateway/browser.json, the server's
// --config.
const browserConfigFile = "gateway/browser.json"

// browserCap normalizes the manifest's browser line, refusing it where the
// run cannot hold it: beside a built-in's grants, and with public scope
// unless the operator's policy allows any public site, as "*" egress is
// refused.
func browserCap(b *manifest.Browser, in Inputs) (*spec.BrowserCap, error) {
	if b == nil {
		return nil, nil
	}
	if in.Web || in.Review || in.Source == "builtin" {
		return nil, &PolicyRefusal{Reason: "a built-in is never granted a browser, nor is one granted beside the web tools or the review grant"}
	}
	if b.IsPublic() && !in.Policy.Browser.AllowPublicWeb {
		return nil, &PolicyRefusal{Reason: "the release's browser asks for any public site (capabilities.browser.scope: public), which this machine's policy does not allow: set browser.allowPublicWeb: true to permit it"}
	}
	c := &spec.BrowserCap{
		Sites:   append([]string(nil), b.Sites...),
		Public:  b.IsPublic(),
		Secrets: append([]string(nil), b.Secrets...),
	}
	for _, h := range b.Hosts {
		c.Hosts = append(c.Hosts, strings.ToLower(h))
	}
	return c, nil
}

// browserFolder is a run folder of the browser server's, in plan
// placeholders.
func browserFolder(name string) string { return spec.PHCfg + "/" + name }

// browserEntry is the browser server's catalog entry. It reaches the browse
// proxy through one L4 relay (allowHosts, the proxy's run-unique name) and
// no other network; Chromium's --proxy-server names the same host, which
// the gateway links to the relay. It runs as the person's uid, so its
// output folder and secrets file stay theirs. Its one read-write bind is
// its output folder; its config and its secrets file are read-only, and no
// other container is given the latter.
func browserEntry(b *spec.BrowserCap, image string) *yaml.Node {
	secrets := len(b.Secrets) > 0
	e := yamlMap{}
	e.set("description", strNode("Agentry's browser: a headless Chromium behind the run's browse proxy (generated per run)"))
	e.set("title", strNode("Browser"))
	e.set("type", strNode("server"))
	e.set("image", strNode(image))
	e.set("command", strListNode(browser.Args(spec.PHBrowseProxy, secrets)))
	e.set("user", strNode(spec.PHBrowserUser))
	e.set("longLived", boolNode(true))
	vols := []string{
		spec.PHCfg + "/" + browserConfigFile + ":" + browser.ConfigMount + ":ro",
		browserFolder(spec.BrowserOutDir) + ":" + browser.OutMount + ":rw",
	}
	if secrets {
		vols = append(vols, browserFolder(spec.BrowserDir)+"/"+spec.BrowserSecretsFile+":"+browser.SecretsMount+":ro")
	}
	e.set("volumes", strListNode(vols))
	e.set("allowHosts", strListNode([]string{fmt.Sprintf("%s:%d/tcp", spec.PHBrowseProxy, browser.ProxyPort)}))
	return e.node()
}

// browserWritable is the browser server's one writable folder, by its
// exact path, as the gateway's bind check takes it.
func browserWritable() []string { return []string{browserFolder(spec.BrowserOutDir)} }

// browserOutVolume shares the browser's output folder with the tools
// server, read-only, inside its output folder: the agent's report can name
// a screenshot, and export_pdf can draw one.
func browserOutVolume() string {
	return browserFolder(spec.BrowserOutDir) + ":" + spec.BrowserOutMount + ":ro"
}

// browserScope says where the browse proxy lets the browser go, before the
// run knows the site.
func browserScope(b *spec.BrowserCap) string {
	if b.Public {
		return "any public site on ports 80 and 443 (scope: public, allowed by this machine's browser.allowPublicWeb)"
	}
	var parts []string
	if len(b.Sites) > 0 {
		parts = append(parts, "the site the person names for "+strings.Join(b.Sites, ", ")+" and its subdomains")
	}
	if len(b.Hosts) > 0 {
		parts = append(parts, strings.Join(b.Hosts, ", "))
	}
	return strings.Join(parts, ", and ")
}

// browserEnforcement is the plan's browser line: what the run does with a
// declared browser, before the run knows the site.
func browserEnforcement(b *spec.BrowserCap, pol *policy.Policy) string {
	line := fmt.Sprintf("a headless Chromium in the browser server (%s, %s, its own container, every capability dropped, a read-only root, Chromium's own sandbox on), "+
		"whose only route out is the run's browse proxy through one relay: the proxy resolves every name itself, dials only the address it checked, and admits %s; "+
		"private, loopback, link-local, CGNAT and metadata addresses are refused but, in a local run, the exact private host the person typed; every refusal is in receipts/browser-proxy.log; "+
		"the server writes only reports/browser/",
		browser.ServerName, pol.Images.BrowserImage(), browserScope(b))
	if len(b.Secrets) > 0 {
		line += fmt.Sprintf("; the secrets %s reach it alone, as its --secrets file: the agent passes a name and the server types the value, replacing the exact value with the name in what it returns (its own masking, which a transformed value would pass, so %s is not granted)",
			strings.Join(b.Secrets, ", "), browser.ToolEvaluate)
	}
	return line
}
