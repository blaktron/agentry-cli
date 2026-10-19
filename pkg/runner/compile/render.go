package compile

import (
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// renderFolder is the page renderer's hand-off folder, in plan
// placeholders: the one folder of the run's cfg the tools server writes.
func renderFolder() string { return spec.PHCfg + "/" + spec.RenderDir }

// renderMount is the tools spec's render folder: the mount when the plan
// grants a rendered web_read, empty otherwise.
func renderMount(caps spec.CapSummary) string {
	if caps.WebRender {
		return spec.RenderMount
	}
	return ""
}

// renderEnforcement is the plan's web.render line: what a rendered
// web_read is and is not.
const renderEnforcement = "web_read renders a page whose text is thin, or when asked, in a headless Chromium the engine runs once a page from the browser image, with Chromium's sandbox on, every capability dropped, a read-only root and no new privileges, on an internal network of its own whose one way out is a proxy that admits public addresses on ports 80 and 443 alone; " +
	"the rendered page's text is held to the same address check and AI Review as a plain read, and the model never drives the browser: no click, no typing, no secret, no cookie kept between pages"
