"use strict";
const fs = require("fs");
const posix = require("path").posix;
const crypto = require("crypto");
const cp = require("child_process");
const os = require("os");
const net = require("net");

class OpError extends Error {
  constructor(code, message) { super(message); this.code = code; }
}
function fail(code, message) { throw new OpError(code, message); }

function within(p, roots) {
  return roots.some((r) => p === r || p.startsWith(r === "/" ? "/" : r + "/"));
}

function fsError(err, label) {
  if (err instanceof OpError) return err;
  const c = err && err.code;
  if (c === "ENOENT") return new OpError("not_found", label + " does not exist");
  if (c === "ENOTDIR") return new OpError("invalid_path", label + ": a parent is not a directory");
  if (c === "EISDIR") return new OpError("invalid_path", label + " is a directory");
  if (c === "ELOOP") return new OpError("invalid_path", label + ": too many symlinks");
  if (c === "ENAMETOOLONG") return new OpError("invalid_path", label + ": name too long");
  if (c === "EACCES" || c === "EPERM" || c === "EROFS") return new OpError("policy_denied", label + ": permission denied");
  if (c === "ENOSPC" || c === "EDQUOT" || c === "EFBIG") return new OpError("too_large", label + ": no space left in the sandbox");
  return new OpError("environment_unavailable", label + ": " + (err && err.message ? err.message : String(err)));
}

// Follow symlinks one component at a time — also through missing tails and
// dangling links, so a write cannot land outside via a link that points nowhere yet.
function realPath(logical, roots) {
  let resolved = "/";
  const pending = logical.split("/").filter(Boolean);
  let hops = 0;
  while (pending.length) {
    const part = pending.shift();
    const next = posix.join(resolved, part);
    let st = null;
    try { st = fs.lstatSync(next); } catch (e) {
      if (e.code !== "ENOENT" && e.code !== "ENOTDIR") throw fsError(e, logical);
    }
    if (st && st.isSymbolicLink()) {
      if (++hops > 40) fail("invalid_path", "Too many symlinks resolving " + logical);
      const target = fs.readlinkSync(next);
      const absolute = target.startsWith("/") ? target : posix.join(resolved, target);
      resolved = "/";
      pending.unshift.apply(pending, absolute.split("/").filter(Boolean));
    } else {
      resolved = next;
    }
  }
  const real = posix.normalize(resolved);
  if (!within(real, roots)) {
    fail("path_escape", "\"" + logical + "\" resolves through a symlink to a location outside the workspace");
  }
  return real;
}

function typeOf(st) {
  if (st.isFile()) return "file";
  if (st.isDirectory()) return "directory";
  if (st.isSymbolicLink()) return "symlink";
  return "other";
}

function sha256File(p) {
  const hash = crypto.createHash("sha256");
  const fd = fs.openSync(p, "r");
  const buf = Buffer.allocUnsafe(1 << 20);
  try {
    let n;
    while ((n = fs.readSync(fd, buf, 0, buf.length, null)) > 0) hash.update(buf.subarray(0, n));
  } finally { fs.closeSync(fd); }
  return hash.digest("hex");
}

