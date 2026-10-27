"""Save a Prefect task report so a fresh flow process can read it.

``write`` runs two real tasks. The first chooses a report id. The second
uploads the closed file and returns that same id. The id is the task's
return value. It is not the file, and this example does not persist it as
a Prefect result, cache it, or treat it as orchestration state.

``read`` is a second process. Its flow receives only the id and opens a
read-only handle. It checks the size and the SHA-256. ``overwrite`` tries
the same id again and leaves the first file in place. ``cleanup`` deletes
that one file.

The filesystem id and the API key stay in the environment. Tasks pass the
report id only. The user is the fixed fixture ``alice``.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import re
import secrets
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Protocol

os.environ.setdefault("PREFECT_LOGGING_LEVEL", "ERROR")
os.environ.setdefault("PREFECT_HOME", str(Path(__file__).parent / ".prefect"))

from prefect import flow, task
from prefect.cache_policies import NO_CACHE

USER = "alice"
MAX_BYTES = 1_048_576
_ID = re.compile(r"[a-f0-9]{16}")
_BOUND: Store | None = None


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


def bind_store(store: Store | None) -> None:
    """Tests inject a fake. ``None`` uses the environment again."""
    global _BOUND
    _BOUND = store


def render_report(report_id: str) -> bytes:
    text = (
        f"report_id: {report_id}\n"
        f"user: {USER}\n"
        "kind: synthetic\n"
        "item,quantity\n"
        "alpha,1\n"
        "beta,2\n"
    )
    return text.encode()


def report_path(report_id: str) -> str:
    if not isinstance(report_id, str) or _ID.fullmatch(report_id) is None:
        raise RecipeError(
            "bad_id",
            "The report id must be 16 lowercase hex characters.",
        )
    return f"reports/{USER}/{report_id}.txt"


def new_report_id() -> str:
    return secrets.token_hex(8)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _mapped(exc: Exception) -> RecipeError | None:
    status = getattr(exc, "status", None)
    code = getattr(exc, "code", None)
    if status == 412 or code == "precondition_failed":
        return RecipeError("exists", "The report already exists.")
    if status == 403 or code in {"read_only", "forbidden"}:
        return RecipeError("read_only", "This handle cannot write.")
    if status == 404 or code == "not_found" or isinstance(exc, FileNotFoundError):
        return RecipeError("not_found", "The report is not there.")
    return None


def put_report(store: Store, report_id: str, data: bytes) -> str:
    if len(data) > MAX_BYTES:
        raise RecipeError("too_large", "The report is larger than 1 MiB.")
    path = report_path(report_id)
    try:
        store.write(path, data, if_none_match=True)
    except RecipeError:
        raise
    except Exception as exc:
        mapped = _mapped(exc)
        if mapped is not None:
            raise mapped from exc
        raise
    return path


def load_report(store: Store, report_id: str) -> bytes:
    path = report_path(report_id)
    try:
        return store.read(path).data
    except RecipeError:
        raise
    except Exception as exc:
        mapped = _mapped(exc)
        if mapped is not None:
            raise mapped from exc
        raise


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


class ReadOnly:
    def __init__(self, inner: Store) -> None:
        self.inner = inner

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        raise RecipeError("read_only", "This handle cannot write.")

    def read(self, path: str) -> FileLike:
        return self.inner.read(path)

    def delete(self, path: str) -> None:
        raise RecipeError("read_only", "This handle cannot delete.")


class SdkStore:
    def __init__(self, filesystem: object) -> None:
        self.filesystem = filesystem

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        return self.filesystem.write(path, data, if_none_match=if_none_match)  # type: ignore[attr-defined]

    def read(self, path: str) -> FileLike:
        return self.filesystem.read(path)  # type: ignore[attr-defined]

    def delete(self, path: str) -> None:
        try:
            self.filesystem.delete(path)  # type: ignore[attr-defined]
        except Exception as exc:
            mapped = _mapped(exc)
            if mapped is not None and mapped.code == "not_found":
                return
            raise


def open_store(mode: str) -> Store:
    if mode not in {"rw", "ro"}:
        raise RecipeError("bad_mode", "The mode must be rw or ro.")
    if _BOUND is not None:
        return _BOUND if mode == "rw" else ReadOnly(_BOUND)
    kind = os.environ.get("REPORT_STORE", "dir")
    if kind == "dir":
        root = Path(os.environ.get("REPORT_DIR", "data"))
        root.mkdir(parents=True, exist_ok=True)
        store: Store = DirStore(root)
        return store if mode == "rw" else ReadOnly(store)
    if kind == "mountable":
        from mountable_sdk import Mountable

        fs_id = os.environ.get("MOUNTABLE_FILESYSTEM_ID", "")
        if not fs_id:
            raise RecipeError(
                "bad_filesystem",
                "Set MOUNTABLE_FILESYSTEM_ID in the environment.",
            )
        return SdkStore(Mountable().filesystem(fs_id, mode))
    raise RecipeError("bad_store", "REPORT_STORE must be dir or mountable.")


@task(persist_result=False, cache_policy=NO_CACHE)
def choose_report_id() -> str:
    """Pick an id. This task does not touch the filesystem."""
    return new_report_id()


@task(persist_result=False, cache_policy=NO_CACHE)
def upload_report(report_id: str) -> str:
    """Upload the closed report. The return value is the id, not the bytes."""
    put_report(open_store("rw"), report_id, render_report(report_id))
    return report_id


@task(persist_result=False, cache_policy=NO_CACHE)
def overwrite_report(report_id: str) -> str:
    """Try to replace the file. Create-only leaves the first bytes in place."""
    put_report(open_store("rw"), report_id, b"replaced\n")
    return report_id


@task(persist_result=False, cache_policy=NO_CACHE)
def verify_report(report_id: str) -> str:
    """Read with a read-only handle and check the size and the hash."""
    data = load_report(open_store("ro"), report_id)
    if data != render_report(report_id):
        raise RecipeError("mismatch", "The report bytes changed.")
    path = report_path(report_id)
    return f"match {path} sha256={sha256(data)} bytes={len(data)}"


@flow
def publish_flow() -> str:
    report_id = choose_report_id()
    return upload_report(report_id)


@flow
def read_flow(report_id: str) -> str:
    return verify_report(report_id)


@flow
def overwrite_flow(report_id: str) -> str:
    return overwrite_report(report_id)


def delete_report(report_id: str) -> str:
    path = report_path(report_id)
    store = open_store("rw")
    try:
        store.delete(path)
    except Exception as exc:
        mapped = _mapped(exc)
        if mapped is None or mapped.code != "not_found":
            raise
    return path


def _remove_empty_parents(store: Store) -> None:
    if not isinstance(store, SdkStore):
        return
    for path in (f"reports/{USER}", "reports"):
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
        f"reserved_cents={usage.reserved_cents} currency={usage.currency}",
        file=sys.stderr,
    )


def _spawn(args: list[str]) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["PREFECT_LOGGING_LEVEL"] = "ERROR"
    env.setdefault("PREFECT_HOME", os.environ.get("PREFECT_HOME", ""))
    return subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), *args],
        cwd=Path(__file__).parent,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def _id_from_output(stdout: str) -> str:
    for line in stdout.splitlines():
        if line.startswith("report_id="):
            return line.removeprefix("report_id=")
    raise RecipeError("no_id", "The writer did not print a report id.")


def live_roundtrip() -> int:
    """Write in one process, then read, reject a replacement, and delete."""
    _print_usage("before")
    wrote = _spawn(["write"])
    if wrote.returncode != 0:
        print(wrote.stderr, file=sys.stderr)
        print("write_failed", file=sys.stderr)
        return 1
    report_id = _id_from_output(wrote.stdout)
    read = _spawn(["read", "--report-id", report_id])
    if read.returncode != 0 or not read.stdout.startswith("match "):
        print(read.stderr, file=sys.stderr)
        print("read_failed", file=sys.stderr)
        return 1
    again = _spawn(["overwrite", "--report-id", report_id])
    if again.returncode == 0:
        print("overwrite_succeeded", file=sys.stderr)
        return 1
    reread = _spawn(["read", "--report-id", report_id])
    if reread.returncode != 0 or reread.stdout.strip() != read.stdout.strip():
        print("mismatch: the first report changed", file=sys.stderr)
        return 1
    denied = _spawn(["read-only-write", "--report-id", report_id])
    if denied.returncode == 0:
        print("read_only_write_succeeded", file=sys.stderr)
        return 1
    still = _spawn(["read", "--report-id", report_id])
    if still.returncode != 0 or still.stdout.strip() != read.stdout.strip():
        print("mismatch: a read-only write changed the report", file=sys.stderr)
        return 1
    removed = _spawn(["cleanup", "--report-id", report_id])
    if removed.returncode != 0:
        print(removed.stderr, file=sys.stderr)
        print("cleanup_failed", file=sys.stderr)
        return 1
    _remove_empty_parents(open_store("rw"))
    gone = _spawn(["read", "--report-id", report_id])
    if gone.returncode == 0 or "not_found" not in gone.stderr:
        print("absent: the report is still there", file=sys.stderr)
        return 1
    _print_usage("after")
    print(read.stdout.strip())
    print("preserved: exists")
    print("read_only: denied")
    print(f"deleted {report_path(report_id)}")
    print("absent: not_found")
    return 0


def _fail(exc: BaseException) -> int:
    if isinstance(exc, RecipeError):
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2
    print(f"failed: {exc}", file=sys.stderr)
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("write")
    sub.add_parser("live")
    for name in ("read", "overwrite", "cleanup", "read-only-write"):
        command = sub.add_parser(name)
        command.add_argument("--report-id", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "live":
            return live_roundtrip()
        if args.command == "write":
            report_id = publish_flow()
            print(f"report_id={report_id}")
            return 0
        if args.command == "read":
            print(read_flow(args.report_id))
            return 0
        if args.command == "overwrite":
            overwrite_flow(args.report_id)
            print(f"report_id={args.report_id}")
            return 0
        if args.command == "read-only-write":
            put_report(open_store("ro"), args.report_id, b"replaced\n")
            print(f"report_id={args.report_id}")
            return 0
        path = delete_report(args.report_id)
        print(f"deleted {path}")
        return 0
    except Exception as exc:  # noqa: BLE001 - CLI maps task and storage failures to an exit code
        return _fail(exc)


if __name__ == "__main__":
    raise SystemExit(main())
