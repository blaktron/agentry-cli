# Runtime facts

Generated from the Agentry CLI's code by its tests; do not edit by hand. These are the defaults a runner uses unless its operator's policy names others.

## Declared MCP servers

- npm servers install and run on `ghcr.io/blaktron/agentry-node-alpine:24-alpine`: Node 24.21.0.
- PyPI servers install and run on `ghcr.io/blaktron/agentry-python-slim:3.12-slim-trixie`: Python 3.12.15.
- The npm image is Alpine Linux, whose C library is musl. The PyPI image is Debian, whose C library is glibc, so a package's `manylinux` wheels install.
- npm installs run with `--ignore-scripts`; PyPI installs take wheels only (`--only-binary=:all:`), for the server and every dependency.
- A QuickAgent declares at most 8 MCP servers.
- A server's declared hosts are reached on TCP port 443 only; with none declared it has no network.

## Tool results

- Every result of the runner's own tools is held to its harness's limit less a margin: 48 KiB and 1,900 lines under docker-agent, the default harness, and less under others. A file is read a page at a time from the offset asked, each page saying where the next begins; a listing or a search stops at the limit and says what it left out.

## The fetch tool

- `fetch` reads at most 512 KiB of a response's body, and gives up on a request after 60 seconds.
- A body larger than one tool result, within that cap, is saved in the scratch folder, under `fetch/`, and answered with its first page and where it was saved; past the cap it is cut.
- `saveTo` saves the whole body at a path in the scratch folder, whatever its type, binary included, and the answer says what was saved instead of returning it.

## Input sources

- A `repository` question's repository is fetched by the runner before the run: at most 20,000 files and 1 GiB on the person's computer, 10,000 files and 256 MiB In Agentry.
- A `url` question's file is fetched the same way: one file of at most 256 MiB on the person's computer, 10 MiB In Agentry.

## Target hosts

- A QuickAgent declares at most 4 target hosts, each a `host` intake question listed in `capabilities.targetHosts`.
- The host tools: `host_run`, `host_read`, `host_write`, `host_put`, `host_get` for single calls, and `host_shell_open`, `host_shell_send`, `host_shell_read`, `host_shell_close` for an interactive shell.
- `host_run` ends a command after 300 seconds, or after its `timeout`, at most 1800.
- `host_put` and `host_get` copy files through `/scratch/hosts`, in the scratch folder.
- At most 4 shell sessions are open on a host at once, and a session no call has used for 30 minutes is closed. `host_shell_send`'s key `sudo-password` types the person's sudo password.

## A browser

- A QuickAgent's `capabilities.browser` names at most 4 sites, each a `url` intake question, and at most 32 hosts.
- The browser tools: `browser_click`, `browser_close`, `browser_console_messages`, `browser_drag`, `browser_drop`, `browser_emulate_media`, `browser_evaluate`, `browser_fill_form`, `browser_find`, `browser_generate_locator`, `browser_handle_dialog`, `browser_hover`, `browser_navigate`, `browser_navigate_back`, `browser_network_request`, `browser_network_requests`, `browser_pdf_save`, `browser_press_key`, `browser_resize`, `browser_select_option`, `browser_snapshot`, `browser_tabs`, `browser_take_screenshot`, `browser_type`, `browser_verify_element_visible`, `browser_verify_list_visible`, `browser_verify_text_visible`, `browser_verify_value`, `browser_wait_for`. A QuickAgent whose browser types keys has every one but `browser_evaluate`.
- The browser reaches only the sites and hosts its line allows, through the run's proxy, on ports 80 and 443; a site on the person's own network, and a site's own port, only in a run on their computer.
- It saves screenshots and PDFs in `reports/browser/`, and a file name the run gives is resolved inside that folder.

## Reports, PDFs and the scratch folder

- `write_report` writes a text file into `reports/`, which the person receives. It refuses a PDF.
- `write_report` and `export_pdf` take two optional arguments, `deliverable` (true) and `title` (one line of plain text, at most 120 characters), that mark the file as what the person asked the run for. A run's deliverables are the files it marked. When it marked none, the runner lists the PDFs the run exported, else every file in `reports/` outside `reports/browser/`, else the run's final message, which it writes to `reports/deliverable.md`. A run that fails has none.
- `export_pdf` turns a Markdown file from `reports/` or the scratch folder, of at most 1024 KiB, into a PDF in `reports/`, rendered by Typst 0.15.1 within 60 seconds, at most 64 MiB. It draws at most 20 local PNG or JPEG images of up to 10 MiB each; any other image is drawn as its alt text. The PDF's look is the person's report theme, which the run cannot choose.
- The scratch folder, `/scratch`, is the run's own working space through `scratch_write`, `scratch_list`, `scratch_read` and `scratch_search`: empty at the start, at most 256 MiB on Linux, never part of what the person receives, and deleted with the run.

## Each container's limits

- Memory 2g and 1 CPU; on Linux, also at most 512 processes.

## The base commands

Every runner provides these to a QuickAgent that declares them in `exec`, with no entry in the operator's catalogue:

base64, basename, bunzip2, bzcat, cat, cksum, cmp, comm, cut, diff, dirname, du, fold, grep, gunzip, head, hexdump, ls, md5sum, nl, od, paste, realpath, rev, sha1sum, sha256sum, sha512sum, sort, stat, strings, tac, tail, tar, tr, tree, uniq, unxz, unzip, wc, xzcat, zcat.
