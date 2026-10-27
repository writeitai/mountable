"""Resume small CSV-to-Parquet batches after a worker restart.

``run`` commits one batch and exits. It builds one bounded slice of a
deterministic synthetic CSV, closes it as a Parquet file, uploads that file
under a unique name, reads it back to verify the checksum, and only then
records the batch in a small JSON manifest. The manifest update uses the
ETag from the manifest read, so a competing writer stops the run instead
of being overwritten. ``check`` is a fresh process: it downloads only the
committed Parquet files and checks the row count and unique ids.
``cleanup`` deletes the manifest and the batch files.

If a worker stops after the upload but before the manifest update, the
next ``run`` finds that uploaded file, verifies it against the rebuilt
batch, and records it without uploading again. No batch is counted twice
and no unrecorded batch is reported as committed. Credentials stay in the
caller's secrets; the API key is never printed.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import re
import sys
from contextlib import suppress
from pathlib import Path
from typing import Protocol

BATCHES = 3
ROWS_PER_BATCH = 20
MAX_ARTIFACT_BYTES = 262_144
MAX_MANIFEST_BYTES = 16_384
_RUN = re.compile(r"[a-z0-9][a-z0-9-]{0,39}")
REGIONS = ("north", "south", "east", "west")
COLUMNS = ["id", "day", "region", "units", "revenue_cents"]


class FileLike(Protocol):
    @property
    def data(self) -> bytes: ...


class Store(Protocol):
    def write(
        self,
        path: str,
        data: bytes,
        *,
        if_none_match: bool = False,
        if_match: str | None = None,
    ) -> object: ...

    def read(self, path: str) -> FileLike: ...

    def delete(self, path: str) -> None: ...


class RecipeError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


class SimulatedCrash(Exception):
    """Raised by --crash-after-upload after upload, before the manifest."""


def _check_run(run: str) -> None:
    if not isinstance(run, str) or _RUN.fullmatch(run) is None:
        raise RecipeError(
            "bad_run",
            "The run id must be 1 to 40 lowercase letters, digits or hyphens, "
            "and start with a letter or digit.",
        )


def manifest_path(run: str) -> str:
    _check_run(run)
    return f"batches/{run}/manifest.json"


def batch_path(run: str, index: int) -> str:
    _check_run(run)
    if not 0 <= index < BATCHES:
        raise RecipeError("bad_batch", f"Batch index must be 0 to {BATCHES - 1}.")
    return f"batches/{run}/batch-{index:03d}.parquet"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def csv_text() -> str:
    """Deterministic synthetic CSV: 60 rows, ids 1 to 60, no real data."""
    lines = [",".join(COLUMNS)]
    for i in range(BATCHES * ROWS_PER_BATCH):
        units = (i * 7) % 13 + 1
        lines.append(
            f"{i + 1},2026-01-{(i % 28) + 1:02d},{REGIONS[i % len(REGIONS)]},"
            f"{units},{units * 199}"
        )
    return "\n".join(lines) + "\n"


def batch_parquet_bytes(index: int) -> bytes:
    """Close batch ``index`` of the CSV as Parquet bytes."""
    import polars as pl

    if not 0 <= index < BATCHES:
        raise RecipeError("bad_batch", f"Batch index must be 0 to {BATCHES - 1}.")
    frame = pl.read_csv(io.StringIO(csv_text()))
    start = index * ROWS_PER_BATCH
    batch = frame.slice(start, ROWS_PER_BATCH)
    buf = io.BytesIO()
    batch.write_parquet(buf)
    data = buf.getvalue()
    if len(data) > MAX_ARTIFACT_BYTES:
        raise RecipeError("too_large", "A batch Parquet file is larger than 256 KiB.")
    return data


def _read_manifest(store: Store, run: str) -> tuple[dict, str | None]:
    """Return (manifest, etag); a missing manifest means nothing committed."""
    from mountable_sdk import MountableError

    try:
        got = store.read(manifest_path(run))
    except MountableError as exc:
        if exc.status == 404:
            return {"run": run, "batches": []}, None
        raise
    if len(got.data) > MAX_MANIFEST_BYTES:
        raise RecipeError("bad_manifest", "The manifest is larger than 16 KiB.")
    try:
        manifest = json.loads(got.data)
    except (ValueError, UnicodeDecodeError) as exc:
        raise RecipeError("bad_manifest", "The manifest is not valid JSON.") from exc
    if manifest.get("run") != run or not isinstance(manifest.get("batches"), list):
        raise RecipeError("bad_manifest", "The manifest does not match this run.")
    etag = getattr(getattr(got, "info", None), "etag", None)
    return manifest, etag


def _write_manifest(store: Store, run: str, manifest: dict, etag: str | None) -> None:
    from mountable_sdk import MountableError

    data = json.dumps(manifest, sort_keys=True).encode()
    if len(data) > MAX_MANIFEST_BYTES:
        raise RecipeError("bad_manifest", "The manifest is larger than 16 KiB.")
    try:
        if etag is None:
            store.write(manifest_path(run), data, if_none_match=True)
        else:
            store.write(manifest_path(run), data, if_match=etag)
    except MountableError as exc:
        if exc.status == 412:
            raise RecipeError(
                "competing_writer",
                "The manifest changed since it was read. Stop and check the "
                "other writer; this run did not overwrite it.",
            ) from exc
        raise


def run_next(store: Store, run: str, *, crash_after_upload: bool = False) -> list[str]:
    """Commit the next batch, or adopt an uploaded batch a crash left behind."""
    from mountable_sdk import MountableError

    manifest, etag = _read_manifest(store, run)
    committed = {b["index"] for b in manifest["batches"]}
    remaining = [i for i in range(BATCHES) if i not in committed]
    if not remaining:
        return [f"done run={run} batches={BATCHES} rows={BATCHES * ROWS_PER_BATCH}"]
    index = remaining[0]
    path = batch_path(run, index)
    data = batch_parquet_bytes(index)
    digest = sha256(data)
    adopted = False
    try:
        existing = store.read(path)
    except MountableError as exc:
        if exc.status != 404:
            raise
        existing = None
    if existing is not None:
        if sha256(existing.data) != digest:
            raise RecipeError(
                "orphan_mismatch",
                f"{path} exists but does not match the rebuilt batch. "
                "Stop and inspect it; nothing was recorded.",
            )
        adopted = True
    else:
        uploaded = False
        try:
            store.write(path, data, if_none_match=True)
            uploaded = True
        except MountableError as exc:
            if exc.status != 412:
                raise
            # Another worker uploaded this batch first. Adopt its file
            # when it matches the rebuilt batch, else stop unrecorded.
            raced = store.read(path)
            if sha256(raced.data) != digest:
                raise RecipeError(
                    "orphan_mismatch",
                    f"{path} exists but does not match the rebuilt batch. "
                    "Stop and inspect it; nothing was recorded.",
                ) from exc
            adopted = True
        back = store.read(path)
        if sha256(back.data) != digest:
            raise RecipeError(
                "checksum_mismatch",
                f"{path} read back with a different checksum. "
                "The batch was not recorded in the manifest.",
            )
        if crash_after_upload and uploaded:
            raise SimulatedCrash(f"simulated crash after uploading {path}")
    manifest["batches"] = sorted(
        [
            *manifest["batches"],
            {"index": index, "path": path, "sha256": digest, "rows": ROWS_PER_BATCH},
        ],
        key=lambda b: b["index"],
    )
    _write_manifest(store, run, manifest, etag)
    verb = "adopted" if adopted else "committed"
    line = (
        f"{verb} {path} sha256={digest} rows={ROWS_PER_BATCH} "
        f"batches={len(manifest['batches'])}/{BATCHES}"
    )
    return [line]


def check_batches(store: Store, run: str, work_dir: Path) -> list[str]:
    """Download the committed batches in a fresh process and check them."""
    import polars as pl

    manifest, _ = _read_manifest(store, run)
    work_dir.mkdir(parents=True, exist_ok=True)
    ids: list[int] = []
    for batch in sorted(manifest["batches"], key=lambda b: b["index"]):
        data = store.read(batch["path"]).data
        if sha256(data) != batch["sha256"]:
            raise RecipeError("checksum_mismatch", f"{batch['path']} checksum changed.")
        local = work_dir / Path(batch["path"]).name
        local.write_bytes(data)
        frame = pl.read_parquet(local)
        if frame.columns != COLUMNS:
            raise RecipeError("bad_schema", f"Unexpected columns: {frame.columns}")
        if frame.height != batch["rows"]:
            raise RecipeError("bad_rows", f"{batch['path']} has {frame.height} rows.")
        ids.extend(int(v) for v in frame["id"].to_list())
    if len(ids) != len(set(ids)):
        raise RecipeError("duplicate_rows", "Committed batches share a row id.")
    line = (
        f"match run={run} batches={len(manifest['batches'])} rows={len(ids)} "
        f"unique_ids={len(set(ids))}"
    )
    return [line]


def cleanup_batches(store: Store, run: str) -> list[str]:
    from mountable_sdk import MountableError

    paths = [manifest_path(run)] + [batch_path(run, i) for i in range(BATCHES)]
    deleted = []
    for path in paths:
        try:
            store.delete(path)
            deleted.append(path)
        except MountableError as exc:
            if exc.status != 404:
                raise
    # Remove the run directory and its parent when they are now empty.
    # A non-empty parent belongs to other runs and is left alone.
    for directory in (f"batches/{run}", "batches"):
        with suppress(MountableError):
            store.delete(directory)
            deleted.append(directory)
    return [f"deleted {p}" for p in deleted] + [f"cleanup run={run} done"]


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
    for name in ("run", "check", "cleanup"):
        p = sub.add_parser(name)
        p.add_argument("--filesystem", required=True)
        p.add_argument("--run", required=True)
        p.add_argument("--dir", default="out")
        p.add_argument("--crash-after-upload", action="store_true")
    args = parser.parse_args(argv)
    _FS_ID = args.filesystem
    try:
        _check_run(args.run)
        if args.command == "run":
            lines = run_next(
                _store("rw"), args.run, crash_after_upload=args.crash_after_upload
            )
        elif args.command == "check":
            lines = check_batches(_store("ro"), args.run, Path(args.dir))
        else:
            lines = cleanup_batches(_store("rw"), args.run)
        for line in lines:
            print(line)
        return 0
    except SimulatedCrash as exc:
        print(f"crashed: {exc}", file=sys.stderr)
        return 3
    except RecipeError as exc:
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