function globToRegExp(glob) {
  const escaped = glob.replace(/[.+^$\{\}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".");
  return new RegExp("^" + escaped + "$");
}

function byName(a, b) { return a.name.localeCompare(b.name); }

// Make a tree writable by its owner so rm can remove read-only folders an agent created.
function makeWritable(p) {
  let st;
  try { st = fs.lstatSync(p); } catch (_) { return; }
  if (!st.isDirectory()) return;
  try { fs.chmodSync(p, st.mode | 0o700); } catch (_) {}
  let names = [];
  try { names = fs.readdirSync(p); } catch (_) {}
  for (const name of names) makeWritable(posix.join(p, name));
}

const SEARCH_SKIP_DIRS = new Set([".git", "node_modules"]);
const MAX_LINE_CHARS = 1000;

const ops = {
  read(q) {
    const real = realPath(q.path, q.roots);
    let st;
    try { st = fs.statSync(real); } catch (e) { throw fsError(e, q.path); }
    if (!st.isFile()) fail("invalid_path", "\"" + q.path + "\" is a " + typeOf(st) + ", not a file");
    const ranged = q.startLine != null || q.endLine != null;
    if (ranged && st.size > q.rangeMaxBytes) {
      fail("too_large", "\"" + q.path + "\" is " + st.size + " bytes — too large to read by line range (limit " + q.rangeMaxBytes + ")");
    }
    const want = ranged ? Infinity : q.maxBytes;
    const hash = crypto.createHash("sha256");
    const keep = [];
    let kept = 0;
    let size = 0;
    const fd = fs.openSync(real, "r");
    const buf = Buffer.allocUnsafe(1 << 20);
    try {
      let n;
      while ((n = fs.readSync(fd, buf, 0, buf.length, null)) > 0) {
        const chunk = buf.subarray(0, n);
        hash.update(chunk);
        size += n;
        if (kept < want) {
          const take = chunk.subarray(0, Math.min(n, want - kept));
          keep.push(Buffer.from(take));
          kept += take.length;
        }
      }
    } finally { fs.closeSync(fd); }
    const data = Buffer.concat(keep);
    let content;
    let truncated = false;
    if (ranged) {
      const lines = data.toString("utf8").split("\n");
      const start = Math.max(1, q.startLine == null ? 1 : q.startLine);
      const end = Math.min(lines.length, q.endLine == null ? lines.length : q.endLine);
      content = lines.slice(start - 1, end).join("\n");
      truncated = start > 1 || end < lines.length;
      const bytes = Buffer.from(content, "utf8");
      if (bytes.length > q.maxBytes) {
        content = bytes.subarray(0, q.maxBytes).toString("utf8");
        truncated = true;
      }
    } else {
      content = data.toString("utf8");
      truncated = size > data.length;
    }
    return { path: real, content: content, size: size, truncated: truncated, sha256: hash.digest("hex") };
  },

  write(q) {
    const real = realPath(q.path, q.roots);
    let existing = null;
    try { existing = fs.lstatSync(real); } catch (e) {
      if (e.code !== "ENOENT" && e.code !== "ENOTDIR") throw fsError(e, q.path);
    }
    if (existing && !existing.isFile()) fail("invalid_path", "\"" + q.path + "\" is a " + typeOf(existing));
    if (q.expectedSha256 != null) {
      const current = existing ? sha256File(real) : null;
      if (current !== q.expectedSha256) fail("conflict", "\"" + q.path + "\" changed since it was read (hash mismatch)");
    }
    const parent = posix.dirname(real);
    let parentStat = null;
    try { parentStat = fs.statSync(parent); } catch (e) {
      if (e.code !== "ENOENT") throw fsError(e, parent);
    }
    if (!parentStat) {
      if (q.createDirs === false) fail("not_found", "Directory \"" + parent + "\" does not exist");
      try { fs.mkdirSync(parent, { recursive: true }); } catch (e) { throw fsError(e, parent); }
    } else if (!parentStat.isDirectory()) {
      fail("invalid_path", "\"" + parent + "\" is not a directory");
    }
    // Write to a temp file and rename: readers never see a half-written file.
    const data = Buffer.from(q.content, "utf8");
    const tmp = posix.join(parent, ".aec-write-" + process.pid + "-" + crypto.randomBytes(4).toString("hex"));
    try {
      fs.writeFileSync(tmp, data, { flag: "wx", mode: existing ? existing.mode & 0o7777 : 0o644 });
      fs.renameSync(tmp, real);
    } catch (e) {
      try { fs.unlinkSync(tmp); } catch (_) {}
      throw fsError(e, q.path);
    }
    return {
      path: real,
      bytes: data.length,
      sha256: crypto.createHash("sha256").update(data).digest("hex"),
      created: !existing,
    };
  },

  stat(q) {
    const real = realPath(q.path, q.roots);
    let st;
    try { st = fs.statSync(real); } catch (e) {
      if (e.code === "ENOENT" || e.code === "ENOTDIR") return null;
      throw fsError(e, q.path);
    }
    return { path: real, type: typeOf(st), size: st.isFile() ? st.size : 0, modifiedAt: Math.round(st.mtimeMs) };
  },

  list(q) {
    const real = realPath(q.path, q.roots);
    let st;
    try { st = fs.statSync(real); } catch (e) { throw fsError(e, q.path); }
    if (!st.isDirectory()) fail("invalid_path", "\"" + q.path + "\" is not a directory");
    const entries = [];
    let truncated = false;
    const walk = (dir, top) => {
      let children;
      try { children = fs.readdirSync(dir, { withFileTypes: true }); } catch (e) {
        if (top) throw fsError(e, q.path);
        return; // unreadable subfolder: skipped, not fatal
      }
      children.sort(byName);
      for (const d of children) {
        if (!q.includeHidden && d.name.startsWith(".")) continue;
        if (entries.length >= q.maxEntries) { truncated = true; return; }
        const p = posix.join(dir, d.name);
        const type = d.isFile() ? "file" : d.isDirectory() ? "directory" : d.isSymbolicLink() ? "symlink" : "other";
        let size = 0;
        if (type === "file") { try { size = fs.lstatSync(p).size; } catch (_) {} }
        entries.push({ path: p, name: d.name, type: type, size: size });
        // Symlinks are listed, never followed.
        if (q.recursive && type === "directory") walk(p, false);
        if (truncated) return;
      }
    };
    walk(real, true);
    return { entries: entries, truncated: truncated };
  },

  search(q) {
    const base = realPath(q.path, q.roots);
    let matcher;
    if (q.literal === false) {
      let re;
      try { re = new RegExp(q.pattern, q.caseSensitive === false ? "i" : ""); } catch (e) {
        fail("invalid_argument", "Invalid regular expression: " + e.message);
      }
      matcher = (line) => re.test(line);
    } else if (q.caseSensitive === false) {
      const needle = q.pattern.toLowerCase();
      matcher = (line) => line.toLowerCase().includes(needle);
    } else {
      matcher = (line) => line.includes(q.pattern);
    }
    const nameFilter = q.glob ? globToRegExp(q.glob) : null;
    const deadline = Date.now() + q.timeBudgetMs;
    const matches = [];
    let truncated = false;

    const scan = (p, size) => {
      if (nameFilter && !nameFilter.test(posix.basename(p))) return;
      if (size > q.maxFileBytes) return;
      let buf;
      try { buf = fs.readFileSync(p); } catch (_) { return; }
      if (buf.subarray(0, 8000).includes(0)) return; // binary
      const lines = buf.toString("utf8").split("\n");
      for (let i = 0; i < lines.length; i++) {
        if (!matcher(lines[i])) continue;
        if (matches.length >= q.maxMatches) { truncated = true; return; }
        const text = lines[i].length > MAX_LINE_CHARS ? lines[i].slice(0, MAX_LINE_CHARS) : lines[i];
        matches.push({ path: p, line: i + 1, text: text });
      }
    };
    const walk = (dir) => {
      if (Date.now() > deadline) { truncated = true; return; }
      let children;
      try { children = fs.readdirSync(dir, { withFileTypes: true }); } catch (_) { return; }
      children.sort(byName);
      for (const d of children) {
        if (truncated) return;
        const p = posix.join(dir, d.name);
        if (d.isDirectory()) {
          if (!SEARCH_SKIP_DIRS.has(d.name)) walk(p);
        } else if (d.isFile()) {
          let size;
          try { size = fs.lstatSync(p).size; } catch (_) { continue; }
          scan(p, size);
        }
      }
    };

    let st;
    try { st = fs.statSync(base); } catch (e) { throw fsError(e, q.path); }
    if (st.isDirectory()) walk(base);
    else if (st.isFile()) scan(base, st.size);
    return { matches: matches, truncated: truncated };
  },

  mkdir(q) {
    for (const p of q.paths) {
      const real = realPath(p, q.roots);
      try { fs.mkdirSync(real, { recursive: true }); } catch (e) { throw fsError(e, p); }
    }
    return {};
  },

  remove(q) {
    const real = realPath(q.path, q.roots);
    makeWritable(real);
    try { fs.rmSync(real, { recursive: true, force: true }); } catch (e) { throw fsError(e, q.path); }
    return {};
  },
};

// --- Services (A-124 phase 2) ---

const SERVICE_NAME = /^[a-z0-9][a-z0-9-]{0,39}$/;

function serviceDir(workingDir) {
  return "/tmp/aec-services/" + crypto.createHash("sha256").update(workingDir).digest("hex").slice(0, 16);
}

function sleep(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }

function alive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}

