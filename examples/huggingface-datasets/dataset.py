"""Save a Hugging Face dataset so a fresh process can load it.

``write`` builds a two-row ``Dataset``, calls ``save_to_disk`` with one
shard, and uploads that closed file set. It writes a completion record
only after every required file is stored and reads back. ``read`` is a
second process. It receives the dataset id, checks those files, and only
then calls ``load_from_disk``.

The filesystem id and the API key stay in the environment. The dataset id
is chosen here. The user is the fixed fixture ``alice``. This is not a Hub
upload, a zip archive, or a remote filesystem adapter.
"""

import argparse
import hashlib
import json
import os
import re
import secrets
import subprocess
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace
from typing import Protocol

os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("HF_DATASETS_OFFLINE", "1")
os.environ.setdefault("HF_DATASETS_DISABLE_PROGRESS_BARS", "1")

from datasets import Dataset, Features, Value, load_from_disk
from datasets.utils.logging import disable_progress_bar

disable_progress_bar()

USER = "alice"
REQUIRED = (
    "data-00000-of-00001.arrow",
    "dataset_info.json",
    "state.json",
)
MARKER = "complete.json"
ROWS = ({"item": "alpha", "count": 1}, {"item": "beta", "count": 2})
FEATURES_LABEL = "item:string,count:int64"
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


def build_features() -> Features:
    return Features({"item": Value("string"), "count": Value("int64")})


def build_dataset() -> Dataset:
    return Dataset.from_dict(
        {
            "item": [row["item"] for row in ROWS],
            "count": [row["count"] for row in ROWS],
        },
        features=build_features(),
    )


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def dataset_id_ok(dataset_id: str) -> bool:
    return isinstance(dataset_id, str) and _ID.fullmatch(dataset_id) is not None


def piece_path(dataset_id: str, name: str) -> str:
    if not dataset_id_ok(dataset_id):
        raise RecipeError(
            "bad_id",
            "The dataset id must be 16 lowercase hex characters.",
        )
    if name not in REQUIRED and name != MARKER:
        raise RecipeError("bad_name", "That file is not part of this dataset.")
    return f"datasets/{USER}/{dataset_id}/{name}"


def new_dataset_id() -> str:
    return secrets.token_hex(8)


def _mapped(exc: Exception) -> RecipeError | None:
    status = getattr(exc, "status", None)
    code = getattr(exc, "code", None)
    if status == 412 or code == "precondition_failed":
        return RecipeError("exists", "The file already exists.")
    if status == 403 or code in {"read_only", "forbidden"}:
        return RecipeError("read_only", "This handle cannot write.")
    if status == 404 or code == "not_found" or isinstance(exc, FileNotFoundError):
        return RecipeError("not_found", "The file is not there.")
    return None


def put_file(store: Store, path: str, data: bytes) -> None:
    if len(data) > MAX_BYTES:
        raise RecipeError("too_large", "One file is larger than 1 MiB.")
    try:
        store.write(path, data, if_none_match=True)
    except RecipeError:
        raise
    except Exception as exc:
        mapped = _mapped(exc)
        if mapped is not None:
            raise mapped from exc
        raise


def read_file(store: Store, path: str) -> bytes:
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
            raise RecipeError("bad_path", "The path leaves the dataset directory.")
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
            raise RecipeError("exists", "The file already exists.") from exc
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
            if dest.is_dir():
                dest.rmdir()
            else:
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
    kind = os.environ.get("DATASET_STORE", "dir")
    if kind == "dir":
        root = Path(os.environ.get("DATASET_DIR", "data"))
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
    raise RecipeError("bad_store", "DATASET_STORE must be dir or mountable.")


def save_closed(directory: Path) -> dict[str, bytes]:
    """Call ``save_to_disk`` and return only the known closed files."""
    directory.mkdir(parents=True, exist_ok=True)
    build_dataset().save_to_disk(os.fspath(directory), num_shards=1)
    entries = list(directory.iterdir())
    if any(entry.is_dir() for entry in entries):
        raise RecipeError(
            "unexpected_files",
            "save_to_disk wrote a directory this recipe does not upload.",
        )
    found = {entry.name for entry in entries}
    if found != set(REQUIRED):
        raise RecipeError(
            "unexpected_files",
            "save_to_disk did not write the known one-shard file set.",
        )
    files = {name: (directory / name).read_bytes() for name in REQUIRED}
    if sum(len(data) for data in files.values()) > MAX_BYTES:
        raise RecipeError("too_large", "The dataset is larger than 1 MiB.")
    return files


