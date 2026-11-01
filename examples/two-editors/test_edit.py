"""Released SDK over a local fake API. No cloud calls."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from edit import (
    VERSION_A,
    VERSION_B,
    VERSION_C,
    _recover,
    apply_owned,
    main,
    parse,
    proposal_from_a,
    render,
    version_name,
)


def test_second_editor_keeps_the_field_it_read():
    remembered = parse(proposal_from_a("two"))
    assert remembered["status"] == "open"
    current = apply_owned(parse(VERSION_B), "two")
    assert current["status"] == "review"
    assert current["editor"] == "two"
    assert render(current) == VERSION_C
    assert version_name(current) == "C"
    assert current != remembered


class Api:
    def __init__(self) -> None:
        self.files: dict[str, tuple[bytes, str]] = {}
        self.sessions: dict[str, dict[str, str]] = {}
        self.serial = 0
        self.reads = 0
        self.writes = 0
        self.lock = threading.Lock()

    def etag_for(self, data: bytes) -> str:
        self.serial += 1
        return f'"{self.serial:x}-{len(data)}"'


def serve(api: Api) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt: str, *args: object) -> None:
            return

        def _json(self, status: int, payload: object) -> None:
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def _raw(self, status: int, body: bytes, headers: dict[str, str]) -> None:
            self.send_response(status)
            for key, value in headers.items():
                self.send_header(key, value)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)

        def _read(self) -> bytes:
            length = int(self.headers.get("Content-Length", "0"))
            return self.rfile.read(length)

        def _file_path(self) -> str | None:
            marker = "/files/"
            if marker not in self.path:
                return None
            return self.path.split(marker, 1)[1]

        def do_POST(self) -> None:  # noqa: N802
            self._read()
            if self.path != "/api/v1/mount-sessions":
                self._json(404, {"detail": {"code": "not_found"}})
                return
            with api.lock:
                api.serial += 1
                session_id = f"sess-{api.serial}"
                token = f"mtblst_{api.serial}"
                record = {
                    "id": session_id,
                    "filesystem_id": "fs1",
                    "mode": "rw",
                    "credential": "token",
                    "state": "active",
                    "created_by": "example",
                    "expires_at": "2099-01-01T00:00:00Z",
                    "created_at": "2026-10-27T00:00:00Z",
                    "token": token,
                }
                api.sessions[session_id] = record
            self._json(201, record)

        def do_DELETE(self) -> None:  # noqa: N802
            if self.path.startswith("/api/v1/mount-sessions/"):
                session_id = self.path.rsplit("/", 1)[-1]
                with api.lock:
                    api.sessions.pop(session_id, None)
                self._raw(204, b"", {})
                return
            path = self._file_path()
            if path is None:
                self._json(404, {"detail": {"code": "not_found"}})
                return
            with api.lock:
                current = api.files.get(path)
                if current is None:
                    self._json(404, {"detail": {"code": "not_found"}})
                    return
                _data, etag = current
                if self.headers.get("If-Match") != etag:
                    self._json(
                        412,
                        {
                            "detail": {
                                "code": "precondition_failed",
                                "current_etag": etag,
                            }
                        },
                    )
                    return
                del api.files[path]
            self._raw(204, b"", {})

        def do_PUT(self) -> None:  # noqa: N802
            body = self._read()
            path = self._file_path()
            if path is None:
                self._json(404, {"detail": {"code": "not_found"}})
                return
            with api.lock:
                current = api.files.get(path)
                if self.headers.get("If-None-Match") == "*" and current is not None:
                    self._json(
                        412,
                        {
                            "detail": {
                                "code": "precondition_failed",
                                "current_etag": current[1],
                            }
                        },
                    )
                    return
                if "If-Match" in self.headers:
                    if current is None or self.headers["If-Match"] != current[1]:
                        self._json(
                            412,
                            {
                                "detail": {
                                    "code": "precondition_failed",
                                    "current_etag": None
                                    if current is None
                                    else current[1],
                                }
                            },
                        )
                        return
                etag = api.etag_for(body)
                api.files[path] = (body, etag)
                api.writes += len(body)
            self._json(
                201,
                {
                    "path": "/" + path,
                    "type": "file",
                    "size": len(body),
                    "etag": etag,
                    "modified_at": "2026-10-27T00:00:00Z",
                },
            )

        def do_GET(self) -> None:  # noqa: N802
            if self.path.endswith("/usage"):
                self._json(
                    200,
                    {
                        "retained_bytes": sum(
                            len(item[0]) for item in api.files.values()
                        ),
                        "byte_quota": 1073741824,
                        "entries": len(api.files),
                        "entry_quota": 10000,
                        "bytes_read_30d": api.reads,
                        "bytes_written_30d": api.writes,
                        "reserved_cents": 3,
                        "currency": "eur",
                    },
                )
                return
            if self.path.endswith("/mount-sessions"):
                with api.lock:
                    listed = [
                        {key: value for key, value in record.items() if key != "token"}
                        for record in api.sessions.values()
                    ]
                self._json(200, listed)
                return
            if "/filesystems/fs1" in self.path and "/files/" not in self.path:
                self._json(
                    200,
                    {
                        "id": "fs1",
                        "name": "example",
                        "byte_quota": 1073741824,
                        "entry_quota": 10000,
                        "spending_cap_cents": 100,
                        "mutations_per_minute": 100,
                        "created_at": "2026-10-27T00:00:00Z",
                    },
                )
                return
            path = self._file_path()
            if path is None:
                self._json(404, {"detail": {"code": "not_found"}})
                return
            with api.lock:
                current = api.files.get(path)
                if current is None:
                    self._json(404, {"detail": {"code": "not_found"}})
                    return
                data, etag = current
                api.reads += len(data)
            self._raw(
                200,
                data,
                {
                    "ETag": etag,
                    "Last-Modified": "Tue, 27 Oct 2026 00:00:00 GMT",
                    "Content-Type": "application/octet-stream",
                },
            )

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


@pytest.fixture
def api_env(monkeypatch: pytest.MonkeyPatch):
    api = Api()
    server = serve(api)
    host, port = server.server_address
    monkeypatch.setenv("MOUNTABLE_API_URL", f"http://{host}:{port}")
    monkeypatch.setenv("MOUNTABLE_API_KEY", "mtbl_fixture")
    monkeypatch.setenv("MOUNTABLE_ORG", "org1")
    yield api
    server.shutdown()


def test_live_two_processes_refuse_stale_write_then_reconcile(
    api_env: Api, capsys: pytest.CaptureFixture[str]
):
    assert main(["live", "--filesystem", "fs1", "--run", "demo"]) == 0
    captured = capsys.readouterr()
    output = captured.out
    assert "version=A" in output
    assert output.count("$ python edit.py read ") >= 2
    assert "412 precondition_failed" in captured.err
    assert "stale: 412" in output
    assert "preserved: B" in output
    assert "reconciled: C" in output
    assert "collision: 412" in output
    assert "absent: 404" in output
    assert "revoked_sessions=" in captured.err
    assert api_env.files == {}
    assert api_env.sessions == {}


def test_stale_body_is_not_stored(api_env: Api):
    assert main(["seed", "--filesystem", "fs1", "--run", "onefile"]) == 0
    # The next assertions use the API directly through the CLI's exit codes.
    assert (
        main(
            [
                "commit",
                "--filesystem",
                "fs1",
                "--run",
                "onefile",
                "--if-match",
                "missing",
                "--editor",
                "one",
            ]
        )
        == 2
    )
    stored = api_env.files["etag-edit-onefile.txt"][0]
    assert stored == VERSION_A


def test_cleanup_refuses_a_different_etag(api_env: Api):
    assert main(["seed", "--filesystem", "fs1", "--run", "keep"]) == 0
    etag = api_env.files["etag-edit-keep.txt"][1]
    assert (
        main(
            [
                "cleanup",
                "--filesystem",
                "fs1",
                "--run",
                "keep",
                "--if-match",
                etag + "x",
            ]
        )
        == 2
    )
    assert api_env.files["etag-edit-keep.txt"][0] == VERSION_A
    assert (
        main(["cleanup", "--filesystem", "fs1", "--run", "keep", "--if-match", etag])
        == 0
    )
    assert "etag-edit-keep.txt" not in api_env.files


def test_recover_deletes_a_known_version_and_leaves_another(
    api_env: Api, capsys: pytest.CaptureFixture[str]
):
    assert main(["seed", "--filesystem", "fs1", "--run", "left"]) == 0
    api_env.files["etag-edit-odd.txt"] = (b"nope\n", '"zzz"')
    _recover("fs1", "left")
    _recover("fs1", "odd")
    captured = capsys.readouterr().err
    assert "etag-edit-left.txt" not in api_env.files
    assert api_env.files["etag-edit-odd.txt"][0] == b"nope\n"
    assert "recovered etag-edit-left.txt" in captured
    assert "recover_left_unfamiliar_file" in captured
