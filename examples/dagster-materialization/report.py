"""Save a Dagster materialization report so a fresh process can read it.

``write`` materializes two assets in one local run. ``synthetic_rows`` builds
a tiny table in memory. ``materialization_report`` closes a JSON report,
uploads that file, and returns an id. The id is the asset's return value.
The file is not a Dagster I/O manager output, and the run record stays in an
ephemeral local instance.

``read`` is a second process. It does not open that instance. It receives
the id and the run id, opens a read-only handle, and checks the stored run
id, asset key, row count and checksums. ``overwrite`` tries the same id
again and leaves the first file in place. ``cleanup`` deletes that one file.

The filesystem id and the API key stay in the environment. The assets pass
the report id only. The user is the fixed fixture ``alice``.
"""

import argparse
import hashlib
import json
import os
import re
import secrets
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Protocol

os.environ.setdefault("DAGSTER_TELEMETRY_ENABLED", "false")

import dagster as dg

USER = "alice"
ASSET_KEY = "materialization_report"
ROWS = ({"sku": "alpha", "quantity": 1}, {"sku": "beta", "quantity": 2})
MAX_BYTES = 1_048_576
_ID = re.compile(r"[a-f0-9]{16}")


class FileLike(Protocol):
    @property
    def data(self) -> bytes: ...


class Store(Protocol):
    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object: ...
    def read(self, path: str) -> FileLike: ...
    def delete(self, path: str) -> None: ...


_BOUND: Store | None = None


class RecipeError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


def bind_store(store: Store | None) -> None:
    """Tests inject a fake. ``None`` uses the environment again."""
    global _BOUND
    _BOUND = store


def rows_payload() -> list[dict[str, object]]:
    return [dict(row) for row in ROWS]


def rows_sha256(rows: list[dict[str, object]]) -> str:
    raw = json.dumps(rows, separators=(",", ":"), sort_keys=True).encode()
    return hashlib.sha256(raw).hexdigest()


def render_report(
    *,
    report_id: str,
    run_id: str,
    asset_key: str,
    rows: list[dict[str, object]],
) -> bytes:
    payload = {
        "asset_key": asset_key,
        "report_id": report_id,
        "row_count": len(rows),
        "rows": rows,
        "rows_sha256": rows_sha256(rows),
        "run_id": run_id,
        "user": USER,
    }
    return (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode()


def report_path(report_id: str) -> str:
    if not isinstance(report_id, str) or _ID.fullmatch(report_id) is None:
        raise RecipeError(
            "bad_id",
            "The report id must be 16 lowercase hex characters.",
        )
    return f"reports/{USER}/{report_id}.json"


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


@dg.asset
def synthetic_rows() -> list[dict[str, object]]:
    """Build the tiny table in memory. This asset does not upload a file."""
    return rows_payload()


@dg.asset
def materialization_report(
    context: dg.AssetExecutionContext,
    synthetic_rows: list[dict[str, object]],
) -> dg.MaterializeResult:
    """Upload the closed report. The returned value is the id, not the bytes."""
    report_id = new_report_id()
    asset_key = context.asset_key.to_user_string()
    if asset_key != ASSET_KEY:
        raise RecipeError("bad_asset", "The report asset key changed.")
    data = render_report(
        report_id=report_id,
        run_id=context.run.run_id,
        asset_key=asset_key,
        rows=synthetic_rows,
    )
    path = put_report(open_store("rw"), report_id, data)
    return dg.MaterializeResult(
        value=report_id,
        metadata={
            "path": dg.MetadataValue.text(path),
            "row_count": dg.MetadataValue.int(len(synthetic_rows)),
        },
    )


def materialize_report() -> dg.ExecuteInProcessResult:
    """Materialize both assets in one ephemeral local instance."""
    instance = dg.DagsterInstance.ephemeral()
    try:
        return dg.materialize_to_memory(
            [synthetic_rows, materialization_report],
            instance=instance,
            raise_on_error=True,
        )
    finally:
        instance.dispose()


def published_lines(result: dg.ExecuteInProcessResult) -> str:
    report_id = result.output_for_node(ASSET_KEY)
    if not isinstance(report_id, str):
        raise RecipeError("no_id", "The asset did not return a report id.")
    data = render_report(
        report_id=report_id,
        run_id=result.run_id,
        asset_key=ASSET_KEY,
        rows=rows_payload(),
    )
    stored = load_report(open_store("ro"), report_id)
    if stored != data:
        raise RecipeError("mismatch", "The stored report is not the closed file.")
    return "\n".join(
        [
            f"report_id={report_id}",
            f"run_id={result.run_id}",
            f"asset_key={ASSET_KEY}",
            f"row_count={len(rows_payload())}",
            f"rows_sha256={rows_sha256(rows_payload())}",
            f"sha256={sha256(stored)}",
            f"bytes={len(stored)}",
        ]
    )


def verify_report(report_id: str, run_id: str) -> str:
    """Read with a read-only handle and check the run, asset, rows and hashes."""
    data = load_report(open_store("ro"), report_id)
    try:
        payload = json.loads(data)
    except json.JSONDecodeError as exc:
        raise RecipeError("mismatch", "The report is not JSON.") from exc
    rows = rows_payload()
    expected = {
        "asset_key": ASSET_KEY,
        "report_id": report_id,
        "row_count": len(rows),
        "rows": rows,
        "rows_sha256": rows_sha256(rows),
        "run_id": run_id,
        "user": USER,
    }
    if payload != expected:
        raise RecipeError("mismatch", "The report does not match this run.")
    path = report_path(report_id)
    return (
        f"match {path} run_id={run_id} asset_key={ASSET_KEY} "
        f"row_count={len(rows)} rows_sha256={rows_sha256(rows)} "
        f"sha256={sha256(data)} bytes={len(data)}"
    )


def overwrite_report(report_id: str) -> str:
    """Try to replace the file. Create-only leaves the first bytes in place."""
    put_report(open_store("rw"), report_id, b'{"replaced":true}\n')
    return report_id


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
    env["DAGSTER_TELEMETRY_ENABLED"] = "false"
    return subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), *args],
        cwd=Path(__file__).parent,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def _field(stdout: str, name: str) -> str:
    prefix = f"{name}="
    for line in stdout.splitlines():
        if line.startswith(prefix):
            return line.removeprefix(prefix)
    raise RecipeError("no_id", f"The writer did not print {name}.")


