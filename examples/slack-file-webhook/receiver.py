#!/usr/bin/env python3
"""Post a Slack line when a signed files.changed webhook arrives.

The process checks the signature on the raw body, then posts the changed
paths to a Slack incoming webhook. It remembers the last cursor in memory.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import hmac
import http.client
import json
import os
import sys
import threading
import time
from collections.abc import Callable, Mapping
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlsplit

MAX_BODY = 262144
MAX_LINES = 8
PATH_LIMIT = 180
HOOK_PATH = "/hooks/mountable"
Post = Callable[[str], None]


class SlackError(Exception):
    """The incoming webhook did not accept the message."""


def verify_webhook(
    secret: str,
    headers: Mapping[str, str],
    body: bytes,
    now: float | None = None,
) -> bool:
    """True when one v1 signature matches the raw body and the timestamp is recent."""
    normalized = {str(key).lower(): str(value) for key, value in headers.items()}
    msg_id = normalized.get("webhook-id", "")
    timestamp = normalized.get("webhook-timestamp", "")
    if not msg_id or not timestamp.isdigit():
        return False
    moment = time.time() if now is None else now
    if abs(moment - int(timestamp)) > 300:
        return False
    try:
        key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    except (ValueError, binascii.Error):
        return False
    if not key:
        return False
    signed = f"{msg_id}.{timestamp}.".encode() + body
    expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest())
    for signature in normalized.get("webhook-signature", "").split():
        if not signature.startswith("v1,"):
            continue
        given = signature[3:].encode()
        if len(given) == len(expected) and hmac.compare_digest(given, expected):
            return True
    return False


def check_secret(secret: str) -> str:
    cleaned = secret.strip()
    try:
        key = base64.b64decode(cleaned.removeprefix("whsec_"), validate=True)
    except (ValueError, binascii.Error):
        key = b""
    if not cleaned.startswith("whsec_") or not key:
        raise ValueError("MOUNTABLE_WEBHOOK_SECRET must be the whsec_ value from webhooks create")
    return cleaned


def check_slack_url(url: str) -> str:
    cleaned = url.strip()
    parts = urlsplit(cleaned)
    host = (parts.hostname or "").lower()
    if (
        parts.scheme != "https"
        or host != "hooks.slack.com"
        or parts.port not in (None, 443)
        or parts.username
        or parts.password
        or parts.query
        or parts.fragment
        or not parts.path.startswith("/services/")
        or parts.path.removeprefix("/services/") == ""
    ):
        raise ValueError("SLACK_WEBHOOK_URL must be an https://hooks.slack.com/services/ URL")
    return cleaned


def slack_text(events: list[tuple[str, str]]) -> str | None:
    lines: list[str] = []
    for kind, path in events:
        if kind not in ("created", "updated", "deleted"):
            continue
        if not path.startswith("/") or "\n" in path or "\r" in path:
            continue
        shown = path if len(path) <= PATH_LIMIT else path[:PATH_LIMIT] + "..."
        lines.append(f"{kind} {shown}")
    if not lines:
        return None
    extra = len(lines) - MAX_LINES
    if extra > 0:
        lines = lines[:MAX_LINES]
        lines.append(f"and {extra} more")
    return "\n".join(lines)


def message_action(body: bytes) -> tuple[int, str | None]:
    message = json.loads(body)
    if not isinstance(message, dict):
        raise ValueError("message")
    kind = message.get("type")
    data = message.get("data")
    if kind not in ("files.changed", "files.resync") or not isinstance(data, dict):
        raise ValueError("type")
    cursor = int(str(data["cursor"]))
    if cursor < 0:
        raise ValueError("cursor")
    if kind == "files.resync":
        return cursor, None
    raw_events = data.get("events")
    if not isinstance(raw_events, list):
        raise ValueError("events")
    pairs: list[tuple[str, str]] = []
    for event in raw_events:
        if not isinstance(event, dict):
            continue
        event_type = event.get("type")
        path = event.get("path")
        if isinstance(event_type, str) and isinstance(path, str):
            pairs.append((event_type, path))
    return cursor, slack_text(pairs)


def post_slack(url: str, text: str, timeout: float = 5) -> None:
    """POST {"text": ...} to a Slack incoming webhook. The URL is not logged."""
    checked = check_slack_url(url)
    parts = urlsplit(checked)
    payload = json.dumps({"text": text}).encode()
    connection = http.client.HTTPSConnection(parts.hostname, timeout=timeout)
    try:
        connection.request(
            "POST",
            parts.path,
            body=payload,
            headers={
                "Content-Type": "application/json",
                "Content-Length": str(len(payload)),
                "User-Agent": "mountable.io-slack-file-webhook",
            },
        )
        response = connection.getresponse()
        response.read()
        status = response.status
    except OSError:
        raise SlackError("Could not reach Slack") from None
    finally:
        connection.close()
    if status < 200 or status >= 300:
        raise SlackError(f"Slack returned HTTP {status}")


class Receiver:
    def __init__(self, secret: str, post: Post) -> None:
        self.secret = secret
        self.post = post
        self.cursor = -1
        self._lock = threading.Lock()

    def handle(self, headers: Mapping[str, str], body: bytes, now: float | None = None) -> int:
        if not verify_webhook(self.secret, headers, body, now):
            return 400
        try:
            cursor, text = message_action(body)
        except (KeyError, ValueError, TypeError, json.JSONDecodeError):
            return 400
        with self._lock:
            if cursor <= self.cursor:
                return 204
            if text is not None:
                try:
                    self.post(text)
                except SlackError:
                    return 500
            self.cursor = cursor
        return 204


def _handler(receiver: Receiver) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, format: str, *args: object) -> None:
            return

        def _reply(self, status: int) -> None:
            self.close_connection = True
            self.send_response(status)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def do_POST(self) -> None:  # noqa: N802
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                self._reply(400)
                return
            if self.path != HOOK_PATH or not 0 < length <= MAX_BODY:
                self._reply(400)
                return
            try:
                self.connection.settimeout(5)
                raw = self.rfile.read(length)
            except OSError:
                self._reply(400)
                return
            self._reply(receiver.handle(dict(self.headers.items()), raw))

    return Handler


def serve(receiver: Receiver, port: int) -> None:
    try:
        server = HTTPServer(("127.0.0.1", port), _handler(receiver))
    except OSError:
        print(f"could not listen on 127.0.0.1:{port}", file=sys.stderr)
        raise SystemExit(1) from None
    print(f"listening on 127.0.0.1:{server.server_address[1]}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


def _sample_body() -> bytes:
    return json.dumps(
        {
            "type": "files.changed",
            "data": {
                "webhook_id": "wh_example",
                "filesystem_id": "fs_example",
                "events": [{"type": "created", "path": "/notes/report.md", "file": None}],
                "cursor": "1",
            },
        },
        separators=(",", ":"),
    ).encode()


def run_check() -> None:
    """Sign one delivery locally and print the Slack line. No network calls."""
    secret = "whsec_" + base64.b64encode(b"local-check-secret").decode()
    body = _sample_body()
    timestamp = "1700000000"
    msg_id = "msg_local_check"
    key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    digest = hmac.new(key, f"{msg_id}.{timestamp}.".encode() + body, hashlib.sha256).digest()
    headers = {
        "webhook-id": msg_id,
        "webhook-timestamp": timestamp,
        "webhook-signature": "v1," + base64.b64encode(digest).decode(),
    }
    posted: list[str] = []
    receiver = Receiver(secret, posted.append)
    status = receiver.handle(headers, body, now=1700000000)
    if status != 204 or posted != ["created /notes/report.md"]:
        print("local check failed", file=sys.stderr)
        raise SystemExit(1)
    print(posted[0])


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="sign one delivery locally and print the Slack line")
    parser.add_argument("--port", type=int, default=8080)
    args = parser.parse_args()
    if args.check:
        run_check()
        return
    if not 1 <= args.port <= 65535:
        print("--port must be from 1 to 65535", file=sys.stderr)
        raise SystemExit(2)
    try:
        secret = check_secret(os.environ.get("MOUNTABLE_WEBHOOK_SECRET", ""))
        slack_url = check_slack_url(os.environ.get("SLACK_WEBHOOK_URL", ""))
    except ValueError as exc:
        print(exc, file=sys.stderr)
        raise SystemExit(2) from None
    serve(Receiver(secret, lambda text: post_slack(slack_url, text)), args.port)


if __name__ == "__main__":
    main()
