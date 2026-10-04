// The npm MCP-server builder (agentry-cli#91). It runs in the policy's Node
// runtime image as
//
//   node -e <this file> PREFIX PACKAGE VERSION REGISTRY INTEGRITY
//
// so every value arrives as its own argv element and reaches npm as one,
// never through a shell. The pinned package's tarball is fetched with
// `npm pack` (which keeps the registry's bytes), hashed here against the
// integrity the runner recorded when it resolved the pin, and only then
// installed — from that verified file, never from a second fetch. This is
// the npm half of what the PyPI builder does for wheels (agentry-cli#4,
// DR-13). The dependency closure is still fetched and checked by npm itself
// against the registry's own metadata: nothing Agentry publishes pins it yet
// (agentry#471, agentry-cli#92).
//
// Since agentry-cli#608 the verified tarball's own `engines.node` is
// checked against this runtime's Node before the install: npm only warns
// (EBADENGINE), and a server built for a newer Node installs, starts and
// then fails every tool call (`Promise.try is not a function` on Node 22).
//
// Exit 3: nothing to verify against (no recorded integrity, or not exactly
// one tarball); exit 4: the tarball does not match; exit 5: its
// `engines.node` excludes this runtime's Node; exit 6: the check could not
// run (no npm semver in the image, or an unreadable package.json);
// otherwise npm's own code.
"use strict";

const { spawnSync } = require("child_process");
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const zlib = require("zlib");

// Kept in step with npmDownloadDir in mcpinstall.go (a test holds them
// together).
const DOWNLOAD_DIR = "dl";

// Strongest first: an SRI string may carry several algorithms, and the
// strongest one present is the one that must match (the W3C SRI rule).
const ALGORITHMS = ["sha512", "sha384", "sha256", "sha1"];

function run(argv) {
  const r = spawnSync(argv[0], argv.slice(1), { stdio: "inherit" });
  if (r.error) {
    console.log(`mcp install: ${argv[0]}: ${r.error.message}`);
    process.exit(1);
  }
  if (r.status !== 0) {
    process.exit(r.status === null ? 1 : r.status);
  }
}

// expected turns the recorded integrity into [algorithm, encoding, digests]:
// an SRI string ("sha512-<base64> …") or the resolver's legacy form for a
// registry that published only a shasum ("sha1:<hex>").
function expected(integrity) {
  const legacy = /^sha1:([0-9a-f]{40})$/i.exec(integrity);
  if (legacy) {
    return ["sha1", "hex", [legacy[1].toLowerCase()]];
  }
  const byAlg = {};
  for (const token of integrity.trim().split(/\s+/)) {
    const m = /^(sha512|sha384|sha256|sha1)-([A-Za-z0-9+/]+={0,2})(\?.*)?$/.exec(token);
    if (m) {
      (byAlg[m[1]] = byAlg[m[1]] || []).push(m[2]);
    }
  }
  for (const alg of ALGORITHMS) {
    if (byAlg[alg]) {
      return [alg, "base64", byAlg[alg]];
    }
  }
  return null;
}

// packageJSON is the text of the tarball's top-level package.json, or null
// when it has none. npm packs one top-level directory (usually "package/")
// whatever its name, so the first "<dir>/package.json" regular file is the
// one npm reads. The tarball is the verified file: no second fetch.
function packageJSON(file) {
  const buf = zlib.gunzipSync(fs.readFileSync(file));
  const field = (h, from, to) => h.subarray(from, to).toString("utf8").replace(/\0[\s\S]*$/, "");
  let longName = null;
  for (let off = 0; off + 512 <= buf.length;) {
    const h = buf.subarray(off, off + 512);
    if (h.every((b) => b === 0)) {
      break;
    }
    let name = field(h, 0, 100);
    const prefix = field(h, 345, 500);
    if (prefix) {
      name = prefix + "/" + name;
    }
    if (longName !== null) {
      name = longName;
      longName = null;
    }
    const size = parseInt(field(h, 124, 136).trim() || "0", 8);
    const type = h[156];
    const body = buf.subarray(off + 512, off + 512 + size);
    if (type === 0x4c) { // "L": a GNU long name for the next entry
      longName = body.toString("utf8").replace(/\0[\s\S]*$/, "");
    } else if ((type === 0x30 || type === 0) && /^[^/]+\/package\.json$/.test(name)) {
      return body.toString("utf8");
    }
    off += 512 + Math.ceil(size / 512) * 512;
  }
  return null;
}