// Can a TCP connection be made to the port (IPv4 loopback, then IPv6)?
function probe(port) {
  const tryHost = (host) => new Promise((resolve) => {
    const s = net.connect({ host: host, port: port });
    const done = (ok) => { s.destroy(); resolve(ok); };
    s.setTimeout(500, () => done(false));
    s.once("connect", () => done(true));
    s.once("error", () => done(false));
  });
  return tryHost("127.0.0.1").then((ok) => ok || tryHost("::1"));
}

function portFree(port) {
  return new Promise((resolve) => {
    const srv = net.createServer();
    srv.once("error", () => resolve(false));
    srv.listen(port, "127.0.0.1", () => srv.close(() => resolve(true)));
  });
}

// The name of a service request: it is joined into file names, so only the plain form passes.
function serviceName(q) {
  if (!SERVICE_NAME.test(q.name || "")) fail("invalid_argument", "Service names are lower-case letters, digits and dashes (at most 40)");
  return q.name;
}

function readRecord(dir, name) {
  try { return JSON.parse(fs.readFileSync(posix.join(dir, name + ".json"), "utf8")); } catch (_) { return null; }
}

async function describe(dir, rec) {
  let status = "exited";
  let exitCode;
  if (alive(rec.pid)) {
    status = (await probe(rec.port)) ? "listening" : "starting";
  } else {
    try {
      const text = fs.readFileSync(posix.join(dir, rec.name + ".exit"), "utf8").trim();
      exitCode = text === "" ? null : Number(text);
    } catch (_) { exitCode = null; }
  }
  const out = { name: rec.name, command: rec.command, cwd: rec.cwd, port: rec.port, status: status, startedAt: rec.startedAt };
  if (status === "exited") out.exitCode = exitCode;
  return out;
}

