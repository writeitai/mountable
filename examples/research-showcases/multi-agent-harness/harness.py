"""Planner/coder/verifier workspace handoff. Offline unless --live is explicit."""

from __future__ import annotations

import argparse
import base64
import binascii
import difflib
import hashlib
import hmac
import json
import os
import queue
import selectors
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import quote, urlsplit

BEFORE = "def add(a, b):\n    return a - b\n"
AFTER = "def add(a, b):\n    return a + b\n"
TEST = "from adder import add\nassert add(2, 3) == 5\nassert add(-2, 3) == 1\n"
PLAN = "Fix add() to add its arguments. Keep the existing tests.\n"
READY = "/READY.json"
LIMITS = {
    "spending_cap_cents": 100,
    "mutations_per_minute": 60,
    "byte_quota": 1073741824,
    "entry_quota": 10000,
}


class DemoError(Exception):
    pass


def signature(secret, msg_id, timestamp, body):
    key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    raw = f"{msg_id}.{timestamp}.".encode() + body
    return "v1," + base64.b64encode(hmac.digest(key, raw, "sha256")).decode()


class Inbox:
    """One short-lived run. Cursor de-duplication is intentionally in memory."""

    def __init__(self, filesystem_id, secret):
        self.filesystem_id = filesystem_id
        self.secret = secret
        self.webhook_id = None
        self.cursor = -1
        self.pending = queue.Queue()

    def accept(self, headers, body):
        if not self.secret or not self.webhook_id:
            return 503  # Registration is finishing; let the sender retry the resync.
        try:
            stamp = headers["webhook-timestamp"]
            if not stamp.isdigit() or abs(time.time() - int(stamp)) > 300:
                return 401
            expected = signature(self.secret, headers["webhook-id"], stamp, body)
            if not any(
                hmac.compare_digest(s, expected)
                for s in headers["webhook-signature"].split()
            ):
                return 401
            message = json.loads(body)
            data = message["data"]
            if (
                data["filesystem_id"] != self.filesystem_id
                or data["webhook_id"] != self.webhook_id
            ):
                return 400
            kind = message["type"]
            cursor = int(data["cursor"])
            if kind not in ("files.changed", "files.resync") or cursor < 0:
                return 400
            events = data["events"]
            if not isinstance(events, list) or any(
                e["type"] not in ("created", "updated", "deleted")
                or not isinstance(e["path"], str)
                for e in events
            ):
                return 400
        except (KeyError, ValueError, TypeError, AttributeError, binascii.Error):
            return 400
        if cursor > self.cursor:
            self.cursor = cursor
            # Reconcile on resync too, so a batched/lost READY event cannot strand a run.
            if kind == "files.resync" or any(e["path"] == READY for e in events):
                self.pending.put(message)
        return 204