// npmSemver is the semver module npm itself checks engines with, found
// beside this image's npm (".../node_modules/npm/bin/npm-cli.js").
function npmSemver() {
  for (const dir of (process.env.PATH || "").split(path.delimiter)) {
    const bin = path.join(dir, "npm");
    if (!fs.existsSync(bin)) {
      continue;
    }
    try {
      return require(path.join(path.dirname(path.dirname(fs.realpathSync(bin))), "node_modules", "semver"));
    } catch {
      return null;
    }
  }
  return null;
}

// checkEngines refuses, before anything is installed, a package whose
// engines.node this runtime's Node does not satisfy, the way npm's own
// engine-strict would for this one package (prereleases included, as npm
// does). A package that names no engines.node passes.
function checkEngines(file, spec) {
  let range;
  try {
    const text = packageJSON(file);
    range = text === null ? undefined : (JSON.parse(text).engines || {}).node;
  } catch (e) {
    console.log(`mcp install: cannot read the package.json of ${spec} to check its engines: ${e.message}`);
    process.exit(6);
  }
  if (typeof range !== "string" || range.trim() === "") {
    return;
  }
  const semver = npmSemver();
  if (!semver) {
    console.log(`mcp install: cannot check ${spec}'s engines.node ("${range}"): this image's npm carries no semver module`);
    process.exit(6);
  }
  if (!semver.satisfies(process.version, range, { includePrerelease: true })) {
    console.log(`mcp install: ${spec} requires Node ${range} (its engines.node), and this runtime image runs Node ${process.version}; it would install and then fail its tool calls. Pin a version of the server that supports Node ${process.version}, or set the runner policy's images.nodeRuntime to an image whose Node it supports`);
    process.exit(5);
  }
}

function main() {
  const [prefix, pkg, version, registry, integrity] = process.argv.slice(1);
  const spec = `${pkg}@${version}`;
  const want = expected(integrity || "");
  if (!want) {
    console.log(`mcp install: the registry advertised no integrity for ${spec} that can be checked; refusing to install it unverified`);
    process.exit(3);
  }

  // Fresh each time: a tarball left by an earlier failed attempt (another
  // version of the same package) must not be the one checked or installed.
  const dl = path.join(prefix, DOWNLOAD_DIR);
  fs.rmSync(dl, { recursive: true, force: true });
  fs.mkdirSync(dl, { recursive: true });
  run(["npm", "pack", spec, "--registry", registry, "--pack-destination", dl,
    "--ignore-scripts", "--loglevel", "error"]);

  const tarballs = fs.readdirSync(dl).filter((n) => n.endsWith(".tgz"));
  if (tarballs.length !== 1) {
    console.log(`mcp install: npm pack left ${tarballs.length} tarballs for ${spec}, want exactly one`);
    process.exit(3);
  }
  const file = path.join(dl, tarballs[0]);
  const [alg, encoding, digests] = want;
  const got = crypto.createHash(alg).update(fs.readFileSync(file)).digest(encoding);
  if (!digests.includes(got)) {
    console.log(`mcp install: ${tarballs[0]} does not match the integrity the registry advertised for ${spec}`);
    process.exit(4);
  }

  checkEngines(file, spec);

  run(["npm", "install", "--prefix", prefix, file, "--registry", registry,
    "--no-audit", "--no-fund", "--ignore-scripts", "--loglevel", "error"]);
}

main();