function listRecords(dir) {
  let names = [];
  try { names = fs.readdirSync(dir); } catch (_) { return []; }
  return names.filter((n) => n.endsWith(".json")).sort().map((n) => readRecord(dir, n.slice(0, -5))).filter(Boolean);
}

const serviceOps = {
  async svcstart(q) {
    serviceName(q);
    if (typeof q.command !== "string" || q.command.trim() === "") fail("invalid_argument", "command must be a non-empty string");
    const cwd = realPath(q.cwd, q.roots);
    let st;
    try { st = fs.statSync(cwd); } catch (e) { throw fsError(e, q.cwd); }
    if (!st.isDirectory()) fail("invalid_path", "\"" + q.cwd + "\" is not a directory");
    const dir = serviceDir(q.workingDir);
    fs.mkdirSync(dir, { recursive: true });
    const records = listRecords(dir);
    const running = records.filter((r) => alive(r.pid));
    if (running.some((r) => r.name === q.name)) fail("conflict", "A service named \"" + q.name + "\" is already running \u2014 stop it first");
    if (running.length >= q.maxServices) {
      fail("policy_denied", "At most " + q.maxServices + " services may run at once \u2014 stop one first (" + running.map((r) => r.name).join(", ") + ")");
    }
    let port = q.port;
    if (port != null) {
      if (!(await portFree(port))) fail("conflict", "Port " + port + " is already in use in the sandbox");
    } else {
      for (let i = 0; i < 50 && port == null; i++) {
        const candidate = 20000 + Math.floor(Math.random() * 10000);
        if (await portFree(candidate)) port = candidate;
      }
      if (port == null) fail("environment_unavailable", "No free port found in the sandbox");
    }
    const log = posix.join(dir, q.name + ".log");
    const exitFile = posix.join(dir, q.name + ".exit");
    try { fs.unlinkSync(exitFile); } catch (_) {}
    const fd = fs.openSync(log, "w");
    const env = {
      PATH: process.env.PATH || "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      HOME: process.env.HOME || "/tmp",
      LANG: "C.UTF-8",
    };
    Object.assign(env, q.env || {});
    env.PORT = String(port);
    env.AEC_CMD = q.command;
    env.AEC_EXIT = exitFile;
    // The wrapper records the exit code; the service gets its own process group (detached)
    // and outlives this helper (the container's init reaps it).
    const child = cp.spawn("/bin/sh", ["-c", "eval \"$AEC_CMD\"; echo $? > \"$AEC_EXIT\""], {
      cwd: cwd, env: env, detached: true, stdio: ["ignore", fd, fd],
    });
    child.unref();
    fs.closeSync(fd);
    const rec = { name: q.name, command: q.command, cwd: cwd, port: port, pid: child.pid, startedAt: Date.now() };
    fs.writeFileSync(posix.join(dir, q.name + ".json"), JSON.stringify(rec));
    return describe(dir, rec);
  },

  async svcstop(q) {
    const dir = serviceDir(q.workingDir);
    const rec = readRecord(dir, serviceName(q));
    if (!rec) return null;
    if (alive(rec.pid)) {
      try { process.kill(-rec.pid, "SIGTERM"); } catch (_) {}
      for (let i = 0; i < 30 && alive(rec.pid); i++) await sleep(100);
      if (alive(rec.pid)) { try { process.kill(-rec.pid, "SIGKILL"); } catch (_) {} await sleep(100); }
    }
    return describe(dir, rec);
  },

  async svclist(q) {
    const dir = serviceDir(q.workingDir);
    const out = [];
    for (const rec of listRecords(dir)) out.push(await describe(dir, rec));
    return out;
  },

  svclogs(q) {
    const dir = serviceDir(q.workingDir);
    if (!readRecord(dir, serviceName(q))) fail("not_found", "There is no service named \"" + q.name + "\"");
    const p = posix.join(dir, q.name + ".log");
    let size = 0;
    try { size = fs.statSync(p).size; } catch (_) { return { name: q.name, text: "", truncated: false }; }
    const start = Math.max(0, size - q.maxBytes);
    const buf = Buffer.alloc(size - start);
    const fd = fs.openSync(p, "r");
    try { fs.readSync(fd, buf, 0, buf.length, start); } finally { fs.closeSync(fd); }
    return { name: q.name, text: buf.toString("utf8"), truncated: start > 0 };
  },
};
Object.assign(ops, serviceOps);