def live_roundtrip() -> int:
    """Write in one process, then read, reject a replacement, and delete."""
    _print_usage("before")
    wrote = _spawn(["write"])
    if wrote.returncode != 0:
        print(wrote.stderr, file=sys.stderr)
        print("write_failed", file=sys.stderr)
        return 1
    report_id = _field(wrote.stdout, "report_id")
    run_id = _field(wrote.stdout, "run_id")
    read = _spawn(["read", "--report-id", report_id, "--run-id", run_id])
    if read.returncode != 0 or not read.stdout.startswith("match "):
        print(read.stderr, file=sys.stderr)
        print("read_failed", file=sys.stderr)
        return 1
    again = _spawn(["overwrite", "--report-id", report_id])
    if again.returncode == 0:
        print("overwrite_succeeded", file=sys.stderr)
        return 1
    reread = _spawn(["read", "--report-id", report_id, "--run-id", run_id])
    if reread.returncode != 0 or reread.stdout.strip() != read.stdout.strip():
        print("mismatch: the first report changed", file=sys.stderr)
        return 1
    denied = _spawn(["read-only-write", "--report-id", report_id])
    if denied.returncode == 0:
        print("read_only_write_succeeded", file=sys.stderr)
        return 1
    still = _spawn(["read", "--report-id", report_id, "--run-id", run_id])
    if still.returncode != 0 or still.stdout.strip() != read.stdout.strip():
        print("mismatch: a read-only write changed the report", file=sys.stderr)
        return 1
    removed = _spawn(["cleanup", "--report-id", report_id])
    if removed.returncode != 0:
        print(removed.stderr, file=sys.stderr)
        print("cleanup_failed", file=sys.stderr)
        return 1
    _remove_empty_parents(open_store("rw"))
    gone = _spawn(["read", "--report-id", report_id, "--run-id", run_id])
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
    read = sub.add_parser("read")
    read.add_argument("--report-id", required=True)
    read.add_argument("--run-id", required=True)
    for name in ("overwrite", "cleanup", "read-only-write"):
        command = sub.add_parser(name)
        command.add_argument("--report-id", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "live":
            return live_roundtrip()
        if args.command == "write":
            print(published_lines(materialize_report()))
            return 0
        if args.command == "read":
            print(verify_report(args.report_id, args.run_id))
            return 0
        if args.command == "overwrite":
            overwrite_report(args.report_id)
            print(f"report_id={args.report_id}")
            return 0
        if args.command == "read-only-write":
            put_report(open_store("ro"), args.report_id, b'{"replaced":true}\n')
            print(f"report_id={args.report_id}")
            return 0
        path = delete_report(args.report_id)
        print(f"deleted {path}")
        return 0
    except Exception as exc:  # noqa: BLE001 - CLI maps asset and storage failures to an exit code
        return _fail(exc)


if __name__ == "__main__":
    raise SystemExit(main())
