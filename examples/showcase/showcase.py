"""Multi-Sandbox Relay and Revocation Kill Switch demos.

`--dry-run` prints an illustrated sequence and does not call Mountable.
A live run needs the `mountable` CLI, FUSE, and `MOUNTABLE_API_KEY` on the
orchestrator. Sandbox processes receive only a one-time ticket on stdin.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import Mapping
from pathlib import Path
from typing import Protocol

API_KEY_ENV = "MOUNTABLE_API_KEY"
EXAMPLE_FS = "k5q2ztr4mvd7wn3xa6jhebcyfu"
ABORT_LINE = "mountable: the mount session was revoked; aborting the mount"
RELAY_CONTRACT = (
    "Dry-run visualization. Playing it does not create a filesystem or call "
    "the Mountable API. Sandbox A writes PLAN.md and exits. Sandbox B mounts "
    "the same directory, implements the plan, and runs the test. The laptop "
    "mounts that directory read-only; a write is rejected."
)
KILL_CONTRACT = (
    "Dry-run visualization, not a new measurement. Playing it does not create "
    "a filesystem or call the Mountable API. Binding contract: no new operation "
    "is admitted more than 15 seconds after revocation; a mutation already "
    "admitted may finish; the CLI aborts the mount within about 20 seconds. "
    "An E2B production check on CLI 0.3.0 on 2026-10-07 saw the mount stop in "
    "2–4 seconds. CLI 0.4.0 has not been re-run there. This replay shows the "
    "abort 4 seconds after revoke as an illustration of that recorded observation."
)
RELAY_RECORDED = (
    "Recorded by examples/showcase/relay.py against a Mountable filesystem. "
    "Sandbox A writes PLAN.md and exits. Sandbox B mounts the same directory, "
    "implements the plan, and runs the test. The laptop mounts that directory "
    "read-only with the orchestrator's login; a write is rejected."
)
KILL_RECORDED = (
    "Recorded by examples/showcase/kill_switch.py against a Mountable filesystem. "
    "measured_abort_ms is the time from sessions revoke until the mount process "
    "reported unmounted. Binding contract: no new operation is admitted more than "
    "15 seconds after revocation; a mutation already admitted may finish; the CLI "
    "aborts the mount within about 20 seconds. An E2B production check on CLI 0.3.0 "
    "on 2026-10-07 saw the mount stop in 2–4 seconds. CLI 0.4.0 has not been re-run "
    "there. This recording is not that E2B check."
)

PLAN = """\
# Adder

Add adder.py with add(left, right) returning left + right.
Add test_adder.py that checks add(2, 3) == 5.
"""

ADDER = """\
def add(left, right):
    return left + right
"""

TEST = """\
import unittest
from adder import add


