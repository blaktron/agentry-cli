// The npm MCP-server builder. It runs in the policy's Node
// runtime image as
//
//   node -e <this file> PREFIX PACKAGE VERSION REGISTRY INTEGRITY
//
// so every value arrives as its own argv element and reaches npm as one, never
// through a shell. The pinned package's tarball is fetched with `npm pack`
// (which keeps the registry's bytes), hashed here against the integrity the
// runner recorded when it resolved the pin, and only then installed — from that
// verified file, never from a second fetch. This is the npm half of what the
// PyPI builder does for wheels. The dependency closure is still fetched and
// checked by npm itself against the registry's own metadata: nothing Agentry
// publishes pins it yet.
//
// The verified tarball's own `engines.node` is checked against this runtime's
// Node before the install: npm only warns (EBADENGINE), and a server built for
// a newer Node installs, starts and then fails every tool call (`Promise.try is
// not a function` on Node 22).
//
// The install is written inside the container's own filesystem, at PREFIX,
// and the builder ends by writing that tree to stdout as one tar archive,
// which the runner unpacks into the server's store. Everything else it and
// npm print goes to stderr. The tree holds no link, which a store on Windows
// could not keep: each command npm links into a node_modules/.bin folder
// becomes a two-line shell script that runs the command's file by its
// absolute path, as the link did.
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
  // npm's own output joins this program's on stderr: stdout carries the
  // archive alone.
  const r = spawnSync(argv[0], argv.slice(1), { stdio: ["ignore", 2, 2] });
  if (r.error) {
    console.error(`mcp install: ${argv[0]}: ${r.error.message}`);
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
// when it has none. npm unpacks a tarball with its top-level directory
// stripped, whatever that directory is called ("package/" by convention),
// so every "<dir>/package.json" lands on one path and the last one written
// is what npm installs: that is the one returned. Names come from the
// ustar prefix, a GNU long name or a pax path record, as npm's tar reads
// them. A header this reader cannot size throws, and the check reports it
// rather than passing the package. The tarball is the verified file: no
// second fetch, and nothing is written to disk.
function packageJSON(file) {
  const buf = zlib.gunzipSync(fs.readFileSync(file));
  const cstr = (b) => b.toString("utf8").replace(/\0[\s\S]*$/, "");
  let next = null; // the next entry's name, from an "L" or "x" record
  let found = null;
  for (let off = 0; off + 512 <= buf.length;) {
    const h = buf.subarray(off, off + 512);
    if (h.every((b) => b === 0)) {
      break;
    }
    const sizeField = cstr(h.subarray(124, 136)).trim();
    if (h[124] & 0x80 || !/^[0-7]*$/.test(sizeField)) {
      throw new Error(`a tar header at byte ${off} has a size this check cannot read`);
    }
    const size = parseInt(sizeField || "0", 8);
    let name = cstr(h.subarray(0, 100));
    if (h.subarray(257, 263).toString("latin1") === "ustar\0") {
      const prefix = cstr(h.subarray(345, 500));
      if (prefix) {
        name = prefix + "/" + name;
      }
    }
    if (next !== null) {
      name = next;
      next = null;
    }
    const type = h[156];
    const body = buf.subarray(off + 512, off + 512 + size);
    if (type === 0x4c) { // "L": a GNU long name for the next entry
      next = cstr(body);
    } else if (type === 0x78) { // "x": pax records for the next entry
      const m = /(?:^|\n)\d+ path=([^\n]*)\n/.exec(body.toString("utf8"));
      if (m) {
        next = m[1];
      }
    } else if ((type === 0x30 || type === 0) && /^(?:\.\/)?[^/]+\/package\.json$/.test(name)) {
      found = body.toString("utf8");
    }
    off += 512 + Math.ceil(size / 512) * 512;
  }
  return found;
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
    console.error(`mcp install: cannot read the package.json of ${spec} to check its engines: ${e.message}`);
    process.exit(6);
  }
  if (typeof range !== "string" || range.trim() === "") {
    return;
  }
  const semver = npmSemver();
  if (!semver) {
    console.error(`mcp install: cannot check ${spec}'s engines.node ("${range}"): this image's npm carries no semver module`);
    process.exit(6);
  }
  if (semver.validRange(range) === null) {
    console.error(`mcp install: ${spec} gives engines.node "${range}", which is not a range npm can read, so no Node satisfies it (npm's engine-strict refuses it the same way); pin a version of the server that names a valid range`);
    process.exit(5);
  }
  if (!semver.satisfies(process.version, range, { includePrerelease: true })) {
    console.error(`mcp install: ${spec} requires Node ${range} (its engines.node), and this runtime image runs Node ${process.version}; it would install and then fail its tool calls. Pin a version of the server that supports Node ${process.version}, or set the runner policy's images.nodeRuntime to an image whose Node it supports`);
    process.exit(5);
  }
}

