# Runtime facts

Generated from the Agentry CLI's code by its tests; do not edit by hand. These are the defaults a runner uses unless its operator's policy names others.

## Declared MCP servers

- npm servers install and run on `ghcr.io/blaktron/agentry-node-alpine:24-alpine`: Node 24.21.0.
- PyPI servers install and run on `ghcr.io/blaktron/agentry-python-alpine:3.12-alpine`: Python 3.12.14.
- Both images are Alpine Linux, whose C library is musl.
- npm installs run with `--ignore-scripts`; PyPI installs take wheels only (`--only-binary=:all:`), for the server and every dependency.
- A QuickAgent declares at most 8 MCP servers.
- A server's declared hosts are reached on TCP port 443 only; with none declared it has no network.

## The fetch tool

- `fetch` returns at most 512 KiB of a response's body, and gives up on a request after 60 seconds.

## Reports, PDFs and the scratch folder

- `write_report` writes a text file into `reports/`, which the person receives. It refuses a PDF.
- `export_pdf` turns a Markdown file from `reports/` or the scratch folder, of at most 1024 KiB, into a PDF in `reports/`, rendered by Typst 0.15.1 within 60 seconds, at most 64 MiB. It draws at most 20 local PNG or JPEG images of up to 10 MiB each; any other image is drawn as its alt text. The PDF's look is the person's report theme, which the run cannot choose.
- The scratch folder, `/scratch`, is the run's own working space through `scratch_write`, `scratch_list` and `scratch_read`: empty at the start, at most 256 MiB on Linux, never part of what the person receives, and deleted with the run.

## Each container's limits

- Memory 2g and 1 CPU; on Linux, also at most 512 processes.

## The base commands

Every runner provides these to a QuickAgent that declares them in `exec`, with no entry in the operator's catalogue:

base64, basename, bunzip2, bzcat, cat, cksum, cmp, comm, cut, diff, dirname, du, fold, grep, gunzip, head, hexdump, ls, md5sum, nl, od, paste, realpath, rev, sha1sum, sha256sum, sha512sum, sort, stat, strings, tac, tail, tar, tr, tree, uniq, unxz, unzip, wc, xzcat, zcat.