function runOperation() {
  const chunks = [];
  process.stdin.on("data", (c) => chunks.push(c));
  process.stdin.on("end", () => {
    const answer = (response) => process.stdout.write(JSON.stringify(response) + "\n", () => process.exit(0));
    const failed = (e) => {
      const err = fsError(e, "operation");
      answer({ ok: false, code: err.code, message: err.message });
    };
    try {
      const q = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      const op = Object.prototype.hasOwnProperty.call(ops, q.op) ? ops[q.op] : null;
      if (!op) fail("invalid_argument", "Unknown operation " + q.op);
      // A-124 phase 2: service operations are async.
      Promise.resolve(op(q)).then((result) => answer({ ok: true, result: result }), failed);
    } catch (e) {
      failed(e);
    }
  });
}

function runConnect() {
  let header = Buffer.alloc(0);
  let socket = null;
  process.stdin.on("data", (chunk) => {
    if (socket) { socket.write(chunk); return; }
    header = Buffer.concat([header, chunk]);
    const nl = header.indexOf(10);
    if (nl === -1) return;
    const q = JSON.parse(header.subarray(0, nl).toString("utf8"));
    const rest = header.subarray(nl + 1);
    process.stdin.pause();
    const open = (host, fallback) => {
      const s = net.connect({ host: host, port: q.port });
      s.once("connect", () => {
        socket = s;
        process.stdout.write("AEC-OK\n");
        if (rest.length) s.write(rest);
        s.on("data", (d) => { if (!process.stdout.write(d)) { s.pause(); process.stdout.once("drain", () => s.resume()); } });
        s.on("end", () => process.stdout.write("", () => process.exit(0)));
        s.on("error", () => process.exit(0));
        process.stdin.resume();
      });
      s.once("error", (e) => {
        if (socket) return;
        if (fallback) { open(fallback, null); return; }
        process.stdout.write("AEC-ERR " + JSON.stringify({ code: "not_found", message: "Nothing listens on port " + q.port + " in the sandbox (" + (e.code || e.message) + ")" }) + "\n", () => process.exit(0));
      });
    };
    open("127.0.0.1", "::1");
  });
  process.stdin.on("end", () => { if (socket) socket.end(); else process.exit(0); });
}