class AdderTest(unittest.TestCase):
    def test_two_plus_three(self):
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
"""


class ShowcaseError(Exception):
    pass


class UsageError(ShowcaseError):
    pass


def sandbox_env(parent: Mapping[str, str]) -> dict[str, str]:
    """Copy of parent with MOUNTABLE_API_KEY removed, including embedded copies."""
    api_key = parent.get(API_KEY_ENV, "")
    env: dict[str, str] = {}
    for key, value in parent.items():
        if key == API_KEY_ENV or (api_key and api_key in value):
            continue
        env[key] = value
    return env


def redact(text: str, secrets: list[str]) -> str:
    for secret in secrets:
        if secret:
            text = text.replace(secret, "[redacted]")
    return text


def dumps(event: Mapping[str, str]) -> str:
    return json.dumps(event, separators=(",", ":"))


def _pane(title: str, subtitle: str) -> dict[str, object]:
    return {"title": title, "subtitle": subtitle, "lines": []}


def _line(t_ms: int, text: str) -> dict[str, object]:
    return {"t_ms": t_ms, "text": text}


def illustrative_relay() -> dict[str, object]:
    fs = EXAMPLE_FS
    mounted = dumps(
        {
            "event": "mounted",
            "path": "/mnt/work",
            "filesystem_id": fs,
            "mode": "rw",
        }
    )
    unmounted = dumps({"event": "unmounted", "reason": "unmount", "detail": ""})
    laptop_mounted = dumps(
        {
            "event": "mounted",
            "path": "/home/dev/review",
            "filesystem_id": fs,
            "mode": "ro",
        }
    )
    return {
        "id": "relay",
        "title": "Containers die. The workspace lives.",
        "source": "illustrative",
        "measured_abort_ms": None,
        "contract": RELAY_CONTRACT,
        "panes": [
            {
                "title": "Sandbox A — planner",
                "subtitle": "Read-write mount, then the sandbox is destroyed.",
                "lines": [
                    _line(0, "$ mkdir -p /mnt/work"),
                    _line(
                        400,
                        '$ printf \'%s\\n\' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work',
                    ),
                    _line(900, mounted),
                    _line(1400, "$ cat > /mnt/work/PLAN.md <<'EOF'"),
                    _line(1800, PLAN.rstrip()),
                    _line(2200, "EOF"),
                    _line(2600, "$ mountable unmount /mnt/work"),
                    _line(3200, unmounted),
                    _line(3800, "sandbox A destroyed"),
                ],
            },
            {
                "title": "Sandbox B — coder",
                "subtitle": "A new sandbox mounts the same directory.",
                "lines": [
                    _line(4200, "$ mkdir -p /mnt/work"),
                    _line(
                        4800,
                        '$ printf \'%s\\n\' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work',
                    ),
                    _line(5400, mounted),
                    _line(6000, "$ cat /mnt/work/PLAN.md"),
                    _line(6600, PLAN.rstrip()),
                    _line(7200, "$ cat > /mnt/work/adder.py <<'EOF'"),
                    _line(7600, ADDER.rstrip()),
                    _line(8000, "EOF"),
                    _line(8400, "$ cat > /mnt/work/test_adder.py <<'EOF'"),
                    _line(8800, "class AdderTest(unittest.TestCase): ..."),
                    _line(9000, "EOF"),
                    _line(9400, "$ python3 -m unittest test_adder"),
                    _line(10000, "OK"),
                    _line(10600, "$ mountable unmount /mnt/work"),
                    _line(11200, unmounted),
                ],
            },
            {
                "title": "Linux laptop",
                "subtitle": "The same directory, mounted read-only.",
                "lines": [
                    _line(11600, "$ mkdir -p ~/review"),
                    _line(
                        12200,
                        f"$ mountable mount --ro --json {fs} ~/review",
                    ),
                    _line(12800, laptop_mounted),
                    _line(13400, "$ cat ~/review/adder.py"),
                    _line(14000, ADDER.rstrip()),
                    _line(14600, "$ touch ~/review/notes.md"),
                    _line(
                        15200,
                        "touch: cannot touch 'notes.md': Read-only file system",
                    ),
                ],
            },
        ],
    }


def illustrative_kill_switch() -> dict[str, object]:
    mounted = dumps(
        {
            "event": "mounted",
            "path": "/mnt/work",
            "filesystem_id": EXAMPLE_FS,
            "mode": "rw",
        }
    )
    aborted = dumps(
        {
            "event": "unmounted",
            "reason": "revoked",
            "detail": (
                "the mount session was revoked; the mount was aborted and "
                "writes not yet committed may be lost"
            ),
        }
    )
    return {
        "id": "kill-switch",
        "title": "Revoke the mount. The sandbox keeps no API key.",
        "source": "illustrative",
        "measured_abort_ms": None,
        "contract": KILL_CONTRACT,
        "panes": [
            {
                "title": "Untrusted agent sandbox",
                "subtitle": "Ticket on stdin. No API key in this environment.",
                "lines": [
                    _line(0, "$ mkdir -p /mnt/work"),
                    _line(
                        400,
                        '$ printf \'%s\\n\' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work',
                    ),
                    _line(800, mounted),
                    _line(1400, "$ printf note > /mnt/work/note-1.txt"),
                    _line(2000, "$ printf note > /mnt/work/note-2.txt"),
                    _line(2600, "$ printf note > /mnt/work/note-3.txt"),
                    _line(2900, "$ printf sweep > /mnt/work/sweep.txt"),
                    _line(3100, "$ printf after > /mnt/work/note-after.txt"),
                    _line(5200, "# write returned; a mutation admitted before revocation may finish"),
                    _line(7200, ABORT_LINE),
                    _line(7600, aborted),
                ],
            },
            {
                "title": "Customer backend",
                "subtitle": "The API key stays here.",
                "lines": [
                    _line(0, "$ # MOUNTABLE_API_KEY stays on this machine"),
                    _line(3200, "$ mountable sessions revoke ses_agent"),
                    _line(3600, "revoked ses_agent"),
                    _line(
                        4000,
                        "# no new operation is admitted more than 15 seconds after this",
                    ),
                ],
            },
        ],
    }


class Mount(Protocol):
    def wait_mounted(self, timeout: float) -> dict[str, str]: ...

    def wait_done(self, timeout: float) -> dict[str, str]: ...

    def diagnostics(self) -> str: ...


class Backend(Protocol):
    def create_filesystem(self, name: str) -> str: ...

    def ticket(self, fs_id: str, *, read_only: bool, idempotency_key: str) -> tuple[str, str]: ...

    def mount_ticket(self, ticket: str, directory: Path) -> Mount: ...

    def mount_login(self, fs_id: str, directory: Path, *, read_only: bool) -> Mount: ...

    def unmount(self, directory: Path) -> None: ...

    def revoke(self, session_id: str) -> None: ...


class Clock:
    def __init__(self) -> None:
        self.start = time.monotonic()
        self._last = -1

    def ms(self) -> int:
        # Break ties so a replay can show lines in the order they were recorded
        # when several land in the same millisecond.
        now = int((time.monotonic() - self.start) * 1000)
        if now <= self._last:
            now = self._last + 1
        self._last = now
        return now


class Trace:
    def __init__(self, demo_id: str, title: str, contract: str) -> None:
        self.body: dict[str, object] = {
            "id": demo_id,
            "title": title,
            "source": "recorded",
            "measured_abort_ms": None,
            "contract": contract,
            "panes": [],
        }
        self._panes: dict[str, dict[str, object]] = {}
        self.secrets: list[str] = []
        self.clock = Clock()

    def pane(self, key: str, title: str, subtitle: str) -> None:
        pane = _pane(title, subtitle)
        self._panes[key] = pane
        panes = self.body["panes"]
        assert isinstance(panes, list)
        panes.append(pane)

    def say(self, key: str, text: str) -> None:
        lines = self._panes[key]["lines"]
        assert isinstance(lines, list)
        lines.append(_line(self.clock.ms(), redact(text, self.secrets)))


def _prepare_dir(backend: Backend, path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    try:
        backend.unmount(path)
    except ShowcaseError:
        pass


def _write(path: Path, text: str) -> None:
    path.write_text(text)


def run_relay(
    backend: Backend,
    root: Path,
    *,
    fs_id: str | None,
    create: bool,
) -> dict[str, object]:
    trace = Trace("relay", "Containers die. The workspace lives.", RELAY_RECORDED)
    api_key = os.environ.get(API_KEY_ENV, "")
    if api_key:
        trace.secrets.append(api_key)
    trace.pane("planner", "Sandbox A — planner", "Read-write mount, then the sandbox is destroyed.")
    trace.pane("coder", "Sandbox B — coder", "A new sandbox mounts the same directory.")
    trace.pane("laptop", "Linux laptop", "The same directory, mounted read-only.")
    sessions: list[str] = []
    mounts: list[tuple[Mount, Path]] = []
    try:
        if fs_id is None:
            if not create:
                raise ShowcaseError("a filesystem id is required")
            fs_id = backend.create_filesystem("showcase-relay")
        trace.say("planner", f"$ # filesystem {fs_id}")

        planner = root / "sandbox-a"
        _prepare_dir(backend, planner)
        session, ticket = backend.ticket(
            fs_id, read_only=False, idempotency_key=f"showcase-planner-{uuid.uuid4().hex}"
        )
        sessions.append(session)
        trace.secrets.append(ticket)
        trace.say("planner", "$ mkdir -p /mnt/work")
        trace.say(
            "planner",
            "$ printf '%s\\n' \"$MOUNTABLE_TICKET\" | mountable mount --ticket-stdin --json /mnt/work",
        )
        mount = backend.mount_ticket(ticket, planner)
        mounts.append((mount, planner))
        trace.say("planner", dumps(mount.wait_mounted(60)))
        _write(planner / "PLAN.md", PLAN)
        trace.say("planner", "$ cat > /mnt/work/PLAN.md <<'EOF'")
        trace.say("planner", PLAN.rstrip())
        trace.say("planner", "EOF")
        backend.unmount(planner)
        trace.say("planner", dumps(mount.wait_done(60)))
        trace.say("planner", "sandbox A destroyed")

        coder = root / "sandbox-b"
        _prepare_dir(backend, coder)
        session, ticket = backend.ticket(
            fs_id, read_only=False, idempotency_key=f"showcase-coder-{uuid.uuid4().hex}"
        )
        sessions.append(session)
        trace.secrets.append(ticket)
        trace.say("coder", "$ mkdir -p /mnt/work")
        trace.say(
            "coder",
            "$ printf '%s\\n' \"$MOUNTABLE_TICKET\" | mountable mount --ticket-stdin --json /mnt/work",
        )
        mount = backend.mount_ticket(ticket, coder)
        mounts.append((mount, coder))
        trace.say("coder", dumps(mount.wait_mounted(60)))
        plan = (coder / "PLAN.md").read_text()
        if plan != PLAN:
            raise ShowcaseError("coder did not see the planner's PLAN.md")
        trace.say("coder", "$ cat /mnt/work/PLAN.md")
        trace.say("coder", plan.rstrip())
        _write(coder / "adder.py", ADDER)
        _write(coder / "test_adder.py", TEST)
        trace.say("coder", "$ python3 -m unittest test_adder")
        test = subprocess.run(
            [sys.executable, "-m", "unittest", "test_adder"],
            cwd=coder,
            capture_output=True,
            text=True,
            check=False,
        )
        output = (test.stderr or test.stdout).strip()
        trace.say("coder", redact(output, trace.secrets))
        if test.returncode != 0:
            raise ShowcaseError("adder test failed")
        backend.unmount(coder)
        trace.say("coder", dumps(mount.wait_done(60)))

        laptop = root / "review"
        _prepare_dir(backend, laptop)
        trace.say("laptop", "$ mkdir -p ~/review")
        trace.say("laptop", f"$ mountable mount --ro --json {fs_id} ~/review")
        mount = backend.mount_login(fs_id, laptop, read_only=True)
        mounts.append((mount, laptop))
        trace.say("laptop", dumps(mount.wait_mounted(60)))
        seen = (laptop / "adder.py").read_text()
        if seen != ADDER:
            raise ShowcaseError("laptop did not see adder.py")
        trace.say("laptop", "$ cat ~/review/adder.py")
        trace.say("laptop", seen.rstrip())
        trace.say("laptop", "$ touch ~/review/notes.md")
        try:
            _write(laptop / "notes.md", "nope\n")
        except OSError as exc:
            trace.say("laptop", f"touch: cannot touch 'notes.md': {exc.strerror}")
        else:
            raise ShowcaseError("read-only mount accepted a write")
        backend.unmount(laptop)
        trace.say("laptop", dumps(mount.wait_done(60)))
    finally:
        for mount, directory in reversed(mounts):
            try:
                backend.unmount(directory)
            except ShowcaseError:
                pass
            try:
                mount.wait_done(5)
            except ShowcaseError:
                pass
        for session in sessions:
            try:
                backend.revoke(session)
            except ShowcaseError:
                pass
    return trace.body


def run_kill_switch(
    backend: Backend,
    root: Path,
    *,
    fs_id: str | None,
    create: bool,
    pause_s: float = 0.3,
    writes_before_revoke: int = 3,
) -> dict[str, object]:
    trace = Trace(
        "kill-switch",
        "Revoke the mount. The sandbox keeps no API key.",
        KILL_RECORDED,
    )
    api_key = os.environ.get(API_KEY_ENV, "")
    if api_key:
        trace.secrets.append(api_key)
    trace.pane("agent", "Untrusted agent sandbox", "Ticket on stdin. No API key in this environment.")
    trace.pane("backend", "Customer backend", "The API key stays here.")
    sessions: list[str] = []
    try:
        if fs_id is None:
            if not create:
                raise ShowcaseError("a filesystem id is required")
            fs_id = backend.create_filesystem("showcase-kill-switch")
        directory = root / "sandbox"
        _prepare_dir(backend, directory)
        session, ticket = backend.ticket(
            fs_id, read_only=False, idempotency_key=f"showcase-kill-{uuid.uuid4().hex}"
        )
        sessions.append(session)
        trace.secrets.append(ticket)
        trace.say("backend", "$ # MOUNTABLE_API_KEY stays on this machine")
        trace.say("agent", "$ mkdir -p /mnt/work")
        trace.say(
            "agent",
            "$ printf '%s\\n' \"$MOUNTABLE_TICKET\" | mountable mount --ticket-stdin --json /mnt/work",
        )
        mount = backend.mount_ticket(ticket, directory)
        trace.say("agent", dumps(mount.wait_mounted(60)))
        for index in range(writes_before_revoke):
            name = f"note-{index + 1}.txt"
            _write(directory / name, "note\n")
            trace.say("agent", f"$ printf note > /mnt/work/{name}")
            if pause_s:
                time.sleep(pause_s)
        _write(directory / "sweep.txt", "sweep\n")
        trace.say("agent", "$ printf sweep > /mnt/work/sweep.txt")
        # Start this write before revocation. Only a write that began then
        # may be described as already admitted.
        trace.say("agent", "$ printf after > /mnt/work/note-after.txt")
        try:
            _write(directory / "note-after.txt", "after\n")
        except OSError as exc:
            admitted: OSError | None = exc
        else:
            admitted = None
        revoked_at = trace.clock.ms()
        trace.say("backend", f"$ mountable sessions revoke {session}")
        backend.revoke(session)
        trace.say("backend", f"revoked {session}")
        trace.say("backend", "# no new operation is admitted more than 15 seconds after this")
        if admitted is None:
            trace.say("agent", "# write returned; a mutation admitted before revocation may finish")
        else:
            trace.say("agent", f"write: {admitted.strerror}")
        done = mount.wait_done(30)
        trace.body["measured_abort_ms"] = trace.clock.ms() - revoked_at
        for line in mount.diagnostics().splitlines():
            if line.startswith("mountable:"):
                trace.say("agent", line)
        trace.say("agent", dumps({key: done[key] for key in ("event", "reason", "detail") if key in done}))
    finally:
        for session in sessions:
            try:
                backend.revoke(session)
            except ShowcaseError:
                pass
    return trace.body


class _FakeMount:
    def __init__(self, directory: Path, mode: str, cloud: dict[str, str], session_id: str = "") -> None:
        self.directory = directory
        self.mode = mode
        self.cloud = cloud
        self.session_id = session_id
        self.events: list[dict[str, str]] = []
        self._done = False
        if directory.exists():
            os.chmod(directory, 0o755)
            for child in directory.iterdir():
                if child.is_file():
                    child.chmod(0o644)
                    child.unlink()
        directory.mkdir(parents=True, exist_ok=True)
        for name, text in cloud.items():
            (directory / name).write_text(text)
        if mode == "ro":
            for child in directory.iterdir():
                if child.is_file():
                    os.chmod(child, 0o444)
            os.chmod(directory, 0o555)
        self.events.append(
            {
                "event": "mounted",
                "path": str(directory),
                "filesystem_id": EXAMPLE_FS,
                "mode": mode,
            }
        )

    def wait_mounted(self, timeout: float) -> dict[str, str]:
        del timeout
        return self.events[0]

    def wait_done(self, timeout: float) -> dict[str, str]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for event in self.events:
                if event.get("event") == "unmounted":
                    return event
            time.sleep(0.01)
        raise ShowcaseError(f"mount at {self.directory} did not finish")

    def diagnostics(self) -> str:
        if any(event.get("reason") == "revoked" for event in self.events):
            return ABORT_LINE + "\n"
        return ""

    def snapshot(self) -> None:
        if self.mode == "ro" or self._done:
            return
        self.cloud.clear()
        if not self.directory.exists():
            return
        os.chmod(self.directory, 0o755)
        for child in self.directory.iterdir():
            if child.is_file():
                child.chmod(0o644)
                self.cloud[child.name] = child.read_text()

    def finish(self, reason: str, detail: str = "") -> None:
        if self._done:
            return
        self.snapshot()
        if self.directory.exists():
            os.chmod(self.directory, 0o755)
            for child in list(self.directory.iterdir()):
                if child.is_file():
                    child.chmod(0o644)
            if reason == "revoked":
                os.chmod(self.directory, 0o555)
        self.events.append({"event": "unmounted", "reason": reason, "detail": detail})
        self._done = True


class FakeBackend:
    """In-memory stand-in for the CLI. Mount points are ordinary directories."""

    def __init__(self, parent: Mapping[str, str]) -> None:
        self.parent = dict(parent)
        self.api_key = self.parent.get(API_KEY_ENV, "")
        self.cloud: dict[str, str] = {}
        self.mounts: dict[Path, _FakeMount] = {}
        self.events: list[str] = []
        self.sandbox_envs: list[dict[str, str]] = []
        self.login_envs: list[dict[str, str]] = []
        self.revoked: list[str] = []
        self._next = 1
        self.tickets: dict[str, str] = {}
        self.session_by_ticket: dict[str, str] = {}

    def create_filesystem(self, name: str) -> str:
        del name
        if not self.api_key:
            raise ShowcaseError("MOUNTABLE_API_KEY is not set")
        return "fs_showcase"

    def ticket(self, fs_id: str, *, read_only: bool, idempotency_key: str) -> tuple[str, str]:
        del fs_id, read_only, idempotency_key
        if not self.api_key:
            raise ShowcaseError("MOUNTABLE_API_KEY is not set")
        session = f"ses_{self._next}"
        self._next += 1
        ticket = f"mtbltk_fake_{session}"
        self.tickets[session] = ticket
        self.session_by_ticket[ticket] = session
        return session, ticket

    def mount_ticket(self, ticket: str, directory: Path) -> Mount:
        session_id = self.session_by_ticket.get(ticket, "")
        if not session_id:
            raise ShowcaseError("unknown ticket")
        env = sandbox_env(self.parent)
        if self.api_key and (API_KEY_ENV in env or any(self.api_key in value for value in env.values())):
            raise ShowcaseError("sandbox environment still contains the API key")
        self.sandbox_envs.append(env)
        self.events.append("mount-ticket")
        mount = _FakeMount(directory, "rw", self.cloud, session_id)
        self.mounts[directory] = mount
        return mount

    def mount_login(self, fs_id: str, directory: Path, *, read_only: bool) -> Mount:
        del fs_id
        if not self.api_key:
            raise ShowcaseError("MOUNTABLE_API_KEY is not set")
        self.login_envs.append(dict(self.parent))
        self.events.append("mount-login")
        mount = _FakeMount(directory, "ro" if read_only else "rw", self.cloud)
        self.mounts[directory] = mount
        return mount

    def unmount(self, directory: Path) -> None:
        mount = self.mounts.get(directory)
        if mount is None:
            raise ShowcaseError(f"{directory} is not mounted")
        self.events.append("unmount")
        mount.finish("unmount")

    def revoke(self, session_id: str) -> None:
        self.revoked.append(session_id)
        self.tickets.pop(session_id, None)
        for mount in self.mounts.values():
            if mount.session_id == session_id and not mount._done:
                mount.finish(
                    "revoked",
                    "the mount session was revoked; the mount was aborted and writes not yet committed may be lost",
                )


class _ProcessMount:
    def __init__(self, proc: subprocess.Popen[str]) -> None:
        self.proc = proc
        self.lines: list[str] = []
        self.stderr_lines: list[str] = []
        self._lock = threading.Lock()
        threading.Thread(target=self._pump, args=(proc.stdout, self.lines), daemon=True).start()
        threading.Thread(target=self._pump, args=(proc.stderr, self.stderr_lines), daemon=True).start()

    def _pump(self, pipe: object, dest: list[str]) -> None:
        if pipe is None:
            return
        for line in pipe:  # type: ignore[union-attr]
            with self._lock:
                dest.append(str(line).rstrip("\n"))

    def _events(self) -> list[dict[str, str]]:
        with self._lock:
            snapshot = list(self.lines)
        events: list[dict[str, str]] = []
        for line in snapshot:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(event, dict):
                events.append({str(key): str(value) for key, value in event.items()})
        return events

    def wait_mounted(self, timeout: float) -> dict[str, str]:
        return self._wait("mounted", timeout)

    def wait_done(self, timeout: float) -> dict[str, str]:
        return self._wait("unmounted", timeout)

    def _wait(self, name: str, timeout: float) -> dict[str, str]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for event in self._events():
                if event.get("event") == name:
                    return event
            if self.proc.poll() is not None and name == "mounted":
                raise ShowcaseError(self.diagnostics() or "mount exited before it was ready")
            time.sleep(0.05)
        raise ShowcaseError(f"timed out waiting for {name}")

    def diagnostics(self) -> str:
        with self._lock:
            return "\n".join(self.stderr_lines)


class LiveBackend:
    def __init__(self, parent: Mapping[str, str], binary: str) -> None:
        self.parent = dict(parent)
        self.binary = binary
        self.api_key = self.parent.get(API_KEY_ENV, "")

    def _run(self, argv: list[str], *, env: Mapping[str, str], stdin: str | None = None) -> subprocess.CompletedProcess[str]:
        try:
            completed = subprocess.run(
                argv,
                env=dict(env),
                input=stdin,
                capture_output=True,
                text=True,
                timeout=60,
                check=False,
            )
        except subprocess.TimeoutExpired as exc:
            raise ShowcaseError(f"timed out: {' '.join(argv)}") from exc
        except FileNotFoundError as exc:
            raise ShowcaseError(f"{self.binary} was not found on PATH") from exc
        if completed.returncode != 0:
            detail = redact(completed.stderr.strip() or completed.stdout.strip(), [self.api_key])
            raise ShowcaseError(detail or f"{argv[0]} failed")
        return completed

    def _json(self, completed: subprocess.CompletedProcess[str]) -> dict[str, object]:
        try:
            parsed = json.loads(completed.stdout)
        except json.JSONDecodeError as exc:
            raise ShowcaseError("command did not return JSON") from exc
        if not isinstance(parsed, dict):
            raise ShowcaseError("command did not return an object")
        return parsed

    def create_filesystem(self, name: str) -> str:
        completed = self._run([self.binary, "fs", "create", name, "--json"], env=self.parent)
        parsed = self._json(completed)
        fs_id = parsed.get("id")
        if not isinstance(fs_id, str) or not fs_id:
            raise ShowcaseError("fs create returned no id")
        return fs_id

    def ticket(self, fs_id: str, *, read_only: bool, idempotency_key: str) -> tuple[str, str]:
        argv = [self.binary, "ticket", "create", fs_id, "--idempotency-key", idempotency_key, "--json"]
        if read_only:
            argv.insert(-1, "--ro")
        completed = self._run(argv, env=self.parent)
        parsed = self._json(completed)
        session = parsed.get("id")
        ticket = parsed.get("ticket")
        if not isinstance(session, str) or not session:
            raise ShowcaseError("ticket create returned no session id")
        if not isinstance(ticket, str) or not ticket:
            raise ShowcaseError("ticket create returned no ticket")
        return session, ticket

    def _start(self, argv: list[str], *, env: Mapping[str, str], stdin: str | None) -> _ProcessMount:
        try:
            proc = subprocess.Popen(
                argv,
                env=dict(env),
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
        except FileNotFoundError as exc:
            raise ShowcaseError(f"{self.binary} was not found on PATH") from exc
        if stdin is not None and proc.stdin is not None:
            proc.stdin.write(stdin if stdin.endswith("\n") else stdin + "\n")
            proc.stdin.close()
        return _ProcessMount(proc)

    def mount_ticket(self, ticket: str, directory: Path) -> Mount:
        env = sandbox_env(self.parent)
        return self._start(
            [self.binary, "mount", "--ticket-stdin", "--json", str(directory)],
            env=env,
            stdin=ticket,
        )

    def mount_login(self, fs_id: str, directory: Path, *, read_only: bool) -> Mount:
        argv = [self.binary, "mount", "--json", fs_id, str(directory)]
        if read_only:
            argv.insert(2, "--ro")
        return self._start(argv, env=self.parent, stdin=None)

    def unmount(self, directory: Path) -> None:
        self._run([self.binary, "unmount", str(directory)], env=sandbox_env(self.parent))

    def revoke(self, session_id: str) -> None:
        completed = self._run([self.binary, "sessions", "revoke", session_id, "--json"], env=self.parent)
        parsed = self._json(completed)
        if parsed.get("revoked") is not True:
            raise ShowcaseError("sessions revoke did not confirm revocation")


def format_script(trace: Mapping[str, object]) -> str:
    blocks: list[str] = []
    panes = trace["panes"]
    assert isinstance(panes, list)
    for pane in panes:
        assert isinstance(pane, dict)
        blocks.append(f"## {pane['title']}")
        lines = pane["lines"]
        assert isinstance(lines, list)
        for line in lines:
            assert isinstance(line, dict)
            blocks.append(str(line["text"]))
        blocks.append("")
    return "\n".join(blocks).rstrip() + "\n"


def _emit(trace: dict[str, object], trace_path: Path | None) -> None:
    sys.stdout.write(format_script(trace))
    if trace_path is not None:
        trace_path.parent.mkdir(parents=True, exist_ok=True)
        trace_path.write_text(json.dumps(trace, indent=2) + "\n")


def _parser(demo: str) -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=demo)
    parser.add_argument("--dry-run", action="store_true", help="print the illustrated sequence and call nothing")
    parser.add_argument("--fs", default=os.environ.get("MOUNTABLE_FS_ID"), help="existing filesystem id")
    parser.add_argument(
        "--create",
        action="store_true",
        help="create a filesystem when --fs is omitted; delete it later in the console",
    )
    parser.add_argument("--root", type=Path, help="local directory for mount points")
    parser.add_argument("--trace", type=Path, help="write the trace JSON here")
    parser.add_argument("--cli", default=os.environ.get("MOUNTABLE_BIN", "mountable"), help="mountable binary")
    return parser


def _live(args: argparse.Namespace) -> LiveBackend:
    if not os.environ.get(API_KEY_ENV):
        raise UsageError("MOUNTABLE_API_KEY is not set. Use --dry-run to preview without a filesystem.")
    if not args.fs and not args.create:
        raise UsageError(
            "Pass --fs FS_ID or --create. Creating a filesystem leaves stored usage until you delete it in the console."
        )
    return LiveBackend(os.environ, args.cli)


def _finish(trace: dict[str, object], trace_path: Path | None) -> int:
    _emit(trace, trace_path)
    return 0


def _fail(exc: ShowcaseError) -> int:
    print(f"showcase: {exc}", file=sys.stderr)
    return 2 if isinstance(exc, UsageError) else 1


def relay_main(argv: list[str] | None = None) -> int:
    parser = _parser("Multi-Sandbox Agent Relay")
    args = parser.parse_args(argv)
    try:
        if args.dry_run:
            _emit(illustrative_relay(), args.trace)
            return 0
        backend = _live(args)
        root = args.root or Path("/tmp/mountable-showcase-relay")
        trace = run_relay(backend, root, fs_id=args.fs, create=args.create)
    except ShowcaseError as exc:
        return _fail(exc)
    return _finish(trace, args.trace)


def kill_switch_main(argv: list[str] | None = None) -> int:
    parser = _parser("15-second revocation kill switch")
    args = parser.parse_args(argv)
    try:
        if args.dry_run:
            _emit(illustrative_kill_switch(), args.trace)
            return 0
        backend = _live(args)
        root = args.root or Path("/tmp/mountable-showcase-kill-switch")
        trace = run_kill_switch(backend, root, fs_id=args.fs, create=args.create)
    except ShowcaseError as exc:
        return _fail(exc)
    measured = trace.get("measured_abort_ms")
    print(
        "showcase: mount aborted "
        f"{measured} ms after revoke "
        "(contract: no new admission after 15 s; CLI abort within about 20 s)",
        file=sys.stderr,
    )
    return _finish(trace, args.trace)
