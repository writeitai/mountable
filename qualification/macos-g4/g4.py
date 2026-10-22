#!/usr/bin/env python3
"""The macOS qualification runner (roadmap G4); the steps are in CHECKLIST.md.

Usage: MOUNTABLE_ORG=ORG_ID G4_FS=FS_ID g4.py [--cli PATH] [--results FILE] GROUP [GROUP ...]

Groups: env, lifecycle, killswitch, writes, renewal, behaviour, limits, dead.
It mounts the qualification filesystem with the CLI's login, appends what it
measured to the results file, and revokes every session it opened.
Standard library only.
"""

import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import threading
import time

ORG = os.environ.get("MOUNTABLE_ORG", "")  # the organisation and the
FS = os.environ.get("G4_FS", "")  # filesystem the qualification uses
HERE = os.path.dirname(os.path.abspath(__file__))

CLI = None  # the CLI binary, set from --cli
REPORT = None  # the results file, set from --results


def run(args, **kw):
    """Runs a command, returning its CompletedProcess (text, captured)."""
    kw.setdefault("capture_output", True)
    kw.setdefault("text", True)
    env = dict(os.environ, MOUNTABLE_ORG=ORG)
    env.update(kw.pop("env", {}))
    return subprocess.run(args, env=env, **kw)


def cli(*args, **kw):
    return run([CLI, *args], **kw)


def cli_json(*args):
    """Runs an API command of the CLI, retrying a transient failure."""
    for attempt in range(4):
        r = cli(*args, "--json")
        if r.returncode == 0:
            return json.loads(r.stdout)
        time.sleep(5 * (attempt + 1))
    raise RuntimeError(f"mountable {' '.join(args)}: {(r.stdout + r.stderr).strip()}")


def now():
    return time.monotonic()


def stamp():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# --- results -----------------------------------------------------------------


class Report:
    """Appends a run's rows to the results file, and echoes them."""

    def __init__(self, path):
        self.path = path
        self.failed = 0

    def write(self, text):
        print(text, flush=True)
        with open(self.path, "a") as f:
            f.write(text + "\n")

    def section(self, title):
        self.write(f"\n### {title} ({stamp()})\n")
        self.write("| Step | Result | Evidence |\n|---|---|---|")

    def row(self, step, ok, evidence):
        verdict = {True: "pass", False: "FAIL", None: "recorded"}[ok]
        if ok is False:
            self.failed += 1
        evidence = str(evidence).replace("|", "\\|").replace("\n", "<br>")
        self.write(f"| {step} | {verdict} | {evidence} |")


# --- sessions and the mount table -------------------------------------------


def active_sessions():
    return {s["id"] for s in cli_json("sessions", "list", FS) if s["state"] == "active"}


def revoke(ids):
    for sid in ids:
        cli("sessions", "revoke", sid)


def mount_entry(path):
    """The mount table line for path, read with getfsstat(MNT_NOWAIT) by
    mount(8), which does not touch the server; None if not mounted."""
    for line in run(["/sbin/mount"]).stdout.splitlines():
        if f" on {path} (" in line:
            return line
    return None


def tmp_socket_dirs():
    base = f"/tmp/mountable-{os.getuid()}"
    return set(os.listdir(base)) if os.path.isdir(base) else set()


class Mount:
    """One `mountable mount` process and what it emitted."""

    def __init__(self, path, ro=False, ticket=None):
        self.path = path
        self.ro = ro
        self.ticket = ticket
        self.events = []
        self.stderr = []
        self.proc = None
        self.session = None
        self.socket = None
        self.started = None
        self.mounted_after = None
        self.exited_at = None

    def start(self, timeout=150):
        os.makedirs(self.path, exist_ok=True)
        before = active_sessions()
        self.tmp_before = tmp_socket_dirs()
        if self.ticket is not None:
            args = [CLI, "mount", "--ticket-stdin", self.path, "--json"]
        else:
            args = [CLI, "mount", *(["--ro"] if self.ro else []), FS, self.path, "--json"]
        self.started = now()
        self.proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                     text=True, env=dict(os.environ, MOUNTABLE_ORG=ORG))
        if self.ticket is not None:
            self.proc.stdin.write(self.ticket)
        self.proc.stdin.close()
        threading.Thread(target=self._read, args=(self.proc.stdout, self.events, True), daemon=True).start()
        threading.Thread(target=self._read, args=(self.proc.stderr, self.stderr, False), daemon=True).start()
        deadline = now() + timeout
        while now() < deadline:
            if self.event("mounted"):
                self.mounted_after = now() - self.started
                break
            if self.proc.poll() is not None:
                time.sleep(0.2)
                raise RuntimeError(f"mount exited {self.proc.returncode}: {self.tail()}")
            time.sleep(0.05)
        else:
            raise RuntimeError(f"no mounted event within {timeout}s: {self.tail()}")
        entry = mount_entry(self.path) or ""
        m = re.match(r"<([^>]+)>:/ on ", entry)
        self.socket = m.group(1) if m else None
        for _ in range(50):
            new = active_sessions() - before
            if new:
                self.session = sorted(new)[0]
                break
            time.sleep(0.2)
        return self

    def _read(self, stream, into, as_json):
        for line in stream:
            line = line.rstrip("\n")
            if as_json:
                try:
                    into.append(json.loads(line))
                except ValueError:
                    into.append({"raw": line})
            else:
                into.append(line)
        if as_json:
            self.exited_at = now()

    def event(self, name):
        for e in self.events:
            if e.get("event") == name:
                return e
        return None

    def tail(self, n=5):
        return " / ".join(self.stderr[-n:])

    def wait(self, timeout):
        try:
            return self.proc.wait(timeout)
        except subprocess.TimeoutExpired:
            return None

    def cache_dir(self):
        # <cache>/nfs/nfs.sock, or /tmp/mountable-<uid>/nfs-*/nfs.sock
        if not self.socket:
            return None
        d = os.path.dirname(self.socket)
        return os.path.dirname(d) if os.path.basename(d) == "nfs" else d

    def leftovers(self):
        """What a stopped mount left behind, as a list of strings."""
        left = []
        if mount_entry(self.path):
            left.append("mount entry")
        if self.proc.poll() is None:
            left.append("process running")
        if self.socket and os.path.exists(os.path.dirname(self.socket)):
            left.append(f"socket dir {os.path.dirname(self.socket)}")
        cache = self.cache_dir()
        if cache and os.path.exists(cache):
            left.append(f"cache dir {cache}")
        extra = tmp_socket_dirs() - self.tmp_before
        if extra:
            left.append(f"/tmp dirs {sorted(extra)}")
        return left

    def end(self):
        """Stops it whatever state it is in, and revokes its session."""
        if self.proc and self.proc.poll() is None:
            cli("unmount", self.path)
            if self.wait(40) is None:
                self.proc.kill()
                self.wait(10)
        if mount_entry(self.path):
            cli("unmount", self.path)
        if self.session:
            revoke([self.session])


def mode_name(ro):
    return "ro" if ro else "rw"


def mounted_with(ro, path=None, **kw):
    path = path or os.path.expanduser(f"~/mountable-g4-{mode_name(ro)}")
    return Mount(path, ro=ro, **kw).start()


# --- groups ------------------------------------------------------------------