@contextmanager
def receiver(inbox, port):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass  # Never log signed bodies or request headers.

        def do_POST(self):
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if self.path != "/hooks/mountable" or not 0 < length <= 262144:
                    status = 400
                else:
                    self.connection.settimeout(5)
                    status = inbox.accept(self.headers, self.rfile.read(length))
            except (ValueError, OSError):
                status = 400
            self.send_response(status)
            self.end_headers()

    server = HTTPServer(("127.0.0.1", port), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server.server_port
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class Control:
    def __init__(self, http):
        self.http = http

    def call(self, method, path, body=None):
        # No mutation retries: a timeout can mean the operation already happened.
        response = self.http.request(method, path, json=body)
        if not response.is_success:
            try:
                payload = response.json()
            except ValueError:
                payload = {}
            fields = payload.get("detail", payload) if isinstance(payload, dict) else {}
            code = (
                fields.get("code", "request_failed")
                if isinstance(fields, dict)
                else "request_failed"
            )
            if code not in (
                "filesystem_spending_cap_reached",
                "rate_limited",
                "spending_cap_reached",
            ):
                code = "request_failed"
            raise DemoError(f"HTTP {response.status_code} {code}; stopping this run")
        return response.json() if response.content else None


def filesystem_path(org_id, fs_id=None):
    path = f"/api/v1/orgs/{quote(org_id, safe='')}/filesystems"
    return path if fs_id is None else path + "/" + quote(fs_id, safe="")


def planner(fs):
    fs.write("/PLAN.md", PLAN)
    fs.write("/adder.py", BEFORE)
    fs.write("/test_adder.py", TEST)


def coder(fs):
    if fs.read("/PLAN.md").data.decode() != PLAN:
        raise DemoError("plan changed")
    fs.write("/adder.py", AFTER)
    # Publish last: only a complete candidate should trigger a verifier.
    fs.write(READY, json.dumps({"sha256": hashlib.sha256(AFTER.encode()).hexdigest()}))


def await_candidate(inbox, fs, timeout=90):
    deadline = time.monotonic() + timeout
    while True:
        try:
            inbox.pending.get(timeout=max(0, deadline - time.monotonic()))
        except queue.Empty as exc:
            raise DemoError(
                "timed out waiting for a signed READY change or resync"
            ) from exc
        try:
            return json.loads(fs.read(READY).data)
        except Exception as exc:
            # The initial resync may precede the coder's marker. Only 404 is harmless.
            if getattr(exc, "status", None) != 404:
                raise


def check_candidate(root, candidate):
    contents = (root / "adder.py").read_bytes()
    if hashlib.sha256(contents).hexdigest() != candidate["sha256"]:
        raise DemoError("candidate changed before verification")
    print(
        "".join(
            difflib.unified_diff(
                BEFORE.splitlines(True),
                contents.decode().splitlines(True),
                fromfile="before/adder.py",
                tofile="after/adder.py",
            )
        ),
        end="",
    )
    env = {"PATH": os.environ.get("PATH", ""), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(
        [sys.executable, "-B", "test_adder.py"],
        cwd=root,
        env=env,
        check=True,
        timeout=30,
        capture_output=True,
    )


@contextmanager
def readonly_mount(client, control, fs_id, root):
    ticket = client.create_mount_ticket(
        fs_id, mode="ro", max_duration_seconds=600, idempotency_key=str(uuid.uuid4())
    )
    process = None
    try:
        if not ticket.ticket:
            raise DemoError(
                "ticket unavailable; do not retry the mount with an empty ticket"
            )
        # An allowlist also keeps parent credentials, including cloud keys, out of the child.
        env = {
            "PATH": os.environ.get("PATH", ""),
            "HOME": str(root.parent / "mount-home"),
        }
        Path(env["HOME"]).mkdir()
        process = subprocess.Popen(
            ["mountable", "mount", "--ro", "--ticket-stdin", "--json", str(root)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            env=env,
        )
        process.stdin.write(ticket.ticket + "\n")
        process.stdin.close()
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            deadline = time.monotonic() + 45
            while True:
                if not selector.select(max(0, deadline - time.monotonic())):
                    raise DemoError("verifier mount readiness timed out")
                line = process.stdout.readline()
                if not line:
                    raise DemoError("verifier mount stopped before readiness")
                if json.loads(line).get("event") == "mounted":
                    break
        yield root
    finally:
        try:
            if process is not None:
                try:
                    result = subprocess.run(
                        ["mountable", "unmount", str(root)],
                        capture_output=True,
                        timeout=15,
                        check=False,
                        env=env,
                    )
                finally:
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                process.stdout.close()
                if result.returncode:
                    raise DemoError("Unmount failed; inspect mount point: " + str(root))
        finally:
            control.call(
                "DELETE", "/api/v1/mount-sessions/" + quote(ticket.id, safe="")
            )


def live(args):
    import httpx
    from mountable_sdk import Mountable

    if not os.environ.get("MOUNTABLE_API_KEY") or not args.org or not args.webhook_url:
        raise DemoError("--live needs MOUNTABLE_API_KEY, --org and --webhook-url")
    url = urlsplit(args.webhook_url)
    if (
        url.scheme != "https"
        or url.port not in (None, 443)
        or url.path != "/hooks/mountable"
        or url.query
        or url.fragment
        or url.username
        or url.password
    ):
        raise DemoError(
            "webhook URL must be public HTTPS on port 443 at /hooks/mountable"
        )
    with (
        httpx.Client(
            base_url="https://mountable.io",
            follow_redirects=False,
            timeout=30,
            headers={"Authorization": "Bearer " + os.environ["MOUNTABLE_API_KEY"]},
        ) as http,
        Mountable(base_url="https://mountable.io") as client,
    ):
        control = Control(http)
        # Limits are atomic with creation. A worker cannot race ahead uncapped.
        fs_id = control.call(
            "POST",
            filesystem_path(args.org),
            {"name": "harness-" + str(uuid.uuid4())[:8], **LIMITS},
        )["id"]
        print(
            "Created filesystem "
            + fs_id
            + "; delete it in the Mountable console after review.",
            flush=True,
        )
        path = filesystem_path(args.org, fs_id)
        webhook_id = None
        try:
            fs = client.filesystem(fs_id, mode="rw")
            # Start listening before registration. Early resyncs get retried until the secret is set.
            inbox = Inbox(fs_id, "")
            with receiver(inbox, args.port):
                webhook = control.call(
                    "POST",
                    path + "/webhooks",
                    {"url": args.webhook_url, "path_prefix": "/"},
                )
                webhook_id = webhook["id"]
                inbox.webhook_id, inbox.secret = webhook_id, webhook["secret"]
                planner(fs)
                print("Planner wrote plan and failing code via the SDK.", flush=True)
                coder(fs)
                print("Coder wrote the fix and READY marker via the SDK.", flush=True)
                candidate = await_candidate(inbox, fs)
                tmp = Path(tempfile.mkdtemp(prefix="mountable-harness-"))
                root = tmp / "review"
                root.mkdir()
                try:
                    with readonly_mount(client, control, fs_id, root):
                        check_candidate(root, candidate)
                        try:
                            (root / "verifier-must-not-write").write_text("forbidden")
                        except OSError as exc:
                            import errno

                            if exc.errno not in (
                                errno.EROFS,
                                errno.EACCES,
                                errno.EPERM,
                            ):
                                raise
                        else:
                            raise DemoError(
                                "verifier unexpectedly wrote through its read-only mount"
                            )
                finally:
                    # Never recursively clean a directory that might still be mounted.
                    try:
                        root.rmdir()
                    except OSError:
                        print(
                            "Inspect retained mount point: " + str(root),
                            file=sys.stderr,
                        )
                    else:
                        shutil.rmtree(tmp)
                print("Verifier passed tests; workspace write was denied.")
        finally:
            if webhook_id:
                control.call("DELETE", path + "/webhooks/" + quote(webhook_id, safe=""))


def dry_run():
    from types import SimpleNamespace

    class MemoryFiles:
        def __init__(self):
            self.files = {}

        def write(self, path, value):
            self.files[path] = value.encode()

        def read(self, path):
            return SimpleNamespace(data=self.files[path])

    fs = MemoryFiles()
    planner(fs)
    coder(fs)
    secret = "whsec_" + base64.b64encode(b"offline-demo-secret").decode()
    inbox = Inbox("offline-fs", secret)
    inbox.webhook_id = "offline-hook"
    body = json.dumps(
        {
            "type": "files.changed",
            "data": {
                "filesystem_id": "offline-fs",
                "webhook_id": "offline-hook",
                "cursor": "10",
                "events": [{"type": "created", "path": READY}],
            },
        }
    ).encode()
    stamp = str(int(time.time()))
    headers = {
        "webhook-id": "offline-message",
        "webhook-timestamp": stamp,
        "webhook-signature": signature(secret, "offline-message", stamp, body),
    }
    assert inbox.accept(headers, body) == 204
    candidate = await_candidate(inbox, fs, timeout=1)
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        for path, value in fs.files.items():
            (root / path.lstrip("/")).write_bytes(value)
        check_candidate(root, candidate)
    print("Dry run passed: planner → coder → signed webhook → verifier tests.")
    print("Files/API/cap and read-only mount are simulated; no network or billing.")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--live", action="store_true")
    mode.add_argument(
        "--dry-run", action="store_true", help="default; no network or billing"
    )
    parser.add_argument("--org", default=os.environ.get("MOUNTABLE_ORG_ID"))
    parser.add_argument(
        "--webhook-url", help="existing HTTPS proxy to 127.0.0.1:PORT/hooks/mountable"
    )
    parser.add_argument("--port", type=int, default=8787)
    args = parser.parse_args(argv)
    try:
        live(args) if args.live else dry_run()
    except DemoError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 -- sanitize SDK/transport/subprocess errors
        # SDK/transport/subprocess exceptions can contain secrets or echoed code.
        if getattr(exc, "code", None) in (
            "filesystem_spending_cap_reached",
            "spending_cap_reached",
            "rate_limited",
        ):
            print(f"{exc.status} {exc.code}; stopping this run", file=sys.stderr)
            return 1
        print(
            "Harness stopped. Check API limits, webhook routing and FUSE setup. No writes are retried.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
