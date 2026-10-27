"""Offline checks: a fake store, real Prefect flows, and two local processes."""

import os
from pathlib import Path

os.environ.setdefault("PREFECT_LOGGING_LEVEL", "ERROR")

import pytest

from report import (
    RecipeError,
    bind_store,
    live_roundtrip,
    load_report,
    open_store,
    overwrite_flow,
    publish_flow,
    put_report,
    read_flow,
    render_report,
    report_path,
    sha256,
    upload_report,
    verify_report,
)


class Fake:
    def __init__(self, fail: bool = False) -> None:
        self.files: dict[str, bytes] = {}
        self.fail = fail
        self.writes: list[bool] = []

    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object:
        self.writes.append(if_none_match)
        if self.fail:
            raise RuntimeError("storage unavailable")
        if if_none_match and path in self.files:
            raise RecipeError("exists", "The report already exists.")
        self.files[path] = data
        return path

    def read(self, path: str):
        if path not in self.files:
            raise FileNotFoundError(path)
        return type("File", (), {"data": self.files[path]})()

    def delete(self, path: str) -> None:
        self.files.pop(path, None)


@pytest.fixture(autouse=True)
def isolated(tmp_path, monkeypatch):
    monkeypatch.setenv("PREFECT_HOME", str(tmp_path / "prefect"))
    monkeypatch.setenv("PREFECT_LOGGING_LEVEL", "ERROR")
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    bind_store(None)
    yield
    bind_store(None)


def test_publish_flow_returns_the_id_and_stores_the_bytes():
    store = Fake()
    bind_store(store)
    report_id = publish_flow()
    path = report_path(report_id)
    assert report_id != store.files[path]
    assert store.files == {path: render_report(report_id)}
    assert store.writes == [True]
    assert upload_report.persist_result is False
    assert verify_report.persist_result is False


def test_reader_flow_checks_the_hash_through_a_read_only_handle():
    store = Fake()
    bind_store(store)
    report_id = publish_flow()
    line = read_flow(report_id)
    data = render_report(report_id)
    assert line == (
        f"match {report_path(report_id)} sha256={sha256(data)} bytes={len(data)}"
    )
    with pytest.raises(RecipeError) as denied:
        put_report(open_store("ro"), report_id, b"nope\n")
    assert denied.value.code == "read_only"
    assert store.files[report_path(report_id)] == data


def test_failed_upload_fails_the_task_and_returns_no_id():
    store = Fake(fail=True)
    bind_store(store)
    with pytest.raises(RuntimeError, match="storage unavailable"):
        publish_flow()
    assert store.files == {}


def test_create_only_conflict_keeps_the_first_bytes():
    store = Fake()
    bind_store(store)
    report_id = publish_flow()
    path = report_path(report_id)
    original = store.files[path]
    with pytest.raises(RecipeError) as conflict:
        overwrite_flow(report_id)
    assert conflict.value.code == "exists"
    assert store.files[path] == original
    assert read_flow(report_id).startswith("match ")


def test_report_path_rejects_a_filesystem_id():
    with pytest.raises(RecipeError):
        report_path("fs_123")
    with pytest.raises(RecipeError):
        load_report(Fake(), "../alice/" + "ab" * 8)


def test_two_processes_match_then_preserve_and_delete(tmp_path, monkeypatch):
    monkeypatch.setenv("REPORT_STORE", "dir")
    monkeypatch.setenv("REPORT_DIR", str(tmp_path))
    monkeypatch.setenv("PREFECT_HOME", str(tmp_path / "prefect-live"))
    assert live_roundtrip() == 0
    assert list(Path(tmp_path).rglob("*.txt")) == []