def group_env(r):
    r.section("Environment")
    sw = run(["sw_vers"]).stdout.strip().replace("\n", "; ")
    r.row("macOS", None, f"{sw}; kern.osversion {run(['sysctl', '-n', 'kern.osversion']).stdout.strip()}; {os.uname().machine}")
    version = cli("version").stdout.strip()
    rev = run(["git", "-C", HERE, "rev-parse", "HEAD"]).stdout.strip()
    r.row("CLI", None, f"{version}; built from {os.environ.get('G4_CLI_REV', rev)}")
    nfsconf = [l for l in open("/etc/nfs.conf").read().splitlines() if l.strip() and not l.startswith("#")]
    r.row("/etc/nfs.conf", None, "; ".join(nfsconf) or "comments only")
    r.row("Service", None, f"{os.environ.get('MOUNTABLE_API_URL', 'https://mountable.io')}, the qualification filesystem")
    for ro in (False, True):
        m = mounted_with(ro)
        try:
            entry = mount_entry(m.path)
            nfsstat = run(["/usr/bin/nfsstat", "-m", m.path]).stdout
            current = nfsstat.split("Current mount parameters:")[-1]
            params = re.search(r"NFS parameters: (\S+)", current)
            flags = re.search(r"General mount flags: (.*)", current)
            r.row(f"options in effect ({mode_name(ro)})", None,
                  f"mount table: `{entry}`; nfsstat -m: flags {flags.group(1) if flags else '?'}, `{params.group(1) if params else '?'}`")
        finally:
            m.end()


def options_ok(m):
    """L2: what the mount table and nfsstat say, against the design."""
    entry = mount_entry(m.path) or ""
    flags = entry[entry.find("(") + 1:entry.rfind(")")].split(", ")
    nfsstat = run(["/usr/bin/nfsstat", "-m", m.path]).stdout.split("Current mount parameters:")[-1]
    params = re.search(r"NFS parameters: (\S+)", nfsstat)
    params = params.group(1).split(",") if params else []
    problems = []
    for want in ("nfs", "nodev", "nosuid"):
        if want not in flags:
            problems.append(f"no {want}")
    if ("read-only" in flags) != m.ro:
        problems.append("read-only flag wrong")
    if "asynchronous" in flags:
        problems.append("async")
    for want in ("vers=3", "ticotsord", "hard", "rsize=1048576", "wsize=1048576", "nonfc", "noresvport", "deadtimeout=30"):
        if want not in params:
            problems.append(f"no {want}")
    if "soft" in params:
        problems.append("soft")
    return problems, f"flags {flags}; {','.join(params)}"


def group_lifecycle(r):
    for ro in (False, True):
        mode = mode_name(ro)
        r.section(f"Lifecycle ({mode})")
        # L1, L2, L3
        m = mounted_with(ro)
        try:
            e = m.event("mounted")
            r.row("L1 mount, no sudo", e.get("mode") == mode and m.mounted_after < 120,
                  f"`mounted` after {m.mounted_after:.1f}s, event {e}")
            problems, evidence = options_ok(m)
            r.row("L2 options in effect", not problems, f"{problems or 'as designed'}; {evidence}")
            t0 = now()
            u = cli("unmount", m.path, "--json")
            code = m.wait(40)
            left = m.leftovers()
            ev = m.event("unmounted")
            r.row("L3 `mountable unmount`", u.returncode == 0 and code == 0 and not left and ev is not None,
                  f"unmount exit {u.returncode} `{u.stdout.strip()}`; mount exit {code} after {now() - t0:.1f}s; event {ev}; leftovers {left or 'none'}")
        finally:
            m.end()
        # L4
        m = mounted_with(ro)
        try:
            t0 = now()
            m.proc.send_signal(2)
            code = m.wait(40)
            left = m.leftovers()
            r.row("L4 SIGINT", code == 0 and not left, f"exit {code} after {now() - t0:.1f}s; event {m.event('unmounted')}; leftovers {left or 'none'}")
        finally:
            m.end()
        # L5
        m = mounted_with(ro)
        try:
            t0 = now()
            u = run(["/sbin/umount", m.path])
            code = m.wait(40)
            left = m.leftovers()
            r.row("L5 external `umount`", u.returncode == 0 and code is not None and not left,
                  f"umount exit {u.returncode} {u.stderr.strip()}; CLI exit {code} after {now() - t0:.1f}s; event {m.event('unmounted')}; leftovers {left or 'none'}")
        finally:
            m.end()
        # L6: `mountable unmount` of a busy mount aborts it at once
        m = mounted_with(ro)
        busy = subprocess.Popen(["/bin/sleep", "300"], cwd=m.path)
        try:
            t0 = now()
            u = cli("unmount", m.path, "--json")
            code = m.wait(60)
            took = now() - t0
            left = m.leftovers()
            r.row("L6 busy, `mountable unmount`", u.returncode == 1 and "may be lost" in u.stdout and code is not None and not left and took < 10,
                  f"unmount exit {u.returncode} `{u.stdout.strip()}`; CLI exit {code} after {took:.1f}s; event {m.event('unmounted')}; leftovers {left or 'none'}")
        finally:
            busy.kill()
            m.end()
        # L6: SIGINT of a busy mount: the clean unmount fails, and the
        # mount is forced off at once, as on Linux
        m = mounted_with(ro)
        busy = subprocess.Popen(["/bin/sleep", "300"], cwd=m.path)
        try:
            t0 = now()
            m.proc.send_signal(2)
            serving = []
            while m.proc.poll() is None and now() - t0 < 60:
                serving.append(os.path.isdir(os.path.join(m.path, ".")) and mount_entry(m.path) is not None)
                time.sleep(1)
            code = m.wait(5)
            took = now() - t0
            left = m.leftovers()
            ev = m.event("unmounted") or {}
            r.row("L6 busy, SIGINT", code == 1 and took < 10 and "may be lost" in ev.get("detail", "") and not left,
                  f"CLI exit {code} after {took:.1f}s, mounted for {sum(serving)} of {len(serving)} one-second probes; event {ev}; leftovers {left or 'none'}")
        finally:
            busy.kill()
            m.end()


# --- probes: processes using the mount while it is cut off -------------------

PROBE = r"""
import mmap, os, sys, time
kind, path = sys.argv[1], sys.argv[2]
def out(s):
    print(f"{time.time():.3f} {s}", flush=True)
def step(f):
    try:
        f()
        out("ok")
    except OSError as e:
        out(f"err {e.errno}")
if kind == "fd":
    fd = os.open(path, os.O_RDONLY)
    while True:
        step(lambda: os.pread(fd, 65536, 0))
        time.sleep(0.2)
elif kind == "cwd":
    os.chdir(path)
    while True:
        step(lambda: os.listdir("."))
        time.sleep(0.2)
elif kind == "open":
    while True:
        def read():
            with open(path, "rb") as f:
                f.read(65536)
        step(read)
        time.sleep(0.2)
elif kind == "mmap":
    f = open(path, "rb")
    m = mmap.mmap(f.fileno(), 0, prot=mmap.PROT_READ)
    while True:
        step(lambda: sum(m[i] for i in range(0, len(m), 4096)))
        time.sleep(0.2)
"""


class Probe:
    """A process using the mount one way, reporting each try with its time."""

    def __init__(self, kind, path):
        self.kind = kind
        self.lines = []
        self.proc = subprocess.Popen([sys.executable, "-c", PROBE, kind, path], stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, text=True)
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        for line in self.proc.stdout:
            t, rest = line.split(" ", 1)
            self.lines.append((float(t), rest.strip()))

    def ok(self):
        return any(r == "ok" for _, r in self.lines)

    def summary(self, since):
        """When it last read, and how it first failed, relative to since."""
        oks = [t for t, r in self.lines if r == "ok"]
        errs = [(t, r) for t, r in self.lines if r != "ok"]
        parts = [f"last ok {oks[-1] - since:+.1f}s" if oks else "never ok"]
        if errs:
            parts.append(f"first {errs[0][1]} at {errs[0][0] - since:+.1f}s")
        code = self.proc.poll()
        if code is not None:
            parts.append(f"exited {code}")
        elif not errs:
            parts.append("still running, no error")
        return ", ".join(parts)

    def oks_after(self, t):
        return sum(1 for when, r in self.lines if r == "ok" and when > t)

    def stop(self):
        self.proc.kill()


