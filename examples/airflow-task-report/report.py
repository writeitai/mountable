"""Hand an Airflow task a report id, and keep the file outside XCom.

``write`` runs the ``synthetic_report`` DAG with ``airflow dags test``.
The producer task uploads a closed synthetic report and returns a 16
character id. That id is the XCom value. The reader task receives the id
and checks the file. ``read`` is a new process with no Airflow DAG run.
It opens a read-only handle and checks the same bytes.

The filesystem id and the API key stay in the environment. Tasks and the
DAG run conf pass the report id only. The user is the fixed fixture
``alice``. This example does not start a scheduler, and it does not keep
Airflow metadata on mountable.io.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import secrets
import sqlite3
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Protocol

EXAMPLE = Path(__file__).resolve().parent
DAG_FILE = EXAMPLE / "dags" / "synthetic_report.py"
PUBLISH_DAG_ID = "synthetic_report"
REPLACE_DAG_ID = "replace_report"
USER = "alice"
MAX_BYTES = 1_048_576
_ID = re.compile(r"[a-f0-9]{16}")
_READY: set[str] = set()


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


def match_line(report_id: str, data: bytes) -> str:
    path = report_path(report_id)
    return f"match {path} sha256={sha256(data)} bytes={len(data)}"


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


def check_stored_report(report_id: str) -> str:
    data = load_report(open_store("ro"), report_id)
    if data != render_report(report_id):
        raise RecipeError("mismatch", "The report bytes changed.")
    return match_line(report_id, data)


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


class FailStore:
    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        raise RuntimeError("storage unavailable")

    def read(self, path: str) -> FileLike:
        raise RuntimeError("storage unavailable")

    def delete(self, path: str) -> None:
        raise RuntimeError("storage unavailable")


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
    kind = os.environ.get("REPORT_STORE", "dir")
    if kind == "fail":
        store: Store = FailStore()
    elif kind == "dir":
        root = Path(os.environ.get("REPORT_DIR", "data"))
        root.mkdir(parents=True, exist_ok=True)
        store = DirStore(root)
    elif kind == "mountable":
        from mountable_sdk import Mountable

        fs_id = os.environ.get("MOUNTABLE_FILESYSTEM_ID", "")
        if not fs_id:
            raise RecipeError(
                "bad_filesystem",
                "Set MOUNTABLE_FILESYSTEM_ID in the environment.",
            )
        store = SdkStore(Mountable().filesystem(fs_id, mode))
    else:
        raise RecipeError("bad_store", "REPORT_STORE must be dir, fail or mountable.")
    return store if mode == "rw" else ReadOnly(store)


def airflow_env(home: Path) -> dict[str, str]:
    env = os.environ.copy()
    env["AIRFLOW_HOME"] = str(home)
    env["AIRFLOW__CORE__LOAD_EXAMPLES"] = "false"
    env["AIRFLOW__CORE__DAGS_FOLDER"] = str(EXAMPLE / "dags")
    env["AIRFLOW__LOGGING__LOGGING_LEVEL"] = "ERROR"
    return env


def airflow_home() -> Path:
    home = Path(os.environ.get("AIRFLOW_HOME", EXAMPLE / ".airflow"))
    home.mkdir(parents=True, exist_ok=True)
    return home


def prepare_airflow(home: Path) -> None:
    key = str(home.resolve())
    if key in _READY and (home / "airflow.db").is_file():
        return
    result = subprocess.run(
        [sys.executable, "-m", "airflow", "db", "migrate"],
        cwd=EXAMPLE,
        env=airflow_env(home),
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).strip()
        raise RecipeError("airflow_db", detail[-500:] or "airflow db migrate failed")
    _READY.add(key)


def run_dag(
    dag_id: str, conf: dict[str, str] | None = None
) -> subprocess.CompletedProcess[str]:
    home = airflow_home()
    prepare_airflow(home)
    command = [
        sys.executable,
        "-m",
        "airflow",
        "dags",
        "test",
        dag_id,
        "-f",
        str(DAG_FILE),
    ]
    if conf is not None:
        command.extend(["-c", json.dumps(conf)])
    return subprocess.run(
        command,
        cwd=EXAMPLE,
        env=airflow_env(home),
        text=True,
        capture_output=True,
        check=False,
    )


def xcom_values(dag_id: str) -> list[str]:
    database = airflow_home() / "airflow.db"
    with sqlite3.connect(database) as connection:
        rows = connection.execute(
            "SELECT value FROM xcom WHERE dag_id = ? AND key = 'return_value'",
            (dag_id,),
        ).fetchall()
    return [row[0] for row in rows]


def _line(stdout: str, prefix: str) -> str | None:
    for line in stdout.splitlines():
        if line.startswith(prefix):
            return line
    return None


def _show(result: subprocess.CompletedProcess[str]) -> None:
    if result.stdout:
        print(
            result.stdout,
            file=sys.stderr,
            end="" if result.stdout.endswith("\n") else "\n",
        )
    if result.stderr:
        print(
            result.stderr,
            file=sys.stderr,
            end="" if result.stderr.endswith("\n") else "\n",
        )


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


def _spawn(args: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(EXAMPLE / "report.py"), *args],
        cwd=EXAMPLE,
        env=os.environ.copy(),
        text=True,
        capture_output=True,
        check=False,
    )


def live_roundtrip() -> int:
    """Run the DAG, then read, reject a replacement, and delete."""
    _print_usage("before")
    wrote = run_dag(PUBLISH_DAG_ID)
    report_line = _line(wrote.stdout, "report_id=")
    if wrote.returncode != 0 or report_line is None:
        _show(wrote)
        print("write_failed", file=sys.stderr)
        return 1
    report_id = report_line.removeprefix("report_id=")
    dag_match = _line(wrote.stdout, "match ")
    read = _spawn(["read", "--report-id", report_id])
    if read.returncode != 0 or not read.stdout.startswith("match "):
        _show(read)
        print("read_failed", file=sys.stderr)
        return 1
    if dag_match is None or read.stdout.strip() != dag_match:
        print("mismatch: the fresh process saw different bytes", file=sys.stderr)
        return 1
    again = run_dag(REPLACE_DAG_ID, {"report_id": report_id})
    if again.returncode == 0:
        print("overwrite_succeeded", file=sys.stderr)
        return 1
    reread = _spawn(["read", "--report-id", report_id])
    if reread.returncode != 0 or reread.stdout.strip() != dag_match:
        print("mismatch: the first report changed", file=sys.stderr)
        return 1
    denied = _spawn(["read-only-write", "--report-id", report_id])
    if denied.returncode == 0:
        print("read_only_write_succeeded", file=sys.stderr)
        return 1
    still = _spawn(["read", "--report-id", report_id])
    if still.returncode != 0 or still.stdout.strip() != dag_match:
        print("mismatch: a read-only write changed the report", file=sys.stderr)
        return 1
    removed = _spawn(["cleanup", "--report-id", report_id])
    if removed.returncode != 0:
        _show(removed)
        print("cleanup_failed", file=sys.stderr)
        return 1
    _remove_empty_parents(open_store("rw"))
    gone = _spawn(["read", "--report-id", report_id])
    if gone.returncode == 0 or "not_found" not in gone.stderr:
        print("absent: the report is still there", file=sys.stderr)
        return 1
    _print_usage("after")
    print(dag_match)
    print(f"report_id={report_id}")
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
            wrote = run_dag(PUBLISH_DAG_ID)
            report_line = _line(wrote.stdout, "report_id=")
            if wrote.returncode != 0 or report_line is None:
                _show(wrote)
                return 1
            print(report_line)
            match = _line(wrote.stdout, "match ")
            if match is not None:
                print(match)
            return 0
        if args.command == "read":
            print(check_stored_report(args.report_id))
            return 0
        if args.command == "overwrite":
            result = run_dag(REPLACE_DAG_ID, {"report_id": args.report_id})
            if result.returncode == 0:
                print(f"report_id={args.report_id}")
                return 0
            _show(result)
            return result.returncode or 1
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
