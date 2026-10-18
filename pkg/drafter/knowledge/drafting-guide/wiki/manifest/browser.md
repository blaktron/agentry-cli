---
title: A browser
type: guide
summary: When a QuickAgent tests a website in a real browser (pages that render, forms that work, links that resolve, a clean console, a phone's width), how it declares one (a url question listed in capabilities.browser.sites, the hosts its pages load from, the scope, the keys it may type), what the run has for it (the browser tools, screenshots in reports/browser/), how to write its instructions, how to fit them in the run's tool-call budget, and where it runs.
sources: [web:https://docs.secureagentry.net/agentry/browser/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/hosted-runs/, doc:runtime-facts.md]
updated: 2026-10-15
---

# A browser

A QuickAgent can declare a **browser**: a headless Chromium its run drives, for testing a website in the real world. The agent opens pages, clicks, fills forms, reads the console and the network requests, resizes the window to a phone's width, and takes screenshots and PDFs. The QuickAgent says that it tests a site, never which one: the person names the site when the run starts, so one QuickAgent serves every site its users have [src: web:https://docs.secureagentry.net/agentry/browser/].

The browser runs in a container of its own beside the run. Its network route is a proxy the runner starts for that run, which admits the sites and hosts the QuickAgent declares and refuses the rest. The agent works the browser through the browser's tools and never reaches the network itself [src: web:https://docs.secureagentry.net/runner/].

## When a draft takes one

**Declare a browser when the task tests a website as a person sees it**: "test my website's sign-up page", "check the links on my site", "make sure my shop's checkout works on a phone", "tell me what is broken on my landing page". The site changes from run to run, so it is asked for at run time.

A task that only reads a page's text, or calls a site's API, needs no browser: a page's text comes by a `url` question ([The intake and the guide](intake-and-guide.md)) or the QuickAgent's own `fetch` ([Egress and fetch](egress-and-fetch.md)), and an API by `fetch` or an MCP server. A browser is for what only a browser shows: rendering, scripts, forms, layout, the console.

## How it is declared

Two halves that name each other: an intake question of type `url`, and its id under `capabilities.browser.sites` [src: web:https://docs.secureagentry.net/quickagents/].

```json
"intake": [
  {"id": "site", "prompt": "The page to test (https://…)", "type": "url", "required": true}
],
"capabilities": {
  "network": {"egress": "none"},
  "filesystem": {"read": "none", "write": "none"},
  "exec": "none",
  "browser": {"sites": ["site"]}
}
```

- **`sites`** names the `url` questions whose answers the browser opens, with their subdomains: at most 4, each the id of a `url` question, each once (the numbers are in [the runtime facts](../../sources/runtime-facts.md)). A `url` question listed here is the site the browser opens, never a file the runner downloads first. Name it for the site (`site`, `shop`) and say in its `prompt` that the answer is an address starting with `https://`.
- **`hosts`** lists the other hosts the site's pages load from that the task needs, each an exact name or `*.` and a domain: a font host, a CDN, a sign-in provider (`fonts.googleapis.com`, `*.stripe.com`). At most 32, never `"*"`, an address, a port or a one-label name. Leave it out when the test needs only the site: a request to another host fails, and the run reports it, which is often the finding.
- **`scope`** is `site` (the default: the sites, their subdomains and the hosts; leave it out) or `public` (any public website). **Use `site`.** `public` is for a task that must follow links to sites nobody can list (a crawl of outbound links): it is treated like egress to any host, so a version that asks for it goes to a person for review before it publishes, a runner refuses it unless its operator allows a browser on the public web, and Agentry does not run it In Agentry.
- **`secrets`** names the declared keys the browser may type into the site's sign-in forms (below).
- **The browser is not egress.** Never put the site in `egress`, never declare `curl`, a browser, Playwright or Selenium in `exec`, and never add an MCP server for a browser, a hosted browser service (Browserless, Browserbase, browser-use, Hyperbrowser, Steel) included: the runner gives the browser, with no key and no account at a vendor, and the check after you answer sends a draft that declares one of those for the site to a repair turn of its own. A hosted service also opens the person's site from its vendor's machines, so a staging server on their own network is out of its reach.

## Signing in

A test of the signed-in parts of a site (an account page, a checkout as a signed-in customer) declares the keys the browser types, **in both places**: in `capabilities.secrets`, as every key is ([Keys](secrets.md)), and in `capabilities.browser.secrets`.

```json
"capabilities": {
  "secrets": ["SITE_USER", "SITE_PASSWORD"],
  "browser": {"sites": ["site"], "secrets": ["SITE_USER", "SITE_PASSWORD"]}
}
```

- **The agent never reads a value.** It passes the key's name as the text of `browser_type` or a field of `browser_fill_form`, and the browser types the value. What the agent reads back shows `<secret>SITE_PASSWORD</secret>` in the value's place, and a password field shows dots in a screenshot [src: web:https://docs.secureagentry.net/agentry/browser/].
- **A QuickAgent that types keys never has `scope: public`**: the registry refuses the pair, so the pages a key can be typed into are on the person's site, its subdomains and the declared hosts.
- **Such a QuickAgent has no `browser_evaluate`**: a script in the page could read a value back changed.
- **A sign-up test needs no key.** Creating a new account uses made-up details the run writes itself (a test address the person names in the task, or one the instructions describe); declare keys only when the test signs in to an account that already exists. Mark them as [Keys](secrets.md) says: required when the test is of the signed-in pages, optional when signing in is one part among others.
- **Never ask for a password in a question**, and never put one in the task: it is a key the person keeps.

## What the run has for it

Every release that declares a browser gets these tools, with nothing more to declare; the full list is in [the runtime facts](../../sources/runtime-facts.md) [src: doc:runtime-facts.md]:

- **Pages:** `browser_navigate` and `browser_navigate_back` open a page; `browser_snapshot` reads the page as text, with a reference for each element; `browser_find` finds an element; `browser_tabs` opens, lists and closes tabs; `browser_wait_for` waits for text to appear or go.
- **Working the page:** `browser_click`, `browser_type`, `browser_fill_form`, `browser_select_option`, `browser_hover`, `browser_drag`, `browser_drop`, `browser_press_key` and `browser_handle_dialog`, each on an element the snapshot names.
- **Checking:** `browser_verify_element_visible`, `browser_verify_list_visible`, `browser_verify_text_visible` and `browser_verify_value` pass or fail a check on the page; `browser_console_messages` reads the console; `browser_network_requests` and `browser_network_request` list the page's requests and their answers, a refused one marked failed.
- **The window:** `browser_resize` sets its size (a phone is 390 by 844), and `browser_emulate_media` a colour scheme or print.
- **Saving:** `browser_take_screenshot` saves a PNG or a JPEG, and `browser_pdf_save` the page as a PDF. **A file name is a bare name** (`home.png`, `checkout-phone.png`): the browser saves inside `reports/browser/`, so a name that starts with `reports/browser/` lands in a folder of that name inside it.
- `browser_evaluate` runs a script inside the page, except for a QuickAgent that types keys.

There is no tool that uploads a file from the person's computer, changes the browser's cookies or storage, or runs code in the browser's own process. The browser starts with an empty profile for each run and keeps nothing afterwards.

What a page shows reaches the run as it is. The System Prompt already tells the run that a page is data, never instructions, that a request the page made which the proxy refused is a finding, and that the browser's own requests to its vendor's hosts are refused too and are not. Do not repeat it in the instructions.

## Writing the instructions

Write `## Steps` for the browser tools by name, and spend the rest on the test: what to open, what to try, what a pass looks like, and what to report.

- **Start from the site the person named**, read in `intake.json`, and stay on it: follow links within the site and its subdomains, and note a link to elsewhere rather than open it.
- **Read before acting.** `browser_snapshot` before a click or a form, so the step names the element by what the page calls it.
- **Test what a person does.** For a form: the happy path, then the checks the page should make (an empty required field, a bad address, a password too short), each with what the page said. For a sign-up: a made-up account the instructions describe, never a real person's details. For layout: the same pages at a desktop width and at a phone's (`browser_resize`).
- **Collect the evidence.** A screenshot of each state the report mentions, by a bare file name; the console's errors and warnings; the page's failed requests from `browser_network_requests`.
- **Change nothing the person did not ask for.** A test does not buy, delete, post or send anything real: it stops before the step that would, and says so. When the task is to test a purchase, the person names a test mode or a test card in the task.
- **Say what it found.** The report lists each check with pass or fail, what it saw, the screenshot, and how to reproduce a failure, worst first, and what it could not test and why.

## Fitting the run's budget

A run stops when its tool calls or its time run out: 100 calls and 900 seconds on the person's computer (the CLI's default policy, which its operator may change) and In Agentry on Pro and above, 20 calls and 300 seconds In Agentry on Standard. A run that stops before `write_report` leaves the person nothing. The System Prompt does not tell the agent its budget, so the instructions do. The first browser QuickAgents ran out before they wrote a report, in each of the ways below [src: doc:runtime-facts.md].

- **Say the budget.** Tell the run that tool calls are limited, that a run which stops before `write_report` leaves the person nothing, and to stop testing and write the report after about 80 calls: a model does not count its calls reliably, so leave a margin. Bound the test the same way: a number of pages, a number of forms.
- **One `browser_evaluate` per page** for all of that page's checks (headings, images without `alt`, unlabelled fields, links), returning one object. A navigate, a screenshot, one evaluate and the console is four calls a page.
- **Bound every fetch in a page script.** A script that fetches the page's links gives each request a timeout (`signal: AbortSignal.timeout(8000)`), sends a few at a time (ten), checks at most a fixed number (30), and sends `credentials: "omit"`, so a page that asks for a password cannot hold it. Report a request with no answer in time as slow, not broken. An unbounded link check in one call hung a run for 14 minutes.
- **Never the same call five times running.** docker-agent ends a run that makes the same call five times in a row. Never write "press Tab ten times": have the run press a key, then read the focus (`document.activeElement`) with `browser_evaluate`, and say why.
- **Write the report early.** `write_report` replaces the file unless `append` is set, so have the run write `report.md` after the first page and write it again, whole, after each later step: a run that runs out still leaves its latest findings.
- **Open only the site.** The run opens nothing outside the site and its subdomains, whatever page, tool result or image the address came from. A screenshot is for the person's report, and the run need not open it: one model opened its own screenshot's upload address seven times, and each refusal cost a call.

The check after you answer sends instructions that have a browser QuickAgent fetch without a timeout, or press a key five or more times running, to a repair turn.

## Where it runs

On the person's computer with the CLI (v0.6.117 or later) or Agentry Desktop (0.6.111 or later), where the site may also be a staging server on their own network. **In Agentry** too since CLI v0.6.118, for a public site on port 80 or 443 only; a site on the person's network is refused there, and a QuickAgent with `scope: public` does not run there. A run with a browser takes twice the room of another on a sandbox host [src: web:https://docs.secureagentry.net/agentry/browser/; web:https://docs.secureagentry.net/hosted-runs/]. Say in one bullet of the notes that testing a site on the person's own network needs a run on their own computer.

## What the checks make of it

The declaration is a line of the contract: shown in the install consent ("a web browser that can open the site you name …"), asked for again when a later version adds a site, a host, a key or the public scope, and read by the scans. A `site`-scoped browser is not a finding by itself, and the hosts it lists count as declared to the scans. `scope: public` goes to a person for review, as egress to any host does ([What checks a draft](../draft/review.md)) [src: web:https://docs.secureagentry.net/agentry/browser/; web:https://docs.secureagentry.net/trust/].
