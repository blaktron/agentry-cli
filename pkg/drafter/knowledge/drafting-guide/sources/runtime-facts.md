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

## Each container's limits

- Memory 2g and 1 CPU; on Linux, also at most 512 processes.

## The base commands

Every runner provides these to a QuickAgent that declares them in `exec`, with no entry in the operator's catalogue:

base64, basename, bunzip2, bzcat, cat, cksum, cmp, comm, cut, diff, dirname, du, fold, grep, gunzip, head, hexdump, ls, md5sum, nl, od, paste, realpath, rev, sha1sum, sha256sum, sha512sum, sort, stat, strings, tac, tail, tar, tr, tree, uniq, unxz, unzip, wc, xzcat, zcat.