def marker_bytes(dataset_id: str, files: dict[str, bytes]) -> bytes:
    payload = {
        "dataset_id": dataset_id,
        "features": FEATURES_LABEL,
        "files": [
            {"bytes": len(files[name]), "name": name, "sha256": sha256(files[name])}
            for name in REQUIRED
        ],
        "row_count": len(ROWS),
        "rows_sha256": rows_sha256(rows_payload()),
        "user": USER,
    }
    return (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode()


def publish_dataset(dataset_id: str | None = None) -> str:
    """Upload the closed files, then the completion record."""
    dataset_id = dataset_id or new_dataset_id()
    with tempfile.TemporaryDirectory(prefix="hf-dataset-") as tmp:
        files = save_closed(Path(tmp))
    store = open_store("rw")
    for name in REQUIRED:
        put_file(store, piece_path(dataset_id, name), files[name])
    for name in REQUIRED:
        stored = read_file(store, piece_path(dataset_id, name))
        if stored != files[name]:
            raise RecipeError("mismatch", "A stored file is not the closed file.")
    record = marker_bytes(dataset_id, files)
    put_file(store, piece_path(dataset_id, MARKER), record)
    if read_file(store, piece_path(dataset_id, MARKER)) != record:
        raise RecipeError("mismatch", "The completion record did not read back.")
    return dataset_id


def published_lines(dataset_id: str) -> str:
    store = open_store("ro")
    record = json.loads(read_file(store, piece_path(dataset_id, MARKER)))
    total = sum(item["bytes"] for item in record["files"])
    return "\n".join(
        [
            f"dataset_id={dataset_id}",
            f"row_count={record['row_count']}",
            f"features={record['features']}",
            f"rows_sha256={record['rows_sha256']}",
            f"files={len(record['files'])}",
            f"bytes={total}",
        ]
    )


def _load_marker(store: Store, dataset_id: str) -> dict[str, object]:
    path = piece_path(dataset_id, MARKER)
    try:
        raw = read_file(store, path)
    except RecipeError as exc:
        if exc.code == "not_found":
            raise RecipeError(
                "incomplete",
                "The dataset has no completion record.",
            ) from exc
        raise
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RecipeError("corrupt", "The completion record is not JSON.") from exc
    names = [item.get("name") for item in payload.get("files", [])]
    if payload.get("dataset_id") != dataset_id or names != list(REQUIRED):
        raise RecipeError("corrupt", "The completion record does not match.")
    return payload


def reconstruct(dataset_id: str, directory: Path) -> Dataset:
    """Copy the known files locally after their hashes match. Then load."""
    store = open_store("ro")
    payload = _load_marker(store, dataset_id)
    directory.mkdir(parents=True, exist_ok=True)
    for item in payload["files"]:
        name = str(item["name"])
        try:
            data = read_file(store, piece_path(dataset_id, name))
        except RecipeError as exc:
            if exc.code == "not_found":
                raise RecipeError("missing", f"{name} is not stored.") from exc
            raise
        if sha256(data) != item["sha256"] or len(data) != item["bytes"]:
            raise RecipeError("corrupt", f"{name} does not match the record.")
        (directory / name).write_bytes(data)
    loaded = load_from_disk(os.fspath(directory))
    rows = rows_payload()
    if loaded.to_list() != rows or loaded.features != build_features():
        raise RecipeError("mismatch", "The loaded dataset is not the saved one.")
    if payload["rows_sha256"] != rows_sha256(rows) or payload["row_count"] != len(rows):
        raise RecipeError("mismatch", "The completion record disagrees with the rows.")
    if payload["features"] != FEATURES_LABEL:
        raise RecipeError("mismatch", "The recorded features changed.")
    return loaded


def verify_dataset(dataset_id: str) -> str:
    with tempfile.TemporaryDirectory(prefix="hf-reload-") as tmp:
        loaded = reconstruct(dataset_id, Path(tmp))
    digest = rows_sha256(rows_payload())
    return (
        f"match datasets/{USER}/{dataset_id} row_count={len(loaded)} "
        f"features={FEATURES_LABEL} rows_sha256={digest} files={len(REQUIRED)}"
    )


def overwrite_dataset(dataset_id: str) -> None:
    """Try to replace the arrow file. Create-only leaves the first bytes."""
    with tempfile.TemporaryDirectory(prefix="hf-overwrite-") as tmp:
        files = save_closed(Path(tmp))
    put_file(
        open_store("rw"),
        piece_path(dataset_id, REQUIRED[0]),
        files[REQUIRED[0]],
    )


def delete_dataset(dataset_id: str) -> None:
    store = open_store("rw")
    for name in (MARKER, *REQUIRED):
        path = piece_path(dataset_id, name)
        try:
            store.delete(path)
        except Exception as exc:
            mapped = _mapped(exc)
            if mapped is None or mapped.code != "not_found":
                raise


def _remove_empty_parents(store: Store, dataset_id: str) -> None:
    if not isinstance(store, SdkStore):
        return
    for path in (
        f"datasets/{USER}/{dataset_id}",
        f"datasets/{USER}",
        "datasets",
    ):
        try:
            store.delete(path)
        except Exception as exc:
            status = getattr(exc, "status", None)
            if status is None or status >= 500:
                raise


def _print_usage(label: str) -> None:
    if os.environ.get("DATASET_STORE") != "mountable":
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
    env["HF_HUB_OFFLINE"] = "1"
    env["HF_DATASETS_OFFLINE"] = "1"
    env["HF_DATASETS_DISABLE_PROGRESS_BARS"] = "1"
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
    dataset_id = _field(wrote.stdout, "dataset_id")
    read = _spawn(["read", "--dataset-id", dataset_id])
    if read.returncode != 0 or not read.stdout.startswith("match "):
        print(read.stderr, file=sys.stderr)
        print("read_failed", file=sys.stderr)
        return 1
    again = _spawn(["overwrite", "--dataset-id", dataset_id])
    if again.returncode == 0:
        print("overwrite_succeeded", file=sys.stderr)
        return 1
    reread = _spawn(["read", "--dataset-id", dataset_id])
    if reread.returncode != 0 or reread.stdout.strip() != read.stdout.strip():
        print("mismatch: the first dataset changed", file=sys.stderr)
        return 1
    denied = _spawn(["read-only-write", "--dataset-id", dataset_id])
    if denied.returncode == 0:
        print("read_only_write_succeeded", file=sys.stderr)
        return 1
    still = _spawn(["read", "--dataset-id", dataset_id])
    if still.returncode != 0 or still.stdout.strip() != read.stdout.strip():
        print("mismatch: a read-only write changed the dataset", file=sys.stderr)
        return 1
    removed = _spawn(["cleanup", "--dataset-id", dataset_id])
    if removed.returncode != 0:
        print(removed.stderr, file=sys.stderr)
        print("cleanup_failed", file=sys.stderr)
        return 1
    _remove_empty_parents(open_store("rw"), dataset_id)
    gone = _spawn(["read", "--dataset-id", dataset_id])
    if gone.returncode == 0 or "incomplete" not in gone.stderr:
        print("absent: the dataset is still there", file=sys.stderr)
        return 1
    _print_usage("after")
    print(read.stdout.strip())
    print("preserved: exists")
    print("read_only: denied")
    print(f"deleted datasets/{USER}/{dataset_id}")
    print("absent: incomplete")
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
    read.add_argument("--dataset-id", required=True)
    for name in ("overwrite", "cleanup", "read-only-write"):
        command = sub.add_parser(name)
        command.add_argument("--dataset-id", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "live":
            return live_roundtrip()
        if args.command == "write":
            dataset_id = publish_dataset()
            print(published_lines(dataset_id))
            return 0
        if args.command == "read":
            print(verify_dataset(args.dataset_id))
            return 0
        if args.command == "overwrite":
            overwrite_dataset(args.dataset_id)
            print(f"dataset_id={args.dataset_id}")
            return 0
        if args.command == "read-only-write":
            put_file(
                open_store("ro"),
                piece_path(args.dataset_id, REQUIRED[0]),
                b"replaced",
            )
            print(f"dataset_id={args.dataset_id}")
            return 0
        delete_dataset(args.dataset_id)
        print(f"deleted datasets/{USER}/{args.dataset_id}")
        return 0
    except Exception as exc:  # noqa: BLE001 - CLI maps storage failures to an exit code
        return _fail(exc)


if __name__ == "__main__":
    raise SystemExit(main())
