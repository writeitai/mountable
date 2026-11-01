"""Released SDK over a local fake API. No cloud calls."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

import pytest
from mountable_sdk import Mountable

from cleanup import (
    KEEP_BYTES,
    NOTES_BYTES,
    REPORT_BYTES,
    cleanup_job_a,
    cleanup_job_b,
    file_a,
    file_b,
    main,
    manifest_path,
    prefix_a,
)

RUN = "demo"


class Api:
    def __init__(self) -> None:
        self.files: dict[str, tuple[bytes, str]] = {}
        self.dirs: set[str] = set()
        self.sessions: dict[str, dict[str, str]] = {}
        self.deleted: list[str] = []
        self.fail_once: set[str] = set()
        self.serial = 0
        self.reads = 0
        self.writes = 0
        self.lock = threading.Lock()

    def etag_for(self, data: bytes) -> str:
        self.serial += 1
        return f'"{self.serial:x}-{len(data)}"'

    def add_dirs(self, path: str) -> None:
        parts = path.split("/")
        for index in range(1, len(parts)):
            self.dirs.add("/".join(parts[:index]))

    def children(self, directory: str) -> list[dict[str, object]]:
        names: dict[str, dict[str, object]] = {}
        prefix = f"{directory}/" if directory else ""
        for path in self.dirs:
            if path.startswith(prefix) and "/" not in path[len(prefix) :]:
                name = path[len(prefix) :]
                names[name] = {
                    "path": "/" + path,
                    "type": "directory",
                    "size": 0,
                    "modified_at": "2026-10-27T00:00:00Z",
                }
        for path, (data, etag) in self.files.items():
            parent, name = path.rsplit("/", 1) if "/" in path else ("", path)
            if parent == directory:
                names[name] = {
                    "path": "/" + path,
                    "type": "file",
                    "size": len(data),
                    "etag": etag,
                    "modified_at": "2026-10-27T00:00:00Z",
                }
        return [names[name] for name in sorted(names)]


def serve(api: Api) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt: str, *args: object) -> None:
            return

        def _json(self, status: int, payload: object) -> None:
            body = json.dumps(payload).encode()
            self._raw(status, body, {"Content-Type": "application/json"})

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

        def _error(
            self, status: int, code: str, current_etag: str | None = None
        ) -> None:
            detail: dict[str, object] = {"code": code, "message": code}
            if current_etag is not None:
                detail["current_etag"] = current_etag
            self._json(status, {"detail": detail})

        def _parts(self) -> tuple[str, dict[str, list[str]]]:
            parsed = urlsplit(self.path)
            return parsed.path, parse_qs(parsed.query)

        def do_POST(self) -> None:  # noqa: N802
            self._read()
            path, _query = self._parts()
            if path != "/api/v1/mount-sessions":
                self._error(404, "not_found")
                return
            with api.lock:
                api.serial += 1
                session_id = f"sess-{api.serial}"
                record = {
                    "id": session_id,
                    "filesystem_id": "fs1",
                    "mode": "rw",
                    "credential": "token",
                    "state": "active",
                    "created_by": "example",
                    "expires_at": "2099-01-01T00:00:00Z",
                    "created_at": "2026-10-27T00:00:00Z",
                    "token": f"mtblst_{api.serial}",
                }
                api.sessions[session_id] = record
            self._json(201, record)

        def do_DELETE(self) -> None:  # noqa: N802
            path, _query = self._parts()
            if path.startswith("/api/v1/mount-sessions/"):
                session_id = path.rsplit("/", 1)[-1]
                with api.lock:
                    api.sessions.pop(session_id, None)
                self._raw(204, b"", {})
                return
            marker = "/files/"
            if marker not in path:
                self._error(404, "not_found")
                return
            file_path = path.split(marker, 1)[1]
            with api.lock:
                if file_path in api.fail_once:
                    api.fail_once.remove(file_path)
                    self._error(500, "server_error")
                    return
                if file_path in api.files:
                    _data, etag = api.files[file_path]
                    sent = self.headers.get("If-Match")
                    if sent is not None and sent != etag:
                        self._error(412, "precondition_failed", etag)
                        return
                    del api.files[file_path]
                    api.deleted.append(file_path)
                    self._raw(204, b"", {})
                    return
                if file_path in api.dirs:
                    prefix = file_path + "/"
                    busy = any(
                        item.startswith(prefix) for item in (*api.files, *api.dirs)
                    )
                    if busy:
                        self._error(409, "directory_not_empty")
                        return
                    api.dirs.remove(file_path)
                    api.deleted.append(file_path)
                    self._raw(204, b"", {})
                    return
            self._error(404, "not_found")

        def do_PUT(self) -> None:  # noqa: N802
            body = self._read()
            path, _query = self._parts()
            marker = "/files/"
            if marker not in path:
                self._error(404, "not_found")
                return
            file_path = path.split(marker, 1)[1]
            with api.lock:
                current = api.files.get(file_path)
                if self.headers.get("If-None-Match") == "*" and current is not None:
                    self._error(412, "precondition_failed", current[1])
                    return
                sent = self.headers.get("If-Match")
                if sent is not None and (current is None or sent != current[1]):
                    self._error(
                        412,
                        "precondition_failed",
                        None if current is None else current[1],
                    )
                    return
                etag = api.etag_for(body)
                api.files[file_path] = (body, etag)
                api.add_dirs(file_path)
                api.writes += len(body)
            self._json(
                201,
                {
                    "path": "/" + file_path,
                    "type": "file",
                    "size": len(body),
                    "etag": etag,
                    "modified_at": "2026-10-27T00:00:00Z",
                },
            )

        def do_GET(self) -> None:  # noqa: N802
            path, query = self._parts()
            if path.endswith("/usage"):
                with api.lock:
                    self._json(
                        200,
                        {
                            "retained_bytes": sum(
                                len(item[0]) for item in api.files.values()
                            ),
                            "byte_quota": 1073741824,
                            "entries": len(api.files) + len(api.dirs),
                            "entry_quota": 10000,
                            "bytes_read_30d": api.reads,
                            "bytes_written_30d": api.writes,
                            "reserved_cents": 3,
                            "currency": "eur",
                        },
                    )
                return
            if path.endswith("/mount-sessions"):
                with api.lock:
                    listed = [
                        {key: value for key, value in record.items() if key != "token"}
                        for record in api.sessions.values()
                    ]
                self._json(200, listed)
                return
            if "/list/" in path or path.endswith("/list") or path.endswith("/list/"):
                directory = ""
                if "/list/" in path:
                    directory = path.split("/list/", 1)[1]
                with api.lock:
                    if directory and directory not in api.dirs:
                        self._error(404, "not_found")
                        return
                    entries = api.children(directory)
                after = query.get("after", [None])[0]
                if after is not None:
                    entries = [
                        entry
                        for entry in entries
                        if str(entry["path"]).rsplit("/", 1)[-1] > after
                    ]
                limit = int(query.get("limit", ["1000"])[0])
                page, rest = entries[:limit], entries[limit:]
                nxt = (
                    str(page[-1]["path"]).rsplit("/", 1)[-1] if rest and page else None
                )
                self._json(200, {"entries": page, "next": nxt})
                return
            marker = "/files/"
            if marker in path:
                file_path = path.split(marker, 1)[1]
                with api.lock:
                    current = api.files.get(file_path)
                    if current is None:
                        self._error(404, "not_found")
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
                return
            if "/filesystems/fs1" in path:
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
            self._error(404, "not_found")

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


def _report(stdout: str) -> dict[str, object]:
    start = stdout.rfind("\n{")
    payload = stdout[start + 1 :] if start != -1 else stdout
    return json.loads(payload)


def test_live_cleans_a_and_leaves_b_until_its_own_cleanup(
    api_env: Api, capsys: pytest.CaptureFixture[str]
):
    assert main(["live", "--filesystem", "fs1", "--run", RUN]) == 0
    captured = capsys.readouterr()
    assert "b_survived=yes" in captured.out
    assert "a_absent=yes" in captured.out
    assert "after_revoke_b_survived=yes" in captured.out
    assert "after_revoke_a_absent=yes" in captured.out
    assert "caps_unchanged=yes" in captured.out
    assert "revoked_sessions=" in captured.err
    assert "deletion_is_logical=yes revoke_does_not_delete=yes" in captured.err
    assert api_env.files == {}
    assert api_env.sessions == {}
    assert file_b(RUN) in api_env.deleted
    assert file_a(RUN, "out/report.txt") in api_env.deleted
    b_index = api_env.deleted.index(file_b(RUN))
    a_index = api_env.deleted.index(file_a(RUN, "out/report.txt"))
    assert a_index < b_index


def test_changed_file_and_foreign_path_stay(api_env: Api):
    assert main(["write-a", "--filesystem", "fs1", "--run", RUN]) == 0
    assert main(["write-b", "--filesystem", "fs1", "--run", RUN]) == 0
    with Mountable() as client:
        fs = client.filesystem("fs1")
        current = fs.read(file_a(RUN, "out/report.txt"))
        fs.write(
            file_a(RUN, "out/report.txt"), b"changed\n", if_match=current.info.etag
        )
        manifest = fs.read(manifest_path(RUN))
        body = json.loads(manifest.data)
        body["files"].append(
            {"path": file_b(RUN), "etag": api_env.files[file_b(RUN)][1]}
        )
        body["files"].append({"path": "jobs/demo/a/../../demo/b/keep.txt", "etag": "x"})
        fs.write(
            manifest_path(RUN), json.dumps(body).encode(), if_match=manifest.info.etag
        )
        report = cleanup_job_a(fs, RUN)
    assert file_a(RUN, "out/notes.txt") in report["deleted"]
    assert file_b(RUN) not in report["deleted"]
    assert api_env.files[file_b(RUN)][0] == KEEP_BYTES
    assert api_env.files[file_a(RUN, "out/report.txt")][0] == b"changed\n"
    reasons = {item["reason"] for item in report["preserved"]}
    assert "etag_changed" in reasons
    refused = {item["reason"] for item in report["refused"]}
    assert "outside_allowlist" in refused
    assert report["partial"] is True
    assert report["manifest"] == "kept"
    assert manifest_path(RUN) in api_env.files
    assert file_b(RUN) not in api_env.deleted


def test_file_outside_the_manifest_keeps_its_directory(api_env: Api):
    assert main(["write-a", "--filesystem", "fs1", "--run", RUN]) == 0
    extra = f"{prefix_a(RUN)}/out/extra.txt"
    with Mountable() as client:
        fs = client.filesystem("fs1")
        fs.write(extra, b"leave me\n", if_none_match=True)
        report = cleanup_job_a(fs, RUN)
    assert report["partial"] is False
    assert report["manifest"] == "removed"
    assert extra in api_env.files
    assert api_env.files[extra][0] == b"leave me\n"
    kept = {item["path"] for item in report["directories_kept"]}
    assert f"{prefix_a(RUN)}/out" in kept
    assert prefix_a(RUN) in kept
    assert f"{prefix_a(RUN)}/out" not in report["directories_removed"]


def test_directory_in_the_manifest_stays_and_keeps_the_manifest(api_env: Api):
    assert main(["write-a", "--filesystem", "fs1", "--run", RUN]) == 0
    directory = f"{prefix_a(RUN)}/out"
    with Mountable() as client:
        fs = client.filesystem("fs1")
        manifest = fs.read(manifest_path(RUN))
        body = json.loads(manifest.data)
        body["files"] = [{"path": directory, "etag": '"directory"'}]
        fs.write(
            manifest_path(RUN), json.dumps(body).encode(), if_match=manifest.info.etag
        )
        report = cleanup_job_a(fs, RUN)
    assert directory in api_env.dirs
    assert report["partial"] is True
    assert report["manifest"] == "kept"
    assert {"path": directory, "reason": "not_a_file"} in report["preserved"]
    assert manifest_path(RUN) in api_env.files
    assert file_a(RUN, "out/report.txt") in api_env.files


def test_directory_at_job_b_is_not_reported_deleted(api_env: Api):
    assert main(["write-b", "--filesystem", "fs1", "--run", RUN]) == 0
    path = file_b(RUN)
    with api_env.lock:
        del api_env.files[path]
        api_env.dirs.add(path)
    with Mountable() as client:
        fs = client.filesystem("fs1")
        report = cleanup_job_b(fs, RUN)
    assert path in api_env.dirs
    assert path not in report["deleted"]
    assert report["partial"] is True
    assert {"path": path, "reason": "not_a_file"} in report["preserved"]


def test_cleanup_b_leaves_files_that_share_the_jobs_prefix(api_env: Api):
    with api_env.lock:
        api_env.files["jobs"] = (b"root-file\n", '"root"')
        api_env.files[f"jobs/{RUN}"] = (b"other-job\n", '"other"')
    with Mountable() as client:
        report = cleanup_job_b(client.filesystem("fs1"), RUN)
    assert api_env.files["jobs"][0] == b"root-file\n"
    assert api_env.files[f"jobs/{RUN}"][0] == b"other-job\n"
    assert "jobs" not in report["directories_removed"]
    assert f"jobs/{RUN}" not in report["directories_removed"]
    assert file_b(RUN) in report["already_absent"]


def test_cleanup_b_does_not_delete_a_file_named_jobs(api_env: Api):
    assert main(["write-b", "--filesystem", "fs1", "--run", RUN]) == 0
    with api_env.lock:
        api_env.files["jobs"] = (b"root-file\n", '"root"')
    with Mountable() as client:
        report = cleanup_job_b(client.filesystem("fs1"), RUN)
    assert api_env.files["jobs"][0] == b"root-file\n"
    assert "jobs" not in report["directories_removed"]
    assert {"path": "jobs", "reason": "not_a_directory"} in report["directories_kept"]
    assert file_b(RUN) in report["deleted"]


def test_failed_delete_is_partial_and_stops(
    api_env: Api, capsys: pytest.CaptureFixture[str]
):
    assert main(["write-a", "--filesystem", "fs1", "--run", RUN]) == 0
    api_env.fail_once.add(file_a(RUN, "out/notes.txt"))
    assert main(["cleanup-a", "--filesystem", "fs1", "--run", RUN]) == 1
    report = _report(capsys.readouterr().out)
    assert report["error"] == "server_error"
    assert report["partial"] is True
    assert file_a(RUN, "out/report.txt") in report["deleted"]
    assert NOTES_BYTES == api_env.files[file_a(RUN, "out/notes.txt")][0]
    assert REPORT_BYTES not in {item[0] for item in api_env.files.values()}
    assert manifest_path(RUN) in api_env.files


def test_cleanup_refuses_a_manifest_with_too_many_files(api_env: Api):
    assert main(["write-a", "--filesystem", "fs1", "--run", RUN]) == 0
    with Mountable() as client:
        fs = client.filesystem("fs1")
        manifest = fs.read(manifest_path(RUN))
        body = json.loads(manifest.data)
        body["files"] = [
            {"path": f"{prefix_a(RUN)}/out/n{index}.txt", "etag": '"1"'}
            for index in range(33)
        ]
        fs.write(
            manifest_path(RUN), json.dumps(body).encode(), if_match=manifest.info.etag
        )
        report = cleanup_job_a(fs, RUN)
    assert report["error"] == "too_many"
    assert file_a(RUN, "out/report.txt") in api_env.files
    assert api_env.files[file_a(RUN, "out/report.txt")][0] == REPORT_BYTES