FIXTURE = "g4-fixture"


def ensure_fixture():
    """A directory with a 4 MiB file, made through a read-write mount."""
    m = mounted_with(False)
    try:
        d = os.path.join(m.path, FIXTURE)
        os.makedirs(d, exist_ok=True)
        data = os.path.join(d, "data.bin")
        if not os.path.exists(data) or os.path.getsize(data) != 4 << 20:
            with open(data, "wb") as f:
                f.write(os.urandom(4 << 20))
                f.flush()
                os.fsync(f.fileno())
    finally:
        m.end()


def cut_off(r, m, step, cut, wait_reason, at=None):
    """Runs the probes on m, cuts it off with cut(), and reports how the
    mount and each probe ended, timed from the cut, or from at when the cut
    is a moment (an expiry)."""
    d = os.path.join(m.path, FIXTURE)
    data = os.path.join(d, "data.bin")
    with open(data, "rb") as f:  # warm data and attributes
        f.read()
    os.listdir(d)
    os.access(data, os.R_OK)
    probes = [Probe("fd", data), Probe("cwd", d), Probe("open", data), Probe("mmap", data)]
    try:
        deadline = now() + 20
        while not all(p.ok() for p in probes) and now() < deadline:
            time.sleep(0.1)
        t0 = time.time()
        cut()
        took = time.time() - t0
        if at is not None:
            t0 = at
        gone = exited = None
        while time.time() - t0 < 90 and (gone is None or exited is None):
            if gone is None and mount_entry(m.path) is None:
                gone = time.time()
            if exited is None and m.proc.poll() is not None:
                exited = time.time()
            time.sleep(0.1)
        time.sleep(3)
        ev = m.event("unmounted") or {}
        leaked = {p.kind: p.oks_after(gone) for p in probes if gone and p.kind != "mmap" and p.oks_after(gone + 0.5)}
        ok = gone is not None and exited is not None and m.proc.returncode == 1 and wait_reason in ev.get("reason", "") and not leaked
        r.row(step, ok,
              f"cut call took {took:.1f}s; mount gone at {(gone - t0) if gone else float('nan'):+.1f}s, CLI exit {m.proc.returncode} at {(exited - t0) if exited else float('nan'):+.1f}s; "
              f"event {ev}; " + "; ".join(f"{p.kind}: {p.summary(t0)}" for p in probes) +
              (f"; reads after the mount was gone: {leaked}" if leaked else "") + f"; leftovers {m.leftovers() or 'none'}")
    finally:
        for p in probes:
            p.stop()


def group_killswitch(r):
    ensure_fixture()
    for ro in (False, True):
        mode = mode_name(ro)
        r.section(f"Kill switch ({mode})")
        m = mounted_with(ro)
        try:
            cut_off(r, m, "K1 revocation, API up", lambda: cli("sessions", "revoke", m.session), "revoked")
        finally:
            m.end()
        t = cli_json("ticket", "create", FS, *(["--ro"] if ro else []), "--max-duration", "2m")
        m = Mount(os.path.expanduser(f"~/mountable-g4-{mode}"), ro=ro, ticket=t["ticket"] + "\n").start()
        m.session = m.session or t["id"]
        expires = datetime.datetime.fromisoformat(t["expires_at"].replace("Z", "+00:00")).timestamp()
        try:
            cut_off(r, m, f"K2 expiry, times from the session's expires_at {t['expires_at']}", lambda: None, "expired", at=expires)
        finally:
            m.end()


# --- writes ------------------------------------------------------------------


def killed(m):
    """SIGKILLs the mount's process, then clears the dead mount it leaves
    as `mountable unmount` does."""
    m.proc.kill()
    m.wait(10)
    u = cli("unmount", m.path)
    return u.returncode, (u.stdout + u.stderr).strip()