function runExec() {
  let header = "";
  let started = false;
  let child = null;
  let killed = false;
  let finished = false;

  const kill = () => {
    if (!child || !child.pid || killed) return;
    killed = true;
    // Negative pid = the whole process group, so children of the command die too.
    try { process.kill(-child.pid, "SIGKILL"); } catch (_) {
      try { child.kill("SIGKILL"); } catch (_) {}
    }
  };
  const finish = (code) => {
    if (finished) return;
    finished = true;
    process.stdout.write("", () => process.exit(code));
  };

  // The host closes stdin to stop the command (timeout, cancel, output cap).
  process.stdin.on("end", () => {
    if (!started) process.exit(2);
    kill();
  });
  process.stdin.on("data", (chunk) => {
    if (started) return;
    header += chunk.toString("utf8");
    const nl = header.indexOf("\n");
    if (nl === -1) return;
    started = true;
    start(JSON.parse(header.slice(0, nl)));
  });

  function start(q) {
    let cwd;
    try {
      cwd = realPath(q.cwd, q.roots);
      let st;
      try { st = fs.statSync(cwd); } catch (e) { throw fsError(e, q.cwd); }
      if (!st.isDirectory()) fail("invalid_path", "\"" + q.cwd + "\" is not a directory");
    } catch (e) {
      const err = fsError(e, q.cwd);
      process.stdout.write("AEC-ERR " + JSON.stringify({ code: err.code, message: err.message }) + "\n", () => process.exit(0));
      finished = true;
      return;
    }

    // Only the container's PATH/HOME plus what the request passes — never the server's env.
    const env = {
      PATH: process.env.PATH || "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      HOME: process.env.HOME || "/tmp",
      LANG: "C.UTF-8",
    };
    Object.assign(env, q.env || {});
    const argv = q.shell ? ["/bin/sh", "-c", q.command.join(" ")] : q.command;

    process.stdout.write("AEC-OK\n");
    child = cp.spawn(argv[0], argv.slice(1), { cwd: cwd, env: env, detached: true, stdio: ["pipe", "pipe", "pipe"] });

    // One byte over the cap is passed through so the host can tell the output was cut.
    let budget = q.maxOutputBytes + 1;
    const pipe = (src, dst) => {
      src.on("data", (d) => {
        if (budget <= 0) return;
        const piece = d.length > budget ? d.subarray(0, budget) : d;
        budget -= piece.length;
        dst.write(piece);
        if (budget <= 0) kill();
      });
    };
    pipe(child.stdout, process.stdout);
    pipe(child.stderr, process.stderr);
    child.stdin.on("error", () => {});
    child.stdin.end(q.stdin || "");

    // Backstop: the host normally stops the command first.
    const timer = setTimeout(kill, q.timeoutMs + 1000);
    child.on("error", (e) => {
      clearTimeout(timer);
      const notFound = e.code === "ENOENT";
      process.stderr.write(argv[0] + ": " + (notFound ? "command not found" : e.message) + "\n");
      finish(notFound ? 127 : 126);
    });
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      if (!child.pid) return; // spawn failed — reported by "error"
      finish(code != null ? code : 128 + (os.constants.signals[signal] || 9));
    });
  }
}

