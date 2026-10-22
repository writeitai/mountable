#!/usr/bin/env python3
"""An agent creates, updates, and renames a file. The backend gets a signed webhook.

`--dry-run` prints an illustrated sequence and checks a locally signed
delivery. It does not call Mountable. A live run needs Linux, FUSE, the
`mountable` CLI, and a public HTTPS URL on port 443 that forwards to this
machine. Delete the filesystem in the console when you are done.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import hmac
import json
import os
import sys
import threading
import time
import uuid
from collections.abc import Mapping
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Iterator
from urllib.parse import quote, urlsplit

import showcase
from rest import ApiError, Rest

TITLE = "Signed webhooks when files change."
EXPECTED = (
    ("created", "/notes/draft.txt"),
    ("updated", "/notes/draft.txt"),
    ("deleted", "/notes/draft.txt"),
    ("created", "/notes/report.txt"),
)
WEBHOOK_CONTRACT = (
    "Dry-run visualization. Playing it does not create a filesystem or call "
    "the Mountable API. The agent creates, updates, and renames a file. "
    "The backend receives signed type files.changed deliveries. "
    "data.events[] uses created, updated, and deleted. A rename is deleted "
    "for the old path and created for the new path. Apply a message only "
    "when its data.cursor, compared as an integer, is greater than the last "
    "cursor applied."
)
WEBHOOK_RECORDED = (
    "Recorded by examples/showcase/webhooks.py against Mountable. "
    "The receiver checked the signature on the raw body and applied a "
    "files.changed message only when its cursor, compared as an integer, "
    "was greater than the last one applied. A rename arrived as deleted "
    "for the old path and created for the new path."
)


def _line(t_ms: int, text: str) -> dict[str, object]:
    return {"t_ms": t_ms, "text": text}


def _pane(title: str, subtitle: str, lines: list[dict[str, object]]) -> dict[str, object]:
    return {"title": title, "subtitle": subtitle, "lines": lines}


def _changed(cursor: str, events: list[dict[str, str]]) -> str:
    payload = {
        "type": "files.changed",
        "data": {
            "webhook_id": "wh_example",
            "filesystem_id": "fs_example",
            "events": events,
            "cursor": cursor,
        },
    }
    return json.dumps(payload, separators=(",", ":"))


def illustrative_webhooks() -> dict[str, object]:
    mounted = json.dumps(
        {"event": "mounted", "path": "/mnt/work", "filesystem_id": "fs_example", "mode": "rw"},
        separators=(",", ":"),
    )
    return {
        "id": "webhooks",
        "title": TITLE,
        "source": "illustrative",
        "measured_abort_ms": None,
        "contract": WEBHOOK_CONTRACT,
        "panes": [
            _pane(
                "Agent",
                "One shared folder. Create, update, then rename.",
                [
                    _line(0, "$ mkdir -p /mnt/work"),
                    _line(
                        500,
                        '$ printf \'%s\\n\' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work',
                    ),
                    _line(1100, mounted),
                    _line(1700, "$ mkdir -p /mnt/work/notes"),
                    _line(2300, "$ printf draft > /mnt/work/notes/draft.txt"),
                    _line(3200, "$ printf final > /mnt/work/notes/draft.txt"),
                    _line(4100, "$ mv /mnt/work/notes/draft.txt /mnt/work/notes/report.txt"),
                ],
            ),
            _pane(
                "App backend",
                "Signed files.changed deliveries. The signing secret is stored once.",
                [
                    _line(400, '$ # POST .../webhooks {"url":"https://example.com/hooks/mountable","path_prefix":"/"}'),
                    _line(900, "# secret stored once; it is not printed again"),
                    _line(
                        2600,
                        _changed("1", [{"type": "created", "path": "/notes/draft.txt"}]),
                    ),
                    _line(2800, "HTTP 204"),
                    _line(
                        3600,
                        _changed("2", [{"type": "updated", "path": "/notes/draft.txt"}]),
                    ),
                    _line(3800, "HTTP 204"),
                    _line(
                        4600,
                        _changed(
                            "3",
                            [
                                {"type": "deleted", "path": "/notes/draft.txt"},
                                {"type": "created", "path": "/notes/report.txt"},
                            ],
                        ),
                    ),
                    _line(4800, "HTTP 204"),
                ],
            ),
        ],
    }


def verify_webhook(secret: str, headers: Mapping[str, str], body: bytes) -> bool:
    """True when a signature matches the raw body and the timestamp is recent.

    ``body`` is the exact request bytes, including a trailing newline when
    the sender added one. Do not re-encode the JSON before checking.
    """
    msg_id = headers.get("webhook-id", "")
    timestamp = headers.get("webhook-timestamp", "")
    if not msg_id or not timestamp.isdigit() or abs(time.time() - int(timestamp)) > 300:
        return False
    try:
        key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    except (ValueError, binascii.Error):
        return False
    signed = f"{msg_id}.{timestamp}.".encode() + body
    expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest())
    for signature in headers.get("webhook-signature", "").split():
        if not signature.startswith("v1,"):
            continue
        given = signature[3:].encode()
        if len(given) == len(expected) and hmac.compare_digest(given, expected):
            return True
    return False


def sign(secret: str, msg_id: str, timestamp: str, body: bytes) -> dict[str, str]:
    key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    digest = hmac.new(key, f"{msg_id}.{timestamp}.".encode() + body, hashlib.sha256).digest()
    return {
        "webhook-id": msg_id,
        "webhook-timestamp": timestamp,
        "webhook-signature": "v1," + base64.b64encode(digest).decode(),
    }


def files_changed(filesystem_id: str, webhook_id: str, cursor: str, events: list[tuple[str, str]]) -> bytes:
    return json.dumps(
        {
            "type": "files.changed",
            "timestamp": "2026-10-22T10:00:00Z",
            "data": {
                "webhook_id": webhook_id,
                "filesystem_id": filesystem_id,
                "events": [{"type": kind, "path": path, "file": None} for kind, path in events],
                "cursor": cursor,
            },
        },
        separators=(",", ":"),
    ).encode()


class State:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.secret = ""
        self.webhook_id = ""
        self.filesystem_id = ""
        self.cursor = -1
        self.events: list[dict[str, str]] = []

    def snapshot(self) -> list[dict[str, str]]:
        with self._lock:
            return [dict(event) for event in self.events]


def apply_message(state: State, message: Mapping[str, object]) -> bool:
    """Apply one verified message. Cursor comparison is numeric, not lexical."""
    data = message["data"]
    if not isinstance(data, dict):
        raise ValueError("data")
    cursor = int(str(data["cursor"]))
    if cursor < 0:
        raise ValueError("cursor")
    if cursor <= state.cursor:
        return False
    kind = message["type"]
    if kind == "files.changed":
        raw_events = data["events"]
        if not isinstance(raw_events, list):
            raise ValueError("events")
        parsed: list[dict[str, str]] = []
        for event in raw_events:
            if not isinstance(event, dict):
                raise ValueError("event")
            event_type = event.get("type")
            path = event.get("path")
            if event_type not in ("created", "updated", "deleted"):
                raise ValueError("event type")
            if not isinstance(path, str) or not path.startswith("/"):
                raise ValueError("path")
            parsed.append({"type": str(event_type), "path": path})
        state.events.extend(parsed)
    elif kind != "files.resync":
        raise ValueError("type")
    state.cursor = cursor
    return True


def accept(state: State, headers: Mapping[str, str], body: bytes) -> int:
    normalized = {str(key).lower(): str(value) for key, value in headers.items()}
    with state._lock:
        if not state.secret or not state.webhook_id:
            return 503
        if not verify_webhook(state.secret, normalized, body):
            return 401
        try:
            message = json.loads(body)
            if not isinstance(message, dict):
                return 400
            data = message.get("data")
            if not isinstance(data, dict):
                return 400
            if data.get("filesystem_id") != state.filesystem_id or data.get("webhook_id") != state.webhook_id:
                return 400
            apply_message(state, message)
        except (KeyError, ValueError, TypeError, json.JSONDecodeError):
            return 400
        return 204


def contains_in_order(events: list[dict[str, str]], expected: tuple[tuple[str, str], ...]) -> bool:
    index = 0
    for event in events:
        if index < len(expected) and (event["type"], event["path"]) == expected[index]:
            index += 1
    return index == len(expected)


def wait_for_resync(state: State, timeout: float) -> int:
    """Block until the opening files.resync advances the cursor.

    A new webhook's first message is a files.resync whose cursor is the
    change-log head at send time. File writes made before that message is
    applied can sit at or below that cursor and never arrive as events.
    """
    deadline = time.monotonic() + timeout
    while True:
        with state._lock:
            if state.cursor >= 0:
                return state.cursor
        if time.monotonic() >= deadline:
            raise showcase.ShowcaseError("timed out waiting for the opening files.resync")
        time.sleep(0.05)


def wait_for(state: State, expected: tuple[tuple[str, str], ...], timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while True:
        if contains_in_order(state.snapshot(), expected):
            return
        if time.monotonic() >= deadline:
            raise showcase.ShowcaseError(
                f"timed out waiting for file-change events; saw {len(state.snapshot())}"
            )
        time.sleep(0.05)


def agent_script(directory: Path) -> None:
    notes = directory / "notes"
    notes.mkdir(parents=True, exist_ok=True)
    draft = notes / "draft.txt"
    draft.write_text("draft\n")
    draft.write_text("final\n")
    draft.rename(notes / "report.txt")


def check_webhook_url(url: str) -> None:
    parts = urlsplit(url)
    host = (parts.hostname or "").lower()
    if (
        parts.scheme != "https"
        or parts.port not in (None, 443)
        or parts.path != "/hooks/mountable"
        or parts.query
        or parts.fragment
        or parts.username
        or parts.password
        or host in {"", "localhost", "127.0.0.1", "::1"}
    ):
        raise showcase.UsageError(
            "webhook URL must be public HTTPS on port 443 at /hooks/mountable"
        )


def _handler(state: State) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, format: str, *args: object) -> None:
            return

        def _reply(self, status: int) -> None:
            self.send_response(status)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def do_POST(self) -> None:  # noqa: N802
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                self._reply(400)
                return
            if self.path != "/hooks/mountable" or not 0 < length <= 262144:
                self._reply(400)
                return
            try:
                self.connection.settimeout(5)
                raw = self.rfile.read(length)
            except OSError:
                self._reply(400)
                return
            self._reply(accept(state, dict(self.headers.items()), raw))

    return Handler


@contextmanager
def receiver(state: State, port: int) -> Iterator[int]:
    try:
        server = HTTPServer(("127.0.0.1", port), _handler(state))
    except OSError:
        raise showcase.ShowcaseError(f"could not listen on 127.0.0.1:{port}") from None
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield int(server.server_address[1])
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def _org_path(org_id: str, fs_id: str | None = None) -> str:
    path = "/api/v1/orgs/" + quote(org_id, safe="") + "/filesystems"
    if fs_id is None:
        return path
    return path + "/" + quote(fs_id, safe="")


def _mount_and_edit(cli: str, parent: Mapping[str, str], fs_id: str, directory: Path) -> None:
    backend = showcase.LiveBackend(parent, cli)
    session, ticket = backend.ticket(
        fs_id, read_only=False, idempotency_key=f"showcase-webhooks-{uuid.uuid4().hex}"
    )
    mount = None
    try:
        mount = backend.mount_ticket(ticket, directory)
        mount.wait_mounted(60)
        agent_script(directory)
    finally:
        if mount is not None:
            try:
                backend.unmount(directory)
            except showcase.ShowcaseError as exc:
                print(f"showcase: unmount failed: {exc}", file=sys.stderr)
            try:
                mount.wait_done(15)
            except showcase.ShowcaseError:
                pass
        try:
            backend.revoke(session)
        except showcase.ShowcaseError as exc:
            print(f"showcase: could not revoke mount session: {exc}", file=sys.stderr)


def run_webhooks_live(
    *,
    rest: Rest,
    org_id: str,
    webhook_url: str,
    port: int,
    cli: str,
    root: Path,
    parent: Mapping[str, str],
) -> dict[str, object]:
    check_webhook_url(webhook_url)
    state = State()
    trace = showcase.Trace("webhooks", TITLE, WEBHOOK_RECORDED)
    api_key = parent.get(showcase.API_KEY_ENV, "")
    if api_key:
        trace.secrets.append(api_key)
    trace.pane("agent", "Agent", "One shared folder. Create, update, then rename.")
    trace.pane("backend", "App backend", "Signed files.changed deliveries.")
    webhook_id = ""
    fs_id = ""
    try:
        with receiver(state, port):
            try:
                created = rest.json(
                    "POST",
                    _org_path(org_id),
                    {
                        "name": "showcase-webhooks-" + uuid.uuid4().hex[:8],
                        "spending_cap_cents": 100,
                    },
                )
            except ApiError as exc:
                if exc.code == "payment_required":
                    raise showcase.ShowcaseError(
                        "a saved payment method is required before a filesystem can have a spend cap"
                    ) from None
                raise showcase.ShowcaseError(f"HTTP {exc.status} {exc.code}") from None
            found = created.get("id")
            if not isinstance(found, str) or not found:
                raise showcase.ShowcaseError("filesystem create returned no id")
            fs_id = found
            state.filesystem_id = fs_id
            print(
                f"showcase: created {fs_id}; delete it in the console when you are done.",
                file=sys.stderr,
            )
            trace.say("backend", f"$ # filesystem {fs_id}")
            try:
                hook = rest.json(
                    "POST",
                    _org_path(org_id, fs_id) + "/webhooks",
                    {"url": webhook_url, "path_prefix": "/"},
                )
            except ApiError as exc:
                raise showcase.ShowcaseError(f"HTTP {exc.status} {exc.code}") from None
            secret = hook.get("secret")
            hook_id = hook.get("id")
            if not isinstance(secret, str) or not secret.startswith("whsec_") or not isinstance(hook_id, str):
                raise showcase.ShowcaseError("webhook create did not return a one-time secret")
            webhook_id = hook_id
            with state._lock:
                state.webhook_id = hook_id
                state.secret = secret
            trace.secrets.append(secret)
            trace.say("backend", "# secret stored once; it is not printed again")
            cursor = wait_for_resync(state, 40)
            trace.say("backend", f"files.resync cursor {cursor}")
            directory = root / uuid.uuid4().hex
            directory.mkdir(parents=True, exist_ok=True)
            trace.say("agent", f"$ mkdir -p {directory}/notes")
            trace.say("agent", f"$ printf draft > {directory}/notes/draft.txt")
            trace.say("agent", f"$ printf final > {directory}/notes/draft.txt")
            trace.say("agent", f"$ mv {directory}/notes/draft.txt {directory}/notes/report.txt")
            _mount_and_edit(cli, parent, fs_id, directory)
            wait_for(state, EXPECTED, 45)
            for event in state.snapshot():
                trace.say("backend", f"{event['type']} {event['path']}")
    finally:
        if webhook_id and fs_id:
            try:
                rest.request(
                    "DELETE",
                    _org_path(org_id, fs_id) + "/webhooks/" + quote(webhook_id, safe=""),
                )
            except ApiError as exc:
                print(f"showcase: could not delete webhook: HTTP {exc.status} {exc.code}", file=sys.stderr)
    if not contains_in_order(state.snapshot(), EXPECTED):
        raise showcase.ShowcaseError("the signed deliveries did not include the rename")
    return trace.body


def _deliver(
    state: State,
    secret: str,
    stamp: str,
    cursor: str,
    events: list[tuple[str, str]],
    msg_id: str,
) -> int:
    body = files_changed(state.filesystem_id, state.webhook_id, cursor, events)
    return accept(state, sign(secret, msg_id, stamp, body), body)


def _ready(secret: str) -> State:
    state = State()
    state.filesystem_id = "fs_offline"
    state.webhook_id = "wh_offline"
    state.secret = secret
    return state


def cursors_compare_as_integers(secret: str, stamp: str) -> None:
    """``"10"`` is after ``"2"``. ``"9"`` is before ``"10"`` and is ignored."""
    state = _ready(secret)
    if _deliver(state, secret, stamp, "2", [("created", "/notes/a.txt")], "n2") != 204:
        raise showcase.ShowcaseError("cursor 2 was refused")
    if _deliver(state, secret, stamp, "10", [("updated", "/notes/a.txt")], "n10") != 204:
        raise showcase.ShowcaseError("cursor 10 was refused")
    if _deliver(state, secret, stamp, "9", [("deleted", "/notes/a.txt")], "n9") != 204:
        raise showcase.ShowcaseError("cursor 9 was refused")
    if state.cursor != 10:
        raise showcase.ShowcaseError("cursor 10 was not applied after cursor 2")
    if any(event["type"] == "deleted" for event in state.snapshot()):
        raise showcase.ShowcaseError("an older cursor was applied")


def simulate() -> list[dict[str, str]]:
    """Offline signed deliveries, including a duplicate and a bad signature."""
    secret = "whsec_" + base64.b64encode(b"offline-demo-secret").decode()
    stamp = str(int(time.time()))
    cursors_compare_as_integers(secret, stamp)
    state = _ready(secret)
    rename = [
        ("deleted", "/notes/draft.txt"),
        ("created", "/notes/report.txt"),
    ]
    sequence = [
        ("1", [("created", "/notes/draft.txt")], "created"),
        ("2", [("updated", "/notes/draft.txt")], "updated"),
        ("3", rename, "rename"),
    ]
    for cursor, events, msg_id in sequence:
        if _deliver(state, secret, stamp, cursor, events, msg_id) != 204:
            raise showcase.ShowcaseError(f"{msg_id} delivery was refused")
    if _deliver(state, secret, stamp, "3", rename, "rename-duplicate") != 204:
        raise showcase.ShowcaseError("duplicate delivery was refused")
    if state.cursor != 3 or not contains_in_order(state.snapshot(), EXPECTED):
        raise showcase.ShowcaseError("applied events did not match the file changes")
    if len(state.snapshot()) != len(EXPECTED):
        raise showcase.ShowcaseError("a duplicate cursor was applied")
    stale = files_changed("fs_offline", "wh_offline", "4", [("created", "/notes/extra.txt")])
    bad = sign(secret, "bad", stamp, stale)
    bad["webhook-signature"] = "v1,aaaa"
    if accept(state, bad, stale) != 401 or state.cursor != 3:
        raise showcase.ShowcaseError("a bad signature was accepted")
    return state.snapshot()


def _emit(trace: dict[str, object], trace_path: Path | None) -> None:
    sys.stdout.write(showcase.format_script(trace))
    if trace_path is not None:
        trace_path.parent.mkdir(parents=True, exist_ok=True)
        trace_path.write_text(json.dumps(trace, indent=2) + "\n")


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Signed file-change webhook demo")
    parser.add_argument("--dry-run", action="store_true", help="print the illustrated sequence and call nothing")
    parser.add_argument(
        "--org",
        default=os.environ.get("MOUNTABLE_ORG_ID") or os.environ.get("MOUNTABLE_ORG"),
        help="organisation id (or set MOUNTABLE_ORG_ID)",
    )
    parser.add_argument("--webhook-url", help="public HTTPS URL forwarding to 127.0.0.1:PORT/hooks/mountable")
    parser.add_argument("--port", type=int, default=8787)
    parser.add_argument("--root", type=Path, help="local directory for the mount point")
    parser.add_argument("--trace", type=Path, help="write the trace JSON here")
    parser.add_argument("--cli", default=os.environ.get("MOUNTABLE_BIN", "mountable"), help="mountable binary")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.dry_run:
            events = simulate()
            if not contains_in_order(events, EXPECTED):
                raise showcase.ShowcaseError("dry-run signature check did not see the rename")
            _emit(illustrative_webhooks(), args.trace)
            print(
                "showcase: dry run passed; signed files.changed covered created, updated, and a rename",
                file=sys.stderr,
            )
            return 0
        api_key = os.environ.get(showcase.API_KEY_ENV, "")
        if not api_key:
            raise showcase.UsageError("MOUNTABLE_API_KEY is not set. Use --dry-run to preview without a filesystem.")
        if not args.org:
            raise showcase.UsageError("Pass --org ORG_ID or set MOUNTABLE_ORG_ID.")
        if not args.webhook_url:
            raise showcase.UsageError("Pass --webhook-url https://YOUR-HOST/hooks/mountable.")
        root = args.root or Path("/tmp/mountable-showcase-webhooks")
        trace = run_webhooks_live(
            rest=Rest(api_key),
            org_id=args.org,
            webhook_url=args.webhook_url,
            port=args.port,
            cli=args.cli,
            root=root,
            parent=os.environ,
        )
    except showcase.ShowcaseError as exc:
        print(f"showcase: {exc}", file=sys.stderr)
        return 2 if isinstance(exc, showcase.UsageError) else 1
    _emit(trace, args.trace)
    return 0


if __name__ == "__main__":
    sys.exit(main())
