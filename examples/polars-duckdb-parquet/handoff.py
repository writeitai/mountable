"""Hand a closed Parquet dataset from Polars to DuckDB through mountable.io.

``write`` builds a small deterministic table with Polars, closes it as a
local Parquet file, then uploads those bytes create-only. The writer can
exit. ``check`` is a fresh process: it downloads the file read-only, opens
the downloaded local file with DuckDB, and verifies the schema, the row
count, the SHA-256 and one known aggregate. ``cleanup`` deletes the file.
Credentials stay in the caller's secrets; the API key is never printed.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import sys
from datetime import date, timedelta
from pathlib import Path
from typing import Protocol

MAX_BYTES = 1_048_576
ROWS = 90
_RUN = re.compile(r"[a-z0-9][a-z0-9-]{0,39}")
REGIONS = ("north", "south", "east", "west")
EXPECTED_COLUMNS = ["day", "region", "units", "revenue_cents"]


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


def dataset_path(run: str) -> str:
    if not isinstance(run, str) or _RUN.fullmatch(run) is None:
        raise RecipeError(
            "bad_run",
            "The run id must be 1 to 40 lowercase letters, digits or hyphens, "
            "and start with a letter or digit.",
        )
    return f"parquet/{run}/daily.parquet"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def build_frame():
    """Deterministic synthetic sales rows; no randomness, no real data."""
    import polars as pl

    start = date(2026, 1, 1)
    days = [start + timedelta(days=i) for i in range(ROWS)]
    units = [(i * 7) % 13 + 1 for i in range(ROWS)]
    return pl.DataFrame(
        {
            "day": days,
            "region": [REGIONS[i % len(REGIONS)] for i in range(ROWS)],
            "units": units,
            "revenue_cents": [u * 199 for u in units],
        }
    )


def expected_totals() -> tuple[int, int]:
    frame = build_frame()
    return frame.height, int(frame["units"].sum())


def write_parquet_bytes() -> bytes:
    import io

    buf = io.BytesIO()
    build_frame().write_parquet(buf)
    return buf.getvalue()


def upload_dataset(store: Store, run: str, data: bytes) -> str:
    path = dataset_path(run)
    if len(data) > MAX_BYTES:
        raise RecipeError("too_large", "The Parquet file is larger than 1 MiB.")
    store.write(path, data, if_none_match=True)
    return f"{path} sha256={sha256(data)} bytes={len(data)}"


def query_local_parquet(local_file: Path) -> tuple[list[str], int, int]:
    """Query the downloaded local file; returns (columns, rows, units)."""
    import duckdb

    con = duckdb.connect()
    try:
        cols = [
            r[0]
            for r in con.execute(
                "SELECT column_name FROM (DESCRIBE SELECT * FROM read_parquet(?))",
                [str(local_file)],
            ).fetchall()
        ]
        rows, units = con.execute(
            "SELECT count(*), sum(units) FROM read_parquet(?)", [str(local_file)]
        ).fetchone()
    finally:
        con.close()
    return cols, int(rows), int(units)


def check_dataset(store: Store, run: str, work_dir: Path) -> list[str]:
    path = dataset_path(run)
    data = store.read(path).data
    work_dir.mkdir(parents=True, exist_ok=True)
    local_file = work_dir / "daily.parquet"
    local_file.write_bytes(data)
    cols, rows, units = query_local_parquet(local_file)
    if cols != EXPECTED_COLUMNS:
        raise RecipeError("bad_schema", f"Unexpected columns: {cols}")
    want_rows, want_units = expected_totals()
    if (rows, units) != (want_rows, want_units):
        raise RecipeError(
            "bad_totals", f"Got rows={rows} units={units}, want {want_rows}/{want_units}."
        )
    return [
        f"match {path} sha256={sha256(data)} bytes={len(data)}",
        f"parquet rows={rows} units_sum={units} columns={','.join(cols)}",
    ]


def cleanup_dataset(store: Store, run: str) -> list[str]:
    path = dataset_path(run)
    try:
        store.delete(path)
    except Exception as exc:
        if getattr(exc, "status", None) != 404:
            raise
    return [f"deleted {path}"]


_FS_ID = ""


def _store(mode: str):  # pragma: no cover - live path, offline tests inject fakes
    from mountable_sdk import Mountable

    if not _FS_ID:
        raise RecipeError("bad_filesystem", "Pass --filesystem FS_ID.")
    return Mountable().filesystem(_FS_ID, mode)  # type: ignore[arg-type]


def main(argv: list[str] | None = None) -> int:
    global _FS_ID
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("write", "check", "cleanup"):
        p = sub.add_parser(name)
        p.add_argument("--filesystem", required=True)
        p.add_argument("--run", required=True)
        p.add_argument("--dir", default="out")
    args = parser.parse_args(argv)
    _FS_ID = args.filesystem
    try:
        dataset_path(args.run)
        if args.command == "write":
            print(upload_dataset(_store("rw"), args.run, write_parquet_bytes()))
        elif args.command == "check":
            for line in check_dataset(_store("ro"), args.run, Path(args.dir)):
                print(line)
        else:
            for line in cleanup_dataset(_store("rw"), args.run):
                print(line)
        return 0
    except RecipeError as exc:
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