// binScripts replaces every link in a node_modules/.bin folder under dir with
// a shell script that executes the link's target by its absolute path, so the
// program sees the same file name it saw through the link. A target is
// single-quoted for the shell, its own quotes escaped.
function binScripts(dir) {
  for (const name of fs.readdirSync(dir)) {
    const full = path.join(dir, name);
    const st = fs.lstatSync(full);
    if (!st.isDirectory()) {
      continue;
    }
    if (name === ".bin") {
      for (const bin of fs.readdirSync(full)) {
        const link = path.join(full, bin);
        if (!fs.lstatSync(link).isSymbolicLink()) {
          continue;
        }
        const target = path.resolve(full, fs.readlinkSync(link));
        fs.unlinkSync(link);
        fs.writeFileSync(link, `#!/bin/sh\nexec '${target.replace(/'/g, "'\\''")}' "$@"\n`, { mode: 0o755 });
      }
    } else {
      binScripts(full);
    }
  }
}

// The archive this builder ends with: POSIX ustar, with a pax record for a
// name or a link target longer than its ustar field, as Go's archive/tar
// reads it. Folders, regular files with their executable bits, and links,
// in sorted order, each named relative to the prefix.
function tarHeader(name, type, size, mode, linkname) {
  const h = Buffer.alloc(512);
  const put = (s, off, len) => h.write(s, off, len, "utf8");
  const octal = (n, off, len) => put(n.toString(8).padStart(len - 1, "0") + "\0", off, len);
  put(name, 0, 100);
  octal(mode, 100, 8);
  octal(0, 108, 8);
  octal(0, 116, 8);
  octal(size, 124, 12);
  octal(0, 136, 12);
  h.fill(0x20, 148, 156);
  put(type, 156, 1);
  put(linkname || "", 157, 100);
  put("ustar\0", 257, 6);
  put("00", 263, 2);
  let sum = 0;
  for (const b of h) {
    sum += b;
  }
  put(sum.toString(8).padStart(6, "0") + "\0 ", 148, 8);
  return h;
}

function paxRecord(key, value) {
  // "<length> <key>=<value>\n", where the length counts its own digits.
  const base = Buffer.byteLength(` ${key}=${value}\n`);
  let n = base + String(base).length;
  n = base + String(n).length;
  return `${n} ${key}=${value}\n`;
}

function padding(size) {
  return Buffer.alloc((512 - (size % 512)) % 512);
}

function tarEntry(out, name, type, mode, body, linkname) {
  const size = body ? body.length : 0;
  let records = "";
  if (Buffer.byteLength(name) > 100) {
    records += paxRecord("path", name);
  }
  if (linkname && Buffer.byteLength(linkname) > 100) {
    records += paxRecord("linkpath", linkname);
  }
  if (records) {
    const pax = Buffer.from(records, "utf8");
    out(tarHeader("PaxHeader", "x", pax.length, 0o644, ""));
    out(pax);
    out(padding(pax.length));
  }
  out(tarHeader(Buffer.byteLength(name) > 100 ? name.slice(0, 99) : name, type, size, mode, linkname && Buffer.byteLength(linkname) <= 100 ? linkname : ""));
  if (body) {
    out(body);
    out(padding(size));
  }
}

function archive(prefix, out) {
  const walk = (rel) => {
    const dir = rel ? path.join(prefix, rel) : prefix;
    for (const name of fs.readdirSync(dir).sort()) {
      const r = rel ? `${rel}/${name}` : name;
      const full = path.join(prefix, r);
      const st = fs.lstatSync(full);
      if (st.isSymbolicLink()) {
        tarEntry(out, r, "2", 0o777, null, fs.readlinkSync(full));
      } else if (st.isDirectory()) {
        tarEntry(out, r, "5", 0o755, null, "");
        walk(r);
      } else if (st.isFile()) {
        tarEntry(out, r, "0", st.mode & 0o111 ? 0o755 : 0o644, fs.readFileSync(full), "");
      } else {
        console.error(`mcp install: ${r} is neither a file, a folder nor a link; left out`);
      }
    }
  };
  walk("");
  out(Buffer.alloc(1024));
}

// writeAll writes to stdout synchronously, whatever kind of file it is.
function writeAll(buf) {
  for (let off = 0; off < buf.length;) {
    try {
      off += fs.writeSync(1, buf, off, buf.length - off);
    } catch (e) {
      if (e.code !== "EAGAIN") {
        throw e;
      }
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);
    }
  }
}

function main() {
  const [prefix, pkg, version, registry, integrity] = process.argv.slice(1);
  const spec = `${pkg}@${version}`;
  const want = expected(integrity || "");
  if (!want) {
    console.error(`mcp install: the registry advertised no integrity for ${spec} that can be checked; refusing to install it unverified`);
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
    console.error(`mcp install: npm pack left ${tarballs.length} tarballs for ${spec}, want exactly one`);
    process.exit(3);
  }
  const file = path.join(dl, tarballs[0]);
  const [alg, encoding, digests] = want;
  const got = crypto.createHash(alg).update(fs.readFileSync(file)).digest(encoding);
  if (!digests.includes(got)) {
    console.error(`mcp install: ${tarballs[0]} does not match the integrity the registry advertised for ${spec}`);
    process.exit(4);
  }

  checkEngines(file, spec);

  run(["npm", "install", "--prefix", prefix, file, "--registry", registry,
    "--no-audit", "--no-fund", "--ignore-scripts", "--loglevel", "error"]);

  const modules = path.join(prefix, "node_modules");
  if (fs.existsSync(modules)) {
    binScripts(modules);
  }
  archive(prefix, writeAll);
}

main();
