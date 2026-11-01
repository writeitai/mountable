"""Offline checks: a fake store, a real one-shard save, and two local processes."""

import json
from pathlib import Path

import pytest
from datasets import load_from_disk

from dataset import (
    FEATURES_LABEL,
    MARKER,
    REQUIRED,
    USER,
    RecipeError,
    SdkStore,
    bind_store,
    build_features,
    delete_dataset,
    live_roundtrip,
    open_store,
    overwrite_dataset,
    piece_path,
    publish_dataset,
    put_file,
    reconstruct,
    rows_payload,
    rows_sha256,
    save_closed,
    verify_dataset,
)


class Fake:
    def __init__(self, fail_after: int | None = None) -> None:
        self.files: dict[str, bytes] = {}
        self.fail_after = fail_after
        self.writes: list[str] = []

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        if self.fail_after is not None and len(self.writes) >= self.fail_after:
            raise RuntimeError("storage unavailable")
        self.writes.append(path)
        if if_none_match and path in self.files:
            raise RecipeError("exists", "The file already exists.")
        self.files[path] = data
        return path

    def read(self, path: str):
        if path not in self.files:
            raise FileNotFoundError(path)
        return type("File", (), {"data": self.files[path]})()

    def delete(self, path: str) -> None:
        self.files.pop(path, None)


@pytest.fixture(autouse=True)
def isolated(monkeypatch):
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    monkeypatch.delenv("MOUNTABLE_FILESYSTEM_ID", raising=False)
    bind_store(None)
    yield
    bind_store(None)


def test_save_to_disk_writes_the_known_files_and_reloads(tmp_path):
    files = save_closed(tmp_path / "saved")
    assert set(files) == set(REQUIRED)
    loaded = load_from_disk(tmp_path / "saved")
    assert loaded.to_list() == rows_payload()
    assert loaded.features == build_features()
    assert len(loaded) == 2


def test_reader_checks_rows_features_and_hashes():
    store = Fake()
    bind_store(store)
    dataset_id = publish_dataset("ab" * 8)
    line = verify_dataset(dataset_id)
    digest = rows_sha256(rows_payload())
    assert line == (
        f"match datasets/{USER}/{dataset_id} row_count=2 "
        f"features={FEATURES_LABEL} rows_sha256={digest} files={len(REQUIRED)}"
    )
    marker = json.loads(store.files[piece_path(dataset_id, MARKER)])
    assert [item["name"] for item in marker["files"]] == list(REQUIRED)
    assert store.writes[-1].endswith(f"/{MARKER}")
    assert store.writes[:-1] == [piece_path(dataset_id, name) for name in REQUIRED]
    with pytest.raises(RecipeError) as denied:
        put_file(open_store("ro"), piece_path(dataset_id, REQUIRED[0]), b"replaced")
    assert denied.value.code == "read_only"
    assert store.files[piece_path(dataset_id, REQUIRED[0])] != b"replaced"


def test_failed_upload_leaves_earlier_files_and_no_completion_record():
    store = Fake(fail_after=1)
    bind_store(store)
    with pytest.raises(RuntimeError, match="storage unavailable"):
        publish_dataset("cd" * 8)
    assert MARKER not in {Path(path).name for path in store.files}
    assert len(store.files) == 1
    with pytest.raises(RecipeError) as missing:
        reconstruct("cd" * 8, Path("/tmp/unused-hf-reload"))
    assert missing.value.code == "incomplete"


def test_corrupt_file_stops_before_load_from_disk(monkeypatch, tmp_path):
    store = Fake()
    bind_store(store)
    dataset_id = publish_dataset("ef" * 8)
    arrow = piece_path(dataset_id, REQUIRED[0])
    store.files[arrow] = store.files[arrow][:-1] + bytes(
        [store.files[arrow][-1] ^ 0x01]
    )

    def forbid_load(*_args, **_kwargs):
        raise AssertionError("load_from_disk was called")

    monkeypatch.setattr("dataset.load_from_disk", forbid_load)
    with pytest.raises(RecipeError) as corrupt:
        reconstruct(dataset_id, tmp_path)
    assert corrupt.value.code == "corrupt"


def test_missing_required_file_is_refused(tmp_path):
    store = Fake()
    bind_store(store)
    dataset_id = publish_dataset("12" * 8)
    del store.files[piece_path(dataset_id, REQUIRED[1])]
    with pytest.raises(RecipeError) as missing:
        reconstruct(dataset_id, tmp_path)
    assert missing.value.code == "missing"


def test_create_only_keeps_the_first_bytes():
    store = Fake()
    bind_store(store)
    dataset_id = publish_dataset("34" * 8)
    original = dict(store.files)
    with pytest.raises(RecipeError) as conflict:
        overwrite_dataset(dataset_id)
    assert conflict.value.code == "exists"
    assert (
        store.files[piece_path(dataset_id, REQUIRED[0])]
        == original[piece_path(dataset_id, REQUIRED[0])]
    )
    assert verify_dataset(dataset_id).startswith("match ")


def test_bad_id_is_rejected():
    with pytest.raises(RecipeError) as bad:
        piece_path("fs_123", REQUIRED[0])
    assert bad.value.code == "bad_id"
    with pytest.raises(RecipeError):
        piece_path("../alice/" + "ab" * 8, MARKER)


def test_extra_local_file_refuses_the_upload(monkeypatch, tmp_path):
    def extra(self, path, num_shards=1):
        directory = Path(path)
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "notes.txt").write_bytes(b"extra")
        for name in REQUIRED:
            (directory / name).write_bytes(b"closed")

    monkeypatch.setattr("dataset.Dataset.save_to_disk", extra)
    with pytest.raises(RecipeError) as refused:
        save_closed(tmp_path / "extra")
    assert refused.value.code == "unexpected_files"


def test_cleanup_removes_empty_sdk_parents():
    class Recording(Fake):
        def __init__(self) -> None:
            super().__init__()
            self.deleted: list[str] = []

        def delete(self, path: str) -> None:
            self.deleted.append(path)
            super().delete(path)

    recorded = Recording()
    bind_store(SdkStore(recorded))
    dataset_id = publish_dataset("78" * 8)
    delete_dataset(dataset_id)
    assert piece_path(dataset_id, MARKER) not in recorded.files
    assert recorded.deleted[-3:] == [
        f"datasets/{USER}/{dataset_id}",
        f"datasets/{USER}",
        "datasets",
    ]


def test_two_processes_match_then_preserve_and_delete(tmp_path, monkeypatch):
    monkeypatch.setenv("DATASET_STORE", "dir")
    monkeypatch.setenv("DATASET_DIR", str(tmp_path))
    assert live_roundtrip() == 0
    assert [path for path in Path(tmp_path).rglob("*") if path.is_file()] == []
