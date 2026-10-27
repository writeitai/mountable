"""Save a generated report so a fresh FastAPI process can return it.

The HTTP API authenticates a local fixture user and returns an opaque report
id. The filesystem id and the storage path stay in trusted code. ``POST
/reports`` returns that id only after the upload succeeds. ``GET
/reports/{id}`` reads the caller's own file. A different fixture user gets
not found, because the server looks only in that user's prefix.

``REPORT_STORE=dir`` keeps files in ``REPORT_DIR`` so two local processes can
share them. ``REPORT_STORE=mountable`` uses the Python SDK. Set
``MOUNTABLE_API_KEY`` in the environment and ``MOUNTABLE_FILESYSTEM_ID`` to an
existing filesystem. The API key is never printed.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import re
import secrets
import subprocess
import sys
import time
from pathlib import Path
from types import SimpleNamespace
from typing import Protocol

from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.responses import Response

_ID = re.compile(r"[a-f0-9]{16}")
USERS = {"fixture-alice": "alice", "fixture-bob": "bob"}


class FileLike(Protocol):
    @property
    def data(self) -> bytes: ...


class Store(Protocol):
    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object: ...
    def read(self, path: str) -> FileLike: ...
    def delete(self, path: str) -> None: ...


class RecipeError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


def render_report(user: str, report_id: str) -> bytes:
    text = (
        f"report_id: {report_id}\n"
        f"user: {user}\n"
        "kind: synthetic\n"
        "item,quantity\n"
        "alpha,1\n"
        "beta,2\n"
    )
    return text.encode()


def report_path(user: str, report_id: str) -> str:
    if user not in USERS.values():
        raise RecipeError("unknown_user", "The user is not in the fixture map.")
    if not isinstance(report_id, str) or _ID.fullmatch(report_id) is None:
        raise RecipeError(
            "bad_id",
            "The report id must be 16 lowercase hex characters.",
        )
    return f"reports/{user}/{report_id}.txt"


def new_report_id() -> str:
    return secrets.token_hex(8)


def publish_report(store: Store, user: str) -> str:
    """Upload the closed report, then return its id."""
    report_id = new_report_id()
    store.write(
        report_path(user, report_id),
        render_report(user, report_id),
        if_none_match=True,
    )
    return report_id


def read_report(store: Store, user: str, report_id: str) -> bytes:
    path = report_path(user, report_id)
    try:
        return store.read(path).data
    except FileNotFoundError as exc:
        raise RecipeError("not_found", "The report is not there.") from exc
    except Exception as exc:
        if getattr(exc, "status", None) == 404:
            raise RecipeError("not_found", "The report is not there.") from exc
        raise


def delete_report(store: Store, user: str, report_id: str) -> str:
    path = report_path(user, report_id)
    try:
        store.delete(path)
    except Exception as exc:
        if getattr(exc, "status", None) != 404 and not isinstance(
            exc, FileNotFoundError
        ):
            raise
    return path


class DirStore:
    def __init__(self, root: Path) -> None:
        self.root = root

    def _dest(self, path: str) -> Path:
        root = self.root.resolve()
        dest = (root / path).resolve()
        if dest != root and root not in dest.parents:
            raise RecipeError("bad_path", "The path leaves the report directory.")
        return dest

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        dest = self._dest(path)
        dest.parent.mkdir(parents=True, exist_ok=True)
        flags = os.O_CREAT | os.O_WRONLY
        if if_none_match:
            flags |= os.O_EXCL
        else:
            flags |= os.O_TRUNC
        try:
            fd = os.open(dest, flags, 0o644)
        except FileExistsError as exc:
            raise RecipeError("exists", "The report already exists.") from exc
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
        return dest

    def read(self, path: str) -> FileLike:
        dest = self._dest(path)
        if not dest.is_file():
            raise FileNotFoundError(path)
        return SimpleNamespace(data=dest.read_bytes())

    def delete(self, path: str) -> None:
        dest = self._dest(path)
        try:
            dest.unlink()
        except FileNotFoundError:
            return


class SdkStore:
    def __init__(self, filesystem: object) -> None:
        self.filesystem = filesystem

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        return self.filesystem.write(path, data, if_none_match=if_none_match)  # type: ignore[attr-defined]

    def read(self, path: str) -> FileLike:
        return self.filesystem.read(path)  # type: ignore[attr-defined]

    def delete(self, path: str) -> None:
        self.filesystem.delete(path)  # type: ignore[attr-defined]


def store_from_env() -> Store:
    kind = os.environ.get("REPORT_STORE", "dir")
    if kind == "dir":
        root = Path(os.environ.get("REPORT_DIR", "data"))
        root.mkdir(parents=True, exist_ok=True)
        return DirStore(root)
    if kind == "mountable":
        from mountable_sdk import Mountable

        fs_id = os.environ.get("MOUNTABLE_FILESYSTEM_ID", "")
        if not fs_id:
            raise RecipeError(
                "bad_filesystem",
                "Set MOUNTABLE_FILESYSTEM_ID in the environment.",
            )
        return SdkStore(Mountable().filesystem(fs_id, "rw"))
    raise RecipeError("bad_store", "REPORT_STORE must be dir or mountable.")


def _user_from_header(authorization: str | None) -> str:
    if authorization is None or not authorization.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="unauthorized")
    user = USERS.get(authorization.removeprefix("Bearer ").strip())
    if user is None:
        raise HTTPException(status_code=401, detail="unauthorized")
    return user


def create_app(store: Store) -> FastAPI:
    app = FastAPI()

    def current_user(authorization: str | None = Header(default=None)) -> str:
        return _user_from_header(authorization)

    @app.get("/healthz")
    def healthz() -> dict[str, bool]:
        return {"ok": True}

    @app.post("/reports", status_code=201)
    def create_report(user: str = Depends(current_user)) -> dict[str, str]:
        try:
            report_id = publish_report(store, user)
        except Exception as exc:
            raise HTTPException(status_code=503, detail="upload_failed") from exc
        return {"id": report_id}

    @app.get("/reports/{report_id}")
    def fetch_report(report_id: str, user: str = Depends(current_user)) -> Response:
        try:
            data = read_report(store, user, report_id)
        except RecipeError as exc:
            status = 404 if exc.code in {"not_found", "bad_id"} else 503
            detail = "not_found" if status == 404 else "read_failed"
            raise HTTPException(status_code=status, detail=detail) from exc
        return Response(content=data, media_type="text/plain")

    return app


def create_default_app() -> FastAPI:
    return create_app(store_from_env())


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _wait_ready(base: str) -> None:
    import httpx

    deadline = time.monotonic() + 10
    last = "the server did not answer"
    with httpx.Client(trust_env=False, timeout=0.5) as client:
        while time.monotonic() < deadline:
            try:
                response = client.get(base + "/healthz")
                if response.status_code == 200:
                    return
                last = f"healthz returned {response.status_code}"
            except httpx.HTTPError as exc:
                last = str(exc)
            time.sleep(0.05)
    raise RecipeError("not_ready", last)


def _spawn(port: int) -> subprocess.Popen[bytes]:
    return subprocess.Popen(
        [
            sys.executable,
            "-m",
            "uvicorn",
            "app:create_default_app",
            "--factory",
            "--host",
            "127.0.0.1",
            "--port",
            str(port),
            "--log-level",
            "warning",
        ],
        cwd=Path(__file__).parent,
        env=os.environ.copy(),
    )


def _stop(proc: subprocess.Popen[bytes]) -> None:
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def _free_port() -> int:
    import socket

    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def serve_once(port: int):
    """Start one local server. The caller stops it."""
    proc = _spawn(port)
    try:
        _wait_ready(f"http://127.0.0.1:{port}")
    except Exception:
        _stop(proc)
        raise
    return proc


def _client():
    import httpx

    return httpx.Client(trust_env=False, timeout=30)


def _remove_empty_parents(store: Store, user: str) -> None:
    if not isinstance(store, SdkStore):
        return
    for path in (f"reports/{user}", "reports"):
        try:
            store.delete(path)
        except Exception as exc:
            status = getattr(exc, "status", None)
            if status is None or status >= 500:
                raise


def _print_usage(label: str) -> None:
    if os.environ.get("REPORT_STORE") != "mountable":
        return
    from mountable_sdk import Mountable

    usage = Mountable().filesystem_usage(os.environ["MOUNTABLE_FILESYSTEM_ID"])
    print(
        f"usage {label} retained_bytes={usage.retained_bytes} "
        f"entries={usage.entries} bytes_read_30d={usage.bytes_read_30d} "
        f"bytes_written_30d={usage.bytes_written_30d} "
        f"reserved_cents={usage.reserved_cents} currency={usage.currency}"
    )


def live_roundtrip() -> int:
    """Create a report, restart the server, and read the same bytes back."""
    _print_usage("before")
    alice = {"Authorization": "Bearer fixture-alice"}
    bob = {"Authorization": "Bearer fixture-bob"}
    port = _free_port()
    base = f"http://127.0.0.1:{port}"
    proc = serve_once(port)
    try:
        with _client() as client:
            created = client.post(base + "/reports", headers=alice)
            if created.status_code != 201 or "id" not in created.json():
                print(f"create_failed: {created.status_code}", file=sys.stderr)
                return 1
            report_id = created.json()["id"]
            first = client.get(base + "/reports/" + report_id, headers=alice)
    finally:
        _stop(proc)
    if first.status_code != 200:
        print(f"read_failed: {first.status_code}", file=sys.stderr)
        return 1
    first_hash = _sha256(first.content)
    port = _free_port()
    base = f"http://127.0.0.1:{port}"
    proc = serve_once(port)
    try:
        with _client() as client:
            second = client.get(base + "/reports/" + report_id, headers=alice)
            other = client.get(base + "/reports/" + report_id, headers=bob)
    finally:
        _stop(proc)
    if second.status_code != 200 or _sha256(second.content) != first_hash:
        print("mismatch: the report bytes changed across the restart", file=sys.stderr)
        return 1
    if other.status_code != 404:
        print(f"other_user: expected 404, got {other.status_code}", file=sys.stderr)
        return 1
    store = store_from_env()
    path = delete_report(store, "alice", report_id)
    try:
        read_report(store, "alice", report_id)
    except RecipeError as exc:
        if exc.code != "not_found":
            raise
        print("absent: 404")
    else:
        print("absent: the report is still there", file=sys.stderr)
        return 1
    _remove_empty_parents(store, "alice")
    _print_usage("after")
    print(f"match {path} sha256={first_hash} bytes={len(first.content)}")
    print("other_user: 404")
    print(f"deleted {path}")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("live")
    cleanup = sub.add_parser("cleanup")
    cleanup.add_argument("--user", required=True, choices=sorted(set(USERS.values())))
    cleanup.add_argument("--report-id", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "live":
            return live_roundtrip()
        path = delete_report(store_from_env(), args.user, args.report_id)
        print(f"deleted {path}")
        return 0
    except RecipeError as exc:
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
