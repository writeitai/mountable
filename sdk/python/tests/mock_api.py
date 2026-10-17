"""A local HTTP server that plays the Mountable API for the contract tests."""

from __future__ import annotations

import json
import socket
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs

API_KEY = "mtbl_testkeysecret"
FS = "fs_one"
FILES = f"/api/v1/filesystems/{FS}/files"
LIST = f"/api/v1/filesystems/{FS}/list"
CHANGES = f"/api/v1/filesystems/{FS}/changes"
SESSIONS = "/api/v1/mount-sessions"


@dataclass
class Request:
    method: str
    path: str  # as sent, still percent-encoded, without the query
    query: dict[str, str]
    headers: dict[str, str]  # lower-case names
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body)


@dataclass
class Reply:
    status: int
    body: bytes | str | dict[str, Any] | list[Any] = b""
    headers: dict[str, str] = field(default_factory=dict[str, str])
    # "drop": close the connection without an answer; "short": send the
    # headers and only part of the body.
    fault: str | None = None


Handler = Callable[[Request], Reply]


def error(status: int, code: str, **fields: Any) -> Reply:
    return Reply(status, {"detail": {"code": code, **fields}})


def file_info(path: str, etag: str | None = '"e1"', **overrides: Any) -> dict[str, Any]:
    info: dict[str, Any] = {
        "path": path,
        "type": "file",
        "size": 3,
        "etag": etag,
        "modified_at": "2026-10-17T12:00:00.123456789Z",
    }
    info.update(overrides)
    return info


def iso(moment: datetime) -> str:
    return moment.isoformat().replace("+00:00", "Z")


class MockAPI:
    """Answers "METHOD /path" routes and records every request.

    Token sessions are issued by default: mtblst_token1secret, then
    mtblst_token2secret, …, each expiring after session_lifetime.
    """

    def __init__(self) -> None:
        self.routes: dict[str, Handler] = {}
        self.requests: list[Request] = []
        self.session_lifetime = timedelta(hours=1)
        self.tokens = 0
        self._lock = threading.Lock()
        api = self

        class Handle(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args: Any) -> None:
                pass

            def _serve(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else b""
                path, _, query = self.path.partition("?")
                request = Request(
                    method=self.command,
                    path=path,
                    query={k: v[0] for k, v in parse_qs(query).items()},
                    headers={k.lower(): v for k, v in self.headers.items()},
                    body=body,
                )
                with api._lock:
                    api.requests.append(request)
                reply = api.answer(request)
                if reply.fault == "drop":
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    return
                data = reply.body
                if isinstance(data, (dict, list)):
                    data = json.dumps(data).encode()
                    reply.headers.setdefault("Content-Type", "application/json")
                elif isinstance(data, str):
                    data = data.encode()
                self.send_response(reply.status)
                for name, value in reply.headers.items():
                    self.send_header(name, value)
                if "Content-Length" not in reply.headers:
                    self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if reply.fault == "short":
                    self.wfile.write(data[: len(data) // 2])
                    self.wfile.flush()
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    return
                if self.command != "HEAD":
                    self.wfile.write(data)

            do_GET = do_PUT = do_DELETE = do_POST = _serve

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handle)
        self._server.daemon_threads = True
        self.url = f"http://127.0.0.1:{self._server.server_address[1]}"
        threading.Thread(
            target=self._server.serve_forever, args=(0.01,), daemon=True
        ).start()

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def route(self, method: str, path: str, handler: Handler | Reply) -> None:
        if isinstance(handler, Reply):
            reply = handler
            self.routes[f"{method} {path}"] = lambda _: reply
        else:
            self.routes[f"{method} {path}"] = handler

    def answer(self, request: Request) -> Reply:
        handler = self.routes.get(f"{request.method} {request.path}")
        if handler is not None:
            return handler(request)
        if request.method == "POST" and request.path == SESSIONS:
            return self.issue_session(request)
        return error(404, "not_found")

    def issue_session(self, request: Request) -> Reply:
        body = request.json()
        with self._lock:
            self.tokens += 1
            number = self.tokens
        session: dict[str, Any] = {
            "id": f"s{number}",
            "filesystem_id": body["filesystem_id"],
            "mode": body["mode"],
            "state": "active",
            "created_by": "api key test",
            "expires_at": iso(datetime.now(timezone.utc) + self.session_lifetime),
            "created_at": iso(datetime.now(timezone.utc)),
            "ticket": None,
            "ticket_expires_at": None,
        }
        if body.get("credential") == "token":
            session["token"] = f"mtblst_token{number}secret"
            session["credential"] = "token"
        else:
            session.update(
                state="requested",
                ticket=f"mtbltk_ticket{number}secret",
                ticket_expires_at=iso(datetime.now(timezone.utc)),
            )
        return Reply(201, session)

    def calls(self) -> list[str]:
        with self._lock:
            return [f"{r.method} {r.path}" for r in self.requests]

    def operations(self) -> list[Request]:
        """The requests other than session creation."""
        with self._lock:
            return [r for r in self.requests if r.path != SESSIONS]

    def sessions(self) -> list[Request]:
        with self._lock:
            return [r for r in self.requests if r.path == SESSIONS]
