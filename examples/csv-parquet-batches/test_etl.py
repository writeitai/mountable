"""Offline checks: fake filesystem with ETags, real Polars, no network."""

import json
from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from etl import (
    BATCHES,
    RecipeError,
    SimulatedCrash,
    batch_path,
    check_batches,
    cleanup_batches,
    manifest_path,
    run_next,
)


class Fake:
    """In-memory store with create-only, ETag and read-only semantics."""

    def __init__(self, *, read_only: bool = False):
        self.files: dict[str, tuple[bytes, str]] = {}
        self.read_only = read_only
        self.version = 0

    def write(self, path, data, *, if_none_match=False, if_match=None):
        if self.read_only:
            raise MountableError(403, "forbidden", "read-only")
        if if_none_match and path in self.files:
            raise MountableError(412, "precondition_failed", "exists")
        if if_match is not None:
            current = self.files.get(path, (b"", None))[1]
            if current != if_match:
                raise MountableError(412, "precondition_failed", "etag changed")
        self.version += 1
        etag = f"v{self.version}"
        self.files[path] = (bytes(data), etag)
        return SimpleNamespace(etag=etag)

    def read(self, path):
        if path not in self.files:
            raise MountableError(404, "not_found", "gone")
        data, etag = self.files[path]
        return SimpleNamespace(data=data, info=SimpleNamespace(etag=etag))

    def delete(self, path):
        if self.read_only:
            raise MountableError(403, "forbidden", "read-only")
        if path not in self.files:
            raise MountableError(404, "not_found", "gone")
        del self.files[path]


def test_commit_three_batches_in_fresh_calls(tmp_path):
    store = Fake()
    for _ in range(BATCHES):
        run_next(store, "run1")
    assert run_next(store, "run1") == ["done run=run1 batches=3 rows=60"]
    out = check_batches(store, "run1", tmp_path)
    assert out == ["match run=run1 batches=3 rows=60 unique_ids=60"]


def test_crash_after_upload_is_adopted_without_duplicates(tmp_path):
    store = Fake()
    with pytest.raises(SimulatedCrash):
        run_next(store, "run1", crash_after_upload=True)
    # The upload exists, but no batch is recorded as committed yet.
    manifest = (
        json.loads(store.read(manifest_path("run1")).data)
        if manifest_path("run1") in store.files
        else {"batches": []}
    )
    assert manifest["batches"] == []
    assert check_batches(store, "run1", tmp_path) == [
        "match run=run1 batches=0 rows=0 unique_ids=0"
    ]
    line = run_next(store, "run1")
    assert line[0].startswith("adopted batches/run1/batch-000.parquet")
    run_next(store, "run1")
    run_next(store, "run1")
    assert check_batches(store, "run1", tmp_path) == [
        "match run=run1 batches=3 rows=60 unique_ids=60"
    ]


def test_competing_writer_stops_the_run():
    store = Fake()
    run_next(store, "run1")
    # Another writer replaces the manifest after our read would be stale:
    # simulate by changing the stored ETag behind a wrapped read.
    original_read = store.read
    calls = {"n": 0}

    def racing_read(path):
        got = original_read(path)
        calls["n"] += 1
        if path == manifest_path("run1") and calls["n"] == 1:
            store.write(path, got.data)  # competing write bumps the ETag
            return got  # caller still holds the old ETag
        return got

    store.read = racing_read  # type: ignore[method-assign]
    with pytest.raises(RecipeError) as exc:
        run_next(store, "run1")
    assert exc.value.code == "competing_writer"


def test_changed_upload_is_not_recorded():
    store = Fake()
    store.write(batch_path("run1", 0), b"not the batch", if_none_match=True)
    with pytest.raises(RecipeError) as exc:
        run_next(store, "run1")
    assert exc.value.code == "orphan_mismatch"
    assert manifest_path("run1") not in store.files


def test_raced_upload_is_adopted_not_traced(tmp_path):
    """Two workers race one batch: the loser adopts, never tracebacks."""
    from etl import batch_parquet_bytes

    store = Fake()
    # The winning worker's file is already stored, but our first read
    # still sees 404, so our create-only write loses the race with 412.
    store.write(batch_path("run1", 0), batch_parquet_bytes(0), if_none_match=True)
    original_read = store.read
    seen = {"batch": 0}

    def stale_read(path):
        if path == batch_path("run1", 0) and seen["batch"] == 0:
            seen["batch"] += 1
            raise MountableError(404, "not_found", "gone")
        return original_read(path)

    store.read = stale_read  # type: ignore[method-assign]
    line = run_next(store, "run1")
    assert line[0].startswith("adopted batches/run1/batch-000.parquet")
    assert check_batches(store, "run1", tmp_path) == [
        "match run=run1 batches=1 rows=20 unique_ids=20"
    ]


def test_cleanup_removes_manifest_and_batches(tmp_path):
    store = Fake()
    run_next(store, "run1")
    lines = cleanup_batches(store, "run1")
    assert lines[-1] == "cleanup run=run1 done"
    assert store.files == {}
    cleanup_batches(store, "run1")  # already gone stays gone


def test_read_only_store_refuses_writes():
    store = Fake(read_only=True)
    with pytest.raises(MountableError) as exc:
        run_next(store, "run1")
    assert exc.value.status == 403


@pytest.mark.parametrize("run", ["../x", "/x", "Run1", "", "a" * 41])
def test_bad_run_ids_rejected(run):
    with pytest.raises(RecipeError):
        manifest_path(run)