def sha(path):
    import hashlib
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def w1_write(path, how, data):
    """Writes data to path the way how says, returning an open fd to close
    later, or None."""
    import fcntl
    import mmap
    if how == "close":
        with open(path, "wb") as f:
            f.write(data)
        return None
    if how == "msync":
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o644)
        os.ftruncate(fd, len(data))
        mm = mmap.mmap(fd, len(data), mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
        mm[:] = data
        mm.flush()  # msync(MS_SYNC)
        mm.close()
        return fd
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | (os.O_SYNC if how == "O_SYNC" else 0)
    fd = os.open(path, flags, 0o644)
    os.write(fd, data)
    if how == "fsync":
        os.fsync(fd)
    elif how == "F_FULLFSYNC":
        fcntl.fcntl(fd, fcntl.F_FULLFSYNC)
    return fd


W2_WRITER = r"""
import hashlib, os, sys, time
path = sys.argv[1]
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
i = 0
while True:
    payload = os.urandom(65536 - 80)
    rec = f"REC {i:08d} {hashlib.sha256(payload).hexdigest()[:64]}\n".encode().ljust(80, b" ") + payload
    os.write(fd, rec)
    os.fsync(fd)
    print(f"acked {i}", flush=True)
    i += 1
"""


def w2_check(path, acked):
    """Every acknowledged record is there, intact and in order; only the
    tail after the last acknowledged one may be partial."""
    import hashlib
    with open(path, "rb") as f:
        data = f.read()
    good = 0
    while len(data) >= (good + 1) * 65536:
        rec = data[good * 65536:(good + 1) * 65536]
        head, payload = rec[:80], rec[80:]
        want = f"REC {good:08d} {hashlib.sha256(payload).hexdigest()[:64]}".encode()
        if not head.startswith(want):
            break
        good += 1
    return good, len(data)


def group_writes(r):
    import random
    r.section("Writes (rw)")
    for how in ("close", "fsync", "F_FULLFSYNC", "msync", "O_SYNC"):
        m = mounted_with(False)
        d = os.path.join(m.path, "g4-writes")
        os.makedirs(d, exist_ok=True)
        path = os.path.join(d, f"w1-{how}")
        data = os.urandom(3 << 20)
        fd = None
        try:
            fd = w1_write(path, how, data)
            code, out = killed(m)
        finally:
            if fd is not None:
                try:
                    os.close(fd)
                except OSError:
                    pass
            m.end()
        m = mounted_with(False)
        try:
            got = sha(path) if os.path.exists(path) else "missing"
            import hashlib
            r.row(f"W1 {how}, then SIGKILL", got == hashlib.sha256(data).hexdigest(),
                  f"after a fresh mount: {'the acknowledged 3 MiB is there' if got == hashlib.sha256(data).hexdigest() else got}; clearing the dead mount: exit {code} {out}")
            os.remove(path)
        finally:
            m.end()
    # W3: a busy mount forced off keeps every write it acknowledged
    m = mounted_with(False)
    busy = subprocess.Popen(["/bin/sleep", "300"], cwd=m.path)
    d = os.path.join(m.path, "g4-writes")
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, "w3-busy")
    data = os.urandom(4 << 20)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
        os.write(fd, data)
        os.fsync(fd)
        os.close(fd)
        m.proc.send_signal(2)
        code = m.wait(40)
    finally:
        busy.kill()
        m.end()
    m = mounted_with(False)
    try:
        import hashlib
        got = sha(path) if os.path.exists(path) else "missing"
        r.row("W3 a busy mount forced off (SIGINT) keeps what it acknowledged", got == hashlib.sha256(data).hexdigest(),
              f"4 MiB written and fsynced, then SIGINT with a process inside: CLI exit {code}; after a fresh mount the data is {'all there' if got == hashlib.sha256(data).hexdigest() else got}")
        os.remove(path)
    finally:
        m.end()
    rounds = int(os.environ.get("G4_W2_ROUNDS", "8"))
    for i in range(rounds):
        m = mounted_with(False)
        d = os.path.join(m.path, "g4-writes")
        os.makedirs(d, exist_ok=True)
        path = os.path.join(d, f"w2-{i}")
        if os.path.exists(path):
            os.remove(path)
        acked = []
        w = subprocess.Popen([sys.executable, "-c", W2_WRITER, path], stdout=subprocess.PIPE, text=True)
        threading.Thread(target=lambda: [acked.append(int(l.split()[1])) for l in w.stdout], daemon=True).start()
        delay = random.uniform(1, 8)
        time.sleep(delay)
        try:
            code, out = killed(m)
        finally:
            w.kill()
            try:
                w.wait(30)
            except subprocess.TimeoutExpired:
                pass
            m.end()
        last = acked[-1] if acked else -1
        m = mounted_with(False)
        try:
            good, size = w2_check(path, last) if os.path.exists(path) else (0, 0)
            r.row(f"W2 random SIGKILL {i + 1}/{rounds} of a write+fsync loop", good >= last + 1,
                  f"killed after {delay:.1f}s; {last + 1} records acknowledged, {good} intact in order, file {size} bytes; clearing: exit {code}")
            os.remove(path)
        finally:
            m.end()
    for i in range(rounds):
        m = mounted_with(False)
        d = os.path.join(m.path, "g4-writes")
        os.makedirs(d, exist_ok=True)
        path = os.path.join(d, f"w2r-{i}")
        if os.path.exists(path):
            os.remove(path)
        acked = []
        w = subprocess.Popen([sys.executable, "-c", W2_WRITER, path], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        threading.Thread(target=lambda: [acked.append(int(l.split()[1])) for l in w.stdout], daemon=True).start()
        time.sleep(random.uniform(1, 4))
        try:
            cli("sessions", "revoke", m.session)
            m.wait(60)
        finally:
            time.sleep(2)
            w.kill()
            try:
                w.wait(30)
            except subprocess.TimeoutExpired:
                pass
            m.end()
        last = acked[-1] if acked else -1
        m = mounted_with(False)
        try:
            good, size = w2_check(path, last) if os.path.exists(path) else (0, 0)
            r.row(f"W2 revocation {i + 1}/{rounds} during a write+fsync loop", good >= last + 1,
                  f"{last + 1} records acknowledged, {good} intact in order, file {size} bytes")
            os.remove(path)
        finally:
            m.end()


# --- the API, with the CLI's login --------------------------------------------


def api(method, path, body=None, token=None):
    """Calls the Mountable API with the login's access token (the CLI
    refreshes it first) or token; returns (status, parsed body)."""
    import urllib.error
    import urllib.request
    if token is None:
        cli("orgs", "list")  # refreshes the login's access token when needed
        creds = os.path.expanduser("~/Library/Application Support/mountable/credentials.json")
        with open(creds) as f:
            token = json.load(f)["access_token"]
    req = urllib.request.Request(os.environ.get("MOUNTABLE_API_URL", "https://mountable.io") + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, raw.decode(errors="replace")


FS_PATH = f"/api/v1/orgs/{ORG}/filesystems/{FS}"


class Limits:
    """Sets the filesystem's limits for a step and restores them after."""

    def __init__(self, **limits):
        self.limits = limits

    def __enter__(self):
        status, fs = api("GET", FS_PATH)
        if status != 200:
            raise RuntimeError(f"reading the filesystem: {status} {fs}")
        self.saved = {k: fs[k] for k in self.limits}
        status, body = api("PATCH", FS_PATH, self.limits)
        if status != 200:
            raise RuntimeError(f"setting {self.limits}: {status} {body}")
        return body

    def __exit__(self, *exc):
        status, body = api("PATCH", FS_PATH, self.saved)
        if status != 200:
            raise RuntimeError(f"restoring {self.saved}: {status} {body}")


def files_list(path):
    """Lists path through the Files API with a short token session."""
    status, s = api("POST", "/api/v1/mount-sessions", {"filesystem_id": FS, "mode": "ro", "max_duration_seconds": 600,
                                                       "idempotency_key": os.urandom(16).hex(), "credential": "token"})
    if status != 201:
        raise RuntimeError(f"token session: {status} {s}")
    try:
        status, body = api("GET", f"/api/v1/filesystems/{FS}/list/{path}", token=s["token"])
        if status != 200:
            raise RuntimeError(f"list {path}: {status} {body}")
        return [os.path.basename(e["path"].rstrip("/")) for e in body["entries"]]
    finally:
        revoke([s["id"]])


# --- renewal -----------------------------------------------------------------


def group_renewal(r):
    ensure_fixture()
    minutes = float(os.environ.get("G4_RENEWAL_MINUTES", "7"))
    r.section(f"Renewal, both modes at once, {minutes:g} minutes (certificates renew every 2 minutes)")
    rw, ro = mounted_with(False), mounted_with(True)
    try:
        errors = {"rw": [], "ro": []}
        rounds = 0
        end = now() + minutes * 60
        probe = os.path.join(rw.path, "g4-writes", "renewal.txt")
        os.makedirs(os.path.dirname(probe), exist_ok=True)
        while now() < end:
            rounds += 1
            try:
                with open(probe, "w") as f:
                    f.write(f"round {rounds}\n")
                    f.flush()
                    os.fsync(f.fileno())
                with open(probe) as f:
                    if f.read() != f"round {rounds}\n":
                        errors["rw"].append(f"round {rounds}: content")
            except OSError as e:
                errors["rw"].append(f"round {rounds}: {e}")
            try:
                with open(os.path.join(ro.path, FIXTURE, "data.bin"), "rb") as f:
                    f.read()
            except OSError as e:
                errors["ro"].append(f"round {rounds}: {e}")
            time.sleep(5)
        for m, mode in ((rw, "rw"), (ro, "ro")):
            renewal = [l for l in m.stderr if "renewal" in l]
            alive = m.proc.poll() is None and mount_entry(m.path) is not None
            r.row(f"O1 renewal ({mode})", alive and not errors[mode] and not renewal,
                  f"{rounds} rounds of I/O every 5 s; mount alive {alive}; errors {errors[mode] or 'none'}; renewal messages {renewal or 'none'}")
        os.remove(probe)
    finally:
        rw.end()
        ro.end()


# --- dead mounts -------------------------------------------------------------


def group_dead(r):
    ensure_fixture()
    for ro in (False, True):
        mode = mode_name(ro)
        r.section(f"Dead mounts ({mode})")
        m = mounted_with(ro)
        data = os.path.join(m.path, FIXTURE, "data.bin")
        with open(data, "rb") as f:
            f.read()  # cached
        dirty_fd = None
        if not ro:
            dirty = os.path.join(m.path, "g4-writes", "dead-dirty.bin")
            os.makedirs(os.path.dirname(dirty), exist_ok=True)
            dirty_fd = os.open(dirty, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
            os.write(dirty_fd, os.urandom(1 << 20))  # not flushed
        probes = [Probe("fd", data), Probe("cwd", os.path.join(m.path, FIXTURE)), Probe("open", data)]
        try:
            deadline = now() + 20
            while not all(p.ok() for p in probes) and now() < deadline:
                time.sleep(0.1)
            t0 = time.time()
            m.proc.kill()
            m.wait(10)
            gone = None
            while time.time() - t0 < 90 and gone is None:
                if mount_entry(m.path) is None:
                    gone = time.time()
                time.sleep(0.2)
            time.sleep(1)
            stuck = [p.kind for p in probes if p.proc.poll() is None and p.lines and p.lines[-1][0] < time.time() - 20]
            u = cli("unmount", m.path)
            left = m.leftovers()
            again = mounted_with(ro)
            try:
                remount_ok = again.event("mounted") is not None
                lost = None
                if not ro:
                    lost = os.path.getsize(dirty) if os.path.exists(dirty) else "missing"
            finally:
                again.end()
            r.row("D1 SIGKILL with cached, open and dirty files", gone is not None and not stuck and remount_ok and not left,
                  f"mount table entry gone {(gone - t0) if gone else float('nan'):.1f}s after the kill (deadtimeout=30); "
                  + "; ".join(f"{p.kind}: {p.summary(t0)}" for p in probes)
                  + f"; probes stuck over 20 s: {stuck or 'none'}; `mountable unmount` exit {u.returncode} {(u.stdout + u.stderr).strip()}; "
                  f"leftovers of the killed mount {left or 'none'}; a new mount at the same path: {'mounted' if remount_ok else 'failed'}"
                  + (f"; the unflushed 1 MiB file after remount: {lost} bytes (unacknowledged, may be lost)" if not ro else ""))
        finally:
            for p in probes:
                p.stop()
            if dirty_fd is not None:
                try:
                    os.close(dirty_fd)
                except OSError:
                    pass
            m.end()


# --- behaviour ---------------------------------------------------------------

SIGBUS_CHILD = r"""
import mmap, os, sys
p = sys.argv[1]
fd = os.open(p, os.O_RDWR)
m = mmap.mmap(fd, 3 * 4096, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
os.ftruncate(fd, 4096)
print("read past the end:", m[2 * 4096], flush=True)
"""


def posix_checks(base, ro):
    """The runner's single-mount checks; yields (name, ok, evidence)."""
    import errno
    import fcntl
    import mmap
    import sqlite3

    def attempt(f):
        try:
            return f(), None
        except OSError as e:
            return None, e.errno

    if ro:
        data = os.path.join(base, FIXTURE, "data.bin")
        with open(data, "rb") as f:
            n = len(f.read())
        yield "read and list", n == 4 << 20 and "data.bin" in os.listdir(os.path.join(base, FIXTURE)), f"{n} bytes"
        for name, f in (("create", lambda: open(os.path.join(base, "x"), "w")), ("mkdir", lambda: os.mkdir(os.path.join(base, "d"))),
                        ("unlink", lambda: os.remove(data)), ("chmod", lambda: os.chmod(data, 0o600)),
                        ("rename", lambda: os.rename(data, data + "2")), ("open for write", lambda: open(data, "r+b"))):
            _, err = attempt(f)
            yield f"{name} refused", err == errno.EROFS, f"errno {err}"
        fd = os.open(data, os.O_RDONLY)
        m = mmap.mmap(fd, 0, prot=mmap.PROT_READ)
        yield "mmap read", len(m[:4096]) == 4096, "read 4096 bytes through a mapping"
        m.close()
        os.close(fd)
        return

    d = os.path.join(base, "g4-posix")
    if os.path.exists(d):
        subprocess.run(["rm", "-rf", d])
    os.makedirs(d)
    j = lambda *n: os.path.join(d, *n)
    with open(j("a"), "wb") as f:
        f.write(b"one")
    _, err = attempt(lambda: os.open(j("a"), os.O_CREAT | os.O_EXCL | os.O_WRONLY))
    yield "O_EXCL on an existing name", err == errno.EEXIST, f"errno {err}"
    # rename over a file another descriptor holds open
    with open(j("b"), "wb") as f:
        f.write(b"old target")
    held = open(j("b"), "rb")
    os.rename(j("a"), j("b"))
    old = held.read()
    held.close()
    with open(j("b"), "rb") as f:
        new = f.read()
    yield "rename over an open file", old == b"old target" and new == b"one", f"open descriptor read {old!r}, the name reads {new!r}"
    # unlink while open
    with open(j("c"), "wb") as f:
        f.write(b"still here")
    held = open(j("c"), "rb")
    os.remove(j("c"))
    gone = not os.path.exists(j("c"))
    content = held.read()
    held.close()
    time.sleep(1.5)
    silly = [n for n in os.listdir(d) if n.startswith(".nfs")]
    yield "unlink while open", gone and content == b"still here" and not silly, f"name gone {gone}, descriptor read {content!r}, .nfs leftovers after close {silly or 'none'}"
    # hard links
    with open(j("h1"), "wb") as f:
        f.write(b"linked")
    os.link(j("h1"), j("h2"))
    with open(j("h1"), "ab") as f:
        f.write(b"+more")
    n2 = os.stat(j("h2")).st_nlink
    with open(j("h2"), "rb") as f:
        via2 = f.read()
    os.remove(j("h1"))
    left = os.stat(j("h2")).st_nlink
    yield "hard link", n2 == 2 and via2 == b"linked+more" and left == 1 and os.stat(j("h2")).st_ino == os.stat(j("h2")).st_ino, f"nlink {n2}, read through the other name {via2!r}, nlink after removing one {left}"
    # symlinks, modes, times, truncation
    os.symlink("h2", j("s"))
    yield "symlink", os.readlink(j("s")) == "h2" and os.path.islink(j("s")), f"readlink {os.readlink(j('s'))!r}"
    os.chmod(j("h2"), 0o640)
    os.utime(j("h2"), (1000000000, 1100000000))
    st = os.stat(j("h2"))
    yield "chmod and utimes", oct(st.st_mode & 0o777) == "0o640" and int(st.st_mtime) == 1100000000 and int(st.st_atime) == 1000000000, f"mode {oct(st.st_mode & 0o777)}, atime {int(st.st_atime)}, mtime {int(st.st_mtime)}"
    with open(j("t"), "wb") as f:
        f.write(b"x" * 10000)
    os.truncate(j("t"), 100)
    os.truncate(j("t"), 5000)
    with open(j("t"), "rb") as f:
        t = f.read()
    yield "truncate down and up", t == b"x" * 100 + b"\0" * 4900, f"{len(t)} bytes, tail zero {t[100:] == bytes(4900)}"
    # directories
    os.mkdir(j("dir"))
    for i in range(300):
        open(j("dir", f"f{i:03d}"), "w").close()
    names = os.listdir(j("dir"))
    listed = len([n for n in names if not n.startswith("._")])
    _, err = attempt(lambda: os.rmdir(j("dir")))
    yield "directories", listed == 300 and err == errno.ENOTEMPTY, f"listed {listed} of 300 (and {len(names) - listed} AppleDouble `._` files macOS made); rmdir of a non-empty one: errno {err}"
    # mmap
    with open(j("m"), "wb") as f:
        f.write(bytes(65536))
    fd = os.open(j("m"), os.O_RDWR)
    mm = mmap.mmap(fd, 65536, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
    mm[0:5] = b"MSYNC"
    mm.flush()
    mm.close()
    os.close(fd)
    fd = os.open(j("m"), os.O_RDONLY)
    _, err = attempt(lambda: mmap.mmap(fd, 65536, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE))
    os.close(fd)
    yield "read-only descriptor refuses a writable shared mapping", err == errno.EACCES, f"errno {err}"
    import tempfile
    local = tempfile.mktemp(prefix="g4-sigbus-")
    outcomes = []
    for path in (j("sigbus"), local):
        with open(path, "wb") as f:
            f.write(bytes(3 * 4096))
        child = run([sys.executable, "-c", SIGBUS_CHILD, path])
        outcomes.append((child.returncode, child.stdout.strip()))
    os.remove(local)
    yield "truncate under a mapping, as on local APFS", outcomes[0] == outcomes[1], f"mount: exit {outcomes[0][0]} {outcomes[0][1]!r}; APFS: exit {outcomes[1][0]} {outcomes[1][1]!r}"
    # modes that forbid what a descriptor already has
    fd = os.open(j("ro-create"), os.O_CREAT | os.O_WRONLY, 0o444)
    w1, e1 = attempt(lambda: os.write(fd, b"created 0444"))
    _, e1c = attempt(lambda: os.close(fd))
    fd = os.open(j("fchmod"), os.O_CREAT | os.O_RDWR, 0o644)
    os.write(fd, b"before ")
    os.fchmod(fd, 0)
    w2, e2 = attempt(lambda: os.write(fd, b"after fchmod 0"))
    os.fsync(fd)
    _, e2c = attempt(lambda: os.close(fd))
    with open(j("ro-create"), "rb") as f:
        c1 = f.read()
    os.chmod(j("fchmod"), 0o600)
    with open(j("fchmod"), "rb") as f:
        c2 = f.read()
    yield "a file created 0444 through a writable descriptor", e1 is None and e1c is None and c1 == b"created 0444", f"write errno {e1}, close errno {e1c}, content {c1!r}"
    yield "writes after fchmod(fd, 0)", e2 is None and e2c is None and c2 == b"before after fchmod 0", f"write errno {e2}, close errno {e2c}, content {c2!r}"
    # locks are local to the mount (locallocks)
    held = []
    for kind, take in (("flock", lambda f: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)),
                       ("fcntl", lambda f: fcntl.lockf(f, fcntl.LOCK_EX | fcntl.LOCK_NB))):
        with open(j(f"lock-{kind}"), "w") as f:
            _, err = attempt(lambda: take(f))
            held.append(f"{kind}: {'taken' if err is None else f'errno {err}'}")
    yield "flock and fcntl locks (local to the mount)", all(h.endswith("taken") for h in held), "; ".join(held)
    # SQLite in both journal modes
    for jmode in ("delete", "wal"):
        db = j(f"g4-{jmode}.db")
        try:
            c = sqlite3.connect(db)
            c.execute(f"pragma journal_mode={jmode}")
            c.execute("create table t (i integer primary key, v text)")
            for k in range(20):
                with c:
                    c.executemany("insert into t (v) values (?)", [(os.urandom(50).hex(),) for _ in range(50)])
            got = c.execute("pragma journal_mode").fetchone()[0]
            check = c.execute("pragma integrity_check").fetchone()[0]
            rows = c.execute("select count(*) from t").fetchone()[0]
            c.close()
            yield f"SQLite, journal_mode={jmode} asked", check == "ok" and rows == 1000, f"journal_mode in effect {got}, {rows} rows, integrity_check {check}"
        except sqlite3.Error as e:
            yield f"SQLite, journal_mode={jmode}", False, repr(e)


def git(*args, cwd):
    return run(["git", "-c", "user.name=g4", "-c", "user.email=g4@example.invalid", *args], cwd=cwd)


def editors(base):
    """Atomic saves; yields (name, ok, evidence)."""
    d = os.path.join(base, "g4-editors")
    if os.path.exists(d):
        subprocess.run(["rm", "-rf", d])
    os.makedirs(d)

    def fresh(name):
        p = os.path.join(d, name)
        with open(p, "w") as f:
            f.write("line one\nline two\n")
        os.chmod(p, 0o640)
        return p

    def result(p, want):
        with open(p) as f:
            got = f.read()
        mode = oct(os.stat(p).st_mode & 0o777)
        strays = sorted(n for n in os.listdir(d) if n not in saves and not n.startswith("._"))
        return got == want and mode == "0o640" and not strays, f"content {'as saved' if got == want else repr(got)}, mode {mode}, other files {strays or 'none'}"

    saves = {"vim.txt", "sed.txt", "perl.txt", "py.txt"}
    p = fresh("vim.txt")
    v = run(["vim", "-Nu", "NONE", "-es", "-c", "%s/two/2/", "-c", "wq", p])
    ok, ev = result(p, "line one\nline 2\n")
    yield "vim", v.returncode == 0 and ok, f"exit {v.returncode}; {ev}"
    p = fresh("sed.txt")
    v = run(["sed", "-i", "", "s/two/2/", p])
    ok, ev = result(p, "line one\nline 2\n")
    yield "sed -i", v.returncode == 0 and ok, f"exit {v.returncode}; {ev}"
    p = fresh("perl.txt")
    v = run(["perl", "-pi", "-e", "s/two/2/", p])
    ok, ev = result(p, "line one\nline 2\n")
    yield "perl -i", v.returncode == 0 and ok, f"exit {v.returncode}; {ev}"
    p = fresh("py.txt")
    tmp = p + ".tmp-save"
    with open(tmp, "w") as f:
        f.write("saved atomically\n")
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, 0o640)
    os.replace(tmp, p)
    dfd = os.open(d, os.O_RDONLY)
    os.fsync(dfd)
    os.close(dfd)
    ok, ev = result(p, "saved atomically\n")
    yield "write a temporary, fsync, rename, fsync the directory", ok, ev


def git_checks(base):
    d = os.path.join(base, "g4-git")
    if os.path.exists(d):
        subprocess.run(["rm", "-rf", d])
    src = os.environ.get("G4_GIT_SOURCE", "file://" + os.path.abspath(os.path.join(HERE, "..", "..")))
    t0 = now()
    c = git("clone", "--depth", "50", "--no-local", src, d, cwd=base)
    yield "git clone", c.returncode == 0, f"{src} --depth 50 in {now() - t0:.1f}s: exit {c.returncode} {c.stderr.strip()[-200:]}"
    if c.returncode:
        return
    with open(os.path.join(d, "G4.md"), "w") as f:
        f.write("qualification\n")
    steps = [("add", ["add", "G4.md"]), ("commit", ["commit", "-qm", "g4"]), ("branch", ["checkout", "-qb", "g4-branch"]),
             ("checkout", ["checkout", "-q", "-"]), ("gc", ["gc", "-q"]), ("fsck", ["fsck", "--full", "--strict"]),
             ("status", ["status", "--porcelain"])]
    for name, args in steps:
        g = git(*args, cwd=d)
        ok = g.returncode == 0 and (name != "status" or g.stdout.strip() == "")
        yield f"git {name}", ok, f"exit {g.returncode} {(g.stdout + g.stderr).strip()[-200:] or 'clean'}"


def finder_copies(base):
    import tempfile
    src = tempfile.mkdtemp(prefix="g4-ditto-")
    p = os.path.join(src, "with-xattr.txt")
    with open(p, "w") as f:
        f.write("has an extended attribute\n")
    run(["xattr", "-w", "com.example.g4", "qualification", p])
    dest = os.path.join(base, "g4-finder")
    if os.path.exists(dest):
        subprocess.run(["rm", "-rf", dest])
    v = run(["ditto", src, dest])
    copied = os.path.join(dest, "with-xattr.txt")
    attr = run(["xattr", "-p", "com.example.g4", copied]).stdout.strip() if os.path.exists(copied) else None
    names = sorted(os.listdir(dest)) if os.path.exists(dest) else []
    yield "ditto with an extended attribute", v.returncode == 0 and attr == "qualification", f"exit {v.returncode}; attribute read back {attr!r}; names on the mount {names}"
    try:
        listed = files_list("g4-finder")
        yield "the `._` file is an ordinary file in a Files API listing", "._with-xattr.txt" in listed, f"listing {listed}"
    except RuntimeError as e:
        yield "the `._` file is an ordinary file in a Files API listing", False, str(e)
    subprocess.run(["rm", "-rf", src])


def group_behaviour(r):
    ensure_fixture()
    for ro in (False, True):
        mode = mode_name(ro)
        r.section(f"Behaviour ({mode})")
        m = mounted_with(ro)
        try:
            for name, ok, ev in posix_checks(m.path, ro):
                r.row(f"B1 {name}", ok, ev)
            if not ro:
                for name, ok, ev in editors(m.path):
                    r.row(f"B2 {name}", ok, ev)
                for name, ok, ev in git_checks(m.path):
                    r.row(f"B3 {name}", ok, ev)
                for name, ok, ev in finder_copies(m.path):
                    r.row(f"B4 {name}", ok, ev)
            else:
                g = git("log", "-1", "--format=%H", cwd=os.path.join(m.path, "g4-git")) if os.path.isdir(os.path.join(m.path, "g4-git")) else None
                if g is not None:
                    r.row("B3 git log and status on a read-only mount", g.returncode == 0, f"exit {g.returncode} {(g.stdout + g.stderr).strip()[-200:]}")
        finally:
            m.end()


# --- limits ------------------------------------------------------------------


def write_until(path, bs, total):
    """Writes total bytes in bs requests, then fsyncs and closes, noting the
    first error of each kind; returns (bytes written, evidence)."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    block = os.urandom(bs)
    done, notes = 0, []
    t0 = now()
    try:
        while done < total:
            os.write(fd, block)
            done += bs
    except OSError as e:
        notes.append(f"write errno {e.errno} after {done >> 20} MiB")
    for name, f in (("fsync", lambda: os.fsync(fd)), ("close", lambda: os.close(fd))):
        try:
            f()
            notes.append(f"{name} ok")
        except OSError as e:
            notes.append(f"{name} errno {e.errno}")
    notes.append(f"{done >> 20} MiB in {now() - t0:.0f}s")
    return done, ", ".join(notes)


def stored_size(rel):
    """The size of rel, a path in the filesystem, as a fresh mount sees it."""
    m = mounted_with(True)
    try:
        p = os.path.join(m.path, rel)
        return os.path.getsize(p) if os.path.exists(p) else None
    finally:
        m.end()


def group_limits(r):
    r.section("Limits (rw writes, ro reads)")
    usage = cli_json("fs", "usage", FS)
    r.row("usage before", None, json.dumps(usage))
    # Li4: entries. Production refuses an entry quota below 10,000, more
    # creates than a run makes at a mount's create rate: then the dev stack.
    quota = usage["entries"] + 6
    status, body = api("PATCH", FS_PATH, {"entry_quota": quota})
    if status == 422:
        r.row("Li4 entry quota", None, f"not run here: an entry quota of {quota} is refused ({body}); runs on the dev stack")
    elif status != 200:
        raise RuntimeError(f"entry quota: {status} {body}")
    else:
        try:
            m = mounted_with(False)
            try:
                d = os.path.join(m.path, "g4-limits")
                os.makedirs(d, exist_ok=True)
                results = []
                for i in range(12):
                    try:
                        with open(os.path.join(d, f"e{i:02d}"), "w") as f:
                            f.write("x")
                        results.append("ok")
                    except OSError as e:
                        results.append(f"errno {e.errno}")
                refused = [x for x in results if x != "ok"]
                r.row("Li4 entry quota", bool(refused) and all(x == "errno 69" for x in refused),
                      f"entry_quota {quota} with {usage['entries']} entries; 12 creates: {results}")
            finally:
                m.end()
        finally:
            api("PATCH", FS_PATH, {"entry_quota": usage["entry_quota"]})
    # Li5: the metadata rate
    with Limits(mutations_per_minute=10):
        m = mounted_with(False)
        try:
            d = os.path.join(m.path, "g4-limits")
            os.makedirs(d, exist_ok=True)
            t0 = now()
            results = []
            for i in range(15):
                try:
                    with open(os.path.join(d, f"r{i:02d}"), "w") as f:
                        f.write("x")
                    results.append("ok")
                except OSError as e:
                    results.append(f"errno {e.errno}")
            r.row("Li5 mutations_per_minute 10", all(x == "ok" for x in results),
                  f"15 creates in {now() - t0:.0f}s: {results}")
        finally:
            m.end()
    # Li3: stored bytes, at 1 MiB then 32 KiB requests; the file stays for Li1
    for bs, name in ((1 << 20, "1MiB"), (32 << 10, "32KiB")):
        retained = cli_json("fs", "usage", FS)["retained_bytes"]
        quota = max(1 << 30, retained + (64 << 20))
        with Limits(byte_quota=quota):
            m = mounted_with(False)
            path = os.path.join(m.path, "g4-limits", f"quota-{name}.bin")
            os.makedirs(os.path.dirname(path), exist_ok=True)
            try:
                done, ev = write_until(path, bs, quota - retained + (128 << 20))
            finally:
                m.end()
            size = stored_size(os.path.relpath(path, m.path))
            r.row(f"Li3 byte quota, {name} requests", size is not None and size <= done and "errno 69" in ev,
                  f"byte_quota {quota} with {retained} retained; the application saw: {ev}; stored afterwards {size}")
    # Li6: the spending cap on writes
    reserved = cli_json("fs", "usage", FS)["reserved_cents"]
    with Limits(spending_cap_cents=reserved):
        m = mounted_with(False)
        path = os.path.join(m.path, "g4-limits", "cap.bin")
        try:
            done, ev = write_until(path, 1 << 20, 256 << 20)
        finally:
            m.end()
        size = stored_size(os.path.relpath(path, m.path))
        r.row("Li6 spending cap on writes", size is not None and size < (256 << 20) and "errno 69" in ev,
              f"spending_cap_cents {reserved} (the reservation); 256 MiB of writes: {ev}; stored afterwards {size}")
    # Li1: the spending cap on reads of uncached data, from a file written
    # before the cap
    m = mounted_with(False)
    try:
        done, ev = write_until(os.path.join(m.path, "g4-limits", "read-src.bin"), 1 << 20, 512 << 20)
    finally:
        m.end()
    reserved = cli_json("fs", "usage", FS)["reserved_cents"]
    with Limits(spending_cap_cents=reserved):
        m = mounted_with(True)
        try:
            src = os.path.join(m.path, "g4-limits", "read-src.bin")
            size = os.path.getsize(src)
            got, err = 0, None
            t0 = now()
            try:
                with open(src, "rb") as f:
                    while True:
                        b = f.read(1 << 20)
                        if not b:
                            break
                        got += len(b)
            except OSError as e:
                err = e.errno
            r.row("Li1 spending cap on reads", err is not None,
                  f"spending_cap_cents {reserved}; of a {size >> 20} MiB file written before the cap ({ev}), read {got >> 20} MiB in {now() - t0:.0f}s, then errno {err}")
        finally:
            m.end()
    m = mounted_with(False)
    try:
        subprocess.run(["rm", "-rf", os.path.join(m.path, "g4-limits")])
    finally:
        m.end()
    r.row("usage after", None, json.dumps(cli_json("fs", "usage", FS)))


# --- run by the owner: isolation, sleep and wake, pjdfstest -------------------

SECOND_USER = None  # from --second-user

ISOLATION_PROBE = r"""
import os, socket, sys
sock, mnt = sys.argv[1], sys.argv[2]
def tri(name, f):
    try:
        f()
        print(f"{name}: allowed")
    except OSError as e:
        print(f"{name}: errno {e.errno}")
def connect():
    s = socket.socket(socket.AF_UNIX)
    s.connect(sock)
tri("connect to the socket", connect)
tri("stat the mountpoint", lambda: os.stat(mnt))
tri("list the root", lambda: os.listdir(mnt))
tri("list a directory", lambda: os.listdir(os.path.join(mnt, "g4-fixture")))
tri("stat a file", lambda: os.stat(os.path.join(mnt, "g4-fixture", "data.bin")))
tri("read a file", lambda: open(os.path.join(mnt, "g4-fixture", "data.bin"), "rb").read(10))
tri("cd into a directory", lambda: os.chdir(os.path.join(mnt, "g4-fixture")))
tri("create a file", lambda: open(os.path.join(mnt, "g4-intruder"), "w"))
"""


def isolation_run(r, label):
    for ro in (False, True):
        path = f"/tmp/g4-isolation-{mode_name(ro)}"  # reachable by another user, unlike a home directory
        if not os.path.isdir(path):
            os.makedirs(path)
        os.chmod(path, 0o755)
        m = mounted_with(ro, path=path)
        try:
            problems, evidence = options_ok(m)
            r.row(f"I {label}, options ({mode_name(ro)})", not problems, f"{problems or 'as designed'}; {evidence}")
            d = os.path.join(m.path, FIXTURE)
            os.listdir(m.path)
            with open(os.path.join(d, "data.bin"), "rb") as f:  # warm caches
                f.read()
            os.stat(os.path.join(d, "data.bin"))
            p = run(["sudo", "-u", SECOND_USER, sys.executable, "-c", ISOLATION_PROBE, m.socket, m.path])
            lines = [l for l in p.stdout.splitlines() if ":" in l]
            allowed = [l for l in lines if l.endswith("allowed") and not l.startswith("stat the mountpoint")]
            r.row(f"I {label}, second user `{SECOND_USER}` ({mode_name(ro)})", p.returncode == 0 and not allowed and len(lines) == 8,
                  "; ".join(lines) + (f"; stderr {p.stderr.strip()}" if p.stderr.strip() else ""))
        finally:
            m.end()


def group_isolation(r):
    if not SECOND_USER:
        raise RuntimeError("isolation needs --second-user, and is run by the owner (it uses sudo)")
    ensure_fixture()
    r.section(f"Isolation (second user {SECOND_USER})")
    isolation_run(r, "I1-I2")
    saved = open("/etc/nfs.conf").read()
    hostile = saved + "\n# g4 qualification, removed afterwards\nnfs.client.mount.options = noopaque_auth,async,nfc,resvport,soft\n"
    try:
        run(["sudo", "/usr/bin/tee", "/etc/nfs.conf"], input=hostile)
        r.row("I3 /etc/nfs.conf", None, "nfs.client.mount.options = noopaque_auth,async,nfc,resvport,soft")
        isolation_run(r, "I3")
    finally:
        run(["sudo", "/usr/bin/tee", "/etc/nfs.conf"], input=saved)
    r.row("I3 /etc/nfs.conf restored", open("/etc/nfs.conf").read() == saved, "byte for byte")


def group_sleep(r):
    ensure_fixture()
    r.section("Sleep and wake (the owner sleeps the Mac)")
    rw, ro = mounted_with(False), mounted_with(True)
    try:
        print("\nPut the Mac to sleep now (Apple menu > Sleep) for at least 5 minutes, then wake it.\n", flush=True)
        probe = os.path.join(rw.path, "g4-writes", "sleep.txt")
        os.makedirs(os.path.dirname(probe), exist_ok=True)
        errors, last, slept, woke = [], time.time(), None, None
        while woke is None or time.time() - woke < 120:
            t = time.time()
            if t - last > 60 and slept is None:
                slept, woke = t - last, t
            last = t
            try:
                with open(probe, "w") as f:
                    f.write(str(t))
                    f.flush()
                    os.fsync(f.fileno())
                with open(os.path.join(ro.path, FIXTURE, "data.bin"), "rb") as f:
                    f.read(65536)
            except OSError as e:
                errors.append(f"{t - (woke or t):+.0f}s after wake: {e}")
            time.sleep(2)
        for m, mode in ((rw, "rw"), (ro, "ro")):
            alive = m.proc.poll() is None and mount_entry(m.path) is not None
            r.row(f"O3 sleep and wake ({mode})", alive and not errors, f"slept {slept:.0f}s; mount alive {alive}; errors {errors or 'none'}; stderr {m.tail(3)}")
    finally:
        rw.end()
        ro.end()


PJDFSTEST = ("https://github.com/pjd/pjdfstest", "85a8aea9e685999ef0540392fd80535f873d7ff7")


def group_pjdfstest(r):
    """Builds the pinned pjdfstest and runs it as root on a read-write
    mount (the owner: it uses sudo)."""
    import tempfile
    r.section(f"pjdfstest {PJDFSTEST[1][:12]} as root (rw)")
    build = tempfile.mkdtemp(prefix="g4-pjdfstest-")
    steps = [["git", "clone", "-q", PJDFSTEST[0], build], ["git", "-C", build, "checkout", "-q", PJDFSTEST[1]],
             ["autoreconf", "-ifs"], ["./configure"], ["make", "pjdfstest"]]
    for s in steps:
        p = run(s, cwd=build if s[0] not in ("git",) else None)
        if p.returncode:
            raise RuntimeError(f"{' '.join(s)}: {p.stderr.strip()[-300:]}")
    m = mounted_with(False)
    try:
        d = os.path.join(m.path, "g4-pjdfstest")
        os.makedirs(d, exist_ok=True)
        os.chmod(d, 0o777)
        p = run(["sudo", "prove", "-r", os.path.join(build, "tests")], cwd=d)
        summary = [l for l in p.stdout.splitlines() if l.startswith(("Failed", "Files=", "Result:")) or "Failed tests" in l or "Wstat" in l]
        r.row("B1 pjdfstest", p.returncode == 0, "; ".join(summary[-40:]) or p.stdout[-500:])
        run(["sudo", "rm", "-rf", d])
    finally:
        m.end()


GROUPS = {
    "env": group_env,
    "lifecycle": group_lifecycle,
    "killswitch": group_killswitch,
    "writes": group_writes,
    "renewal": group_renewal,
    "dead": group_dead,
    "behaviour": group_behaviour,
    "limits": group_limits,
    "isolation": group_isolation,
    "sleep": group_sleep,
    "pjdfstest": group_pjdfstest,
}


def main():
    global CLI, REPORT
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--cli", default=os.environ.get("G4_CLI", "mountable"))
    p.add_argument("--results", default=os.path.join(HERE, "results", datetime.date.today().isoformat() + ".md"))
    p.add_argument("--second-user", help="for isolation: a second local macOS user")
    p.add_argument("groups", nargs="+", choices=sorted(GROUPS))
    a = p.parse_args()
    if not ORG or not FS:
        p.error("set MOUNTABLE_ORG and G4_FS: the organisation and the filesystem to qualify on")
    global SECOND_USER
    SECOND_USER = a.second_user
    CLI = a.cli
    os.makedirs(os.path.dirname(a.results), exist_ok=True)
    report = Report(a.results)
    leftover = active_sessions()
    for g in a.groups:
        try:
            GROUPS[g](report)
        except Exception as e:  # a group that could not finish is a failure
            report.row(f"{g} (aborted)", False, repr(e))
    revoke(active_sessions() - leftover)
    sys.exit(1 if report.failed else 0)


if __name__ == "__main__":
    main()