// Parent folder of a file about to be written: created when missing (unless createDirs is false).
function ensureParent(parent, createDirs) {
  let parentStat = null;
  try { parentStat = fs.statSync(parent); } catch (e) {
    if (e.code !== "ENOENT") throw fsError(e, parent);
  }
  if (!parentStat) {
    if (createDirs === false) fail("not_found", "Directory \"" + parent + "\" does not exist");
    try { fs.mkdirSync(parent, { recursive: true }); } catch (e) { throw fsError(e, parent); }
  } else if (!parentStat.isDirectory()) {
    fail("invalid_path", "\"" + parent + "\" is not a directory");
  }
}

function runReadRaw() {
  const chunks = [];
  process.stdin.on("data", (c) => chunks.push(c));
  process.stdin.on("end", () => {
    let q;
    let real;
    let st;
    try {
      q = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      real = realPath(q.path, q.roots);
      try { st = fs.statSync(real); } catch (e) { throw fsError(e, q.path); }
      if (!st.isFile()) fail("invalid_path", "\"" + q.path + "\" is a " + typeOf(st) + ", not a file");
    } catch (e) {
      const err = fsError(e, q && q.path ? q.path : "operation");
      process.stdout.write("AEC-ERR " + JSON.stringify({ code: err.code, message: err.message }) + "\n", () => process.exit(0));
      return;
    }
    const offset = Math.min(Math.max(0, Math.floor(q.offset || 0)), st.size);
    const rest = st.size - offset;
    const length = q.length == null ? rest : Math.min(Math.max(0, Math.floor(q.length)), rest);
    const head = { path: real, size: st.size, modifiedAt: Math.round(st.mtimeMs), offset: offset, length: length };
    process.stdout.write("AEC-OK " + JSON.stringify(head) + "\n");
    if (length === 0) {
      process.stdout.write("", () => process.exit(0));
      return;
    }
    const src = fs.createReadStream(real, { start: offset, end: offset + length - 1 });
    src.on("error", () => process.exit(1));
    src.on("data", (d) => {
      if (!process.stdout.write(d)) {
        src.pause();
        process.stdout.once("drain", () => src.resume());
      }
    });
    src.on("end", () => process.stdout.write("", () => process.exit(0)));
  });
}

// writeraw: a JSON header line on stdin, then the file's bytes as frames - a 4-byte
// big-endian length and that many bytes; a zero-length frame ends the upload and puts
// the file in place (temp file renamed). stdin closing before that end frame (the client
// was killed, the upload broke off) discards the temp file: a partial file never goes
// into place. The answer is one JSON line on stdout.
function runWriteRaw() {
  let header = Buffer.alloc(0);
  let q = null;
  let real = null;
  let existing = null;
  let tmp = null;
  let fd = null;
  let bytes = 0;
  let answered = false;
  // What is left of the current frame, and the bytes of a length not complete yet.
  let remaining = 0;
  let prefix = Buffer.alloc(0);
  const hash = crypto.createHash("sha256");

  const answer = (response) => {
    if (answered) return;
    answered = true;
    process.stdout.write(JSON.stringify(response) + "\n", () => process.exit(0));
  };
  const abort = (e) => {
    if (fd !== null) { try { fs.closeSync(fd); } catch (_) {} fd = null; }
    if (tmp) { try { fs.unlinkSync(tmp); } catch (_) {} tmp = null; }
    const err = fsError(e, q && q.path ? q.path : "operation");
    answer({ ok: false, code: err.code, message: err.message });
  };
  const begin = () => {
    real = realPath(q.path, q.roots);
    try { existing = fs.lstatSync(real); } catch (e) {
      if (e.code !== "ENOENT" && e.code !== "ENOTDIR") throw fsError(e, q.path);
    }
    if (existing && !existing.isFile()) fail("invalid_path", "\"" + q.path + "\" is a " + typeOf(existing));
    if (existing && q.exclusive) fail("conflict", "\"" + q.path + "\" already exists");
    const parent = posix.dirname(real);
    ensureParent(parent, q.createDirs);
    tmp = posix.join(parent, ".aec-write-" + process.pid + "-" + crypto.randomBytes(4).toString("hex"));
    try { fd = fs.openSync(tmp, "wx", existing ? existing.mode & 0o7777 : 0o644); } catch (e) { tmp = null; throw fsError(e, q.path); }
  };
  const take = (chunk) => {
    if (chunk.length === 0) return;
    bytes += chunk.length;
    if (bytes > q.maxBytes) fail("too_large", "\"" + q.path + "\" is larger than the limit of " + q.maxBytes + " bytes");
    hash.update(chunk);
    let done = 0;
    try {
      while (done < chunk.length) done += fs.writeSync(fd, chunk, done, chunk.length - done);
    } catch (e) { throw fsError(e, q.path); }
  };

  // The upload is complete: the temp file goes into place.
  const commit = () => {
    fs.closeSync(fd);
    fd = null;
    if (q.exclusive) {
      let exists = false;
      try { fs.lstatSync(real); exists = true; } catch (_) {}
      if (exists) fail("conflict", "\"" + q.path + "\" already exists");
    }
    try { fs.renameSync(tmp, real); } catch (e) { throw fsError(e, q.path); }
    tmp = null;
    answer({ ok: true, result: { path: real, bytes: bytes, sha256: hash.digest("hex"), created: !existing } });
  };
  // Frames: the bytes of a frame go to take(); the zero-length frame commits.
  const feed = (buf) => {
    let i = 0;
    while (i < buf.length) {
      if (remaining > 0) {
        const n = Math.min(remaining, buf.length - i);
        take(buf.subarray(i, i + n));
        remaining -= n;
        i += n;
        continue;
      }
      const got = Math.min(4 - prefix.length, buf.length - i);
      prefix = Buffer.concat([prefix, buf.subarray(i, i + got)]);
      i += got;
      if (prefix.length < 4) return;
      remaining = prefix.readUInt32BE(0);
      prefix = Buffer.alloc(0);
      if (remaining === 0) {
        commit();
        return;
      }
    }
  };

  process.stdin.on("data", (chunk) => {
    if (answered) return;
    try {
      if (q) {
        feed(chunk);
        return;
      }
      header = Buffer.concat([header, chunk]);
      const nl = header.indexOf(10);
      if (nl === -1) {
        if (header.length > 1048576) fail("invalid_argument", "Request header too long");
        return;
      }
      q = JSON.parse(header.subarray(0, nl).toString("utf8"));
      const rest = header.subarray(nl + 1);
      header = null;
      begin();
      feed(rest);
    } catch (e) { abort(e); }
  });
  // stdin closed before the end frame: the upload broke off (the client was killed, the
  // body failed), so nothing goes into place.
  process.stdin.on("end", () => {
    if (answered) return;
    try {
      if (!q) fail("invalid_argument", "Missing request header");
      fail("cancelled", "The upload ended before it was complete (" + bytes + " bytes received)");
    } catch (e) { abort(e); }
  });
}

if (process.argv[2] === "exec") runExec();
else if (process.argv[2] === "readraw") runReadRaw();
else if (process.argv[2] === "writeraw") runWriteRaw();
else if (process.argv[2] === "connect") runConnect();
else runOperation();
