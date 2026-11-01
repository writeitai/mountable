"""Offline checks: a fake store, real Dagster assets, and two local processes."""

import json
import os
from pathlib import Path

os.environ.setdefault("DAGSTER_TELEMETRY_ENABLED", "false")

import dagster as dg
import pytest

from report import (
    ASSET_KEY,
    RecipeError,
    bind_store,
    live_roundtrip,
    load_report,
    materialize_report,
    open_store,
    overwrite_report,
    put_report,
    render_report,
    report_path,
    rows_payload,
    rows_sha256,
    sha256,
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
    monkeypatch.setenv("DAGSTER_HOME", str(tmp_path / "dagster-home"))
    monkeypatch.setenv("DAGSTER_TELEMETRY_ENABLED", "false")
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    bind_store(None)
    yield
    bind_store(None)


def test_materialize_stores_the_closed_report_and_returns_the_id():
    store = Fake()
    bind_store(store)
    result = materialize_report()
    report_id = result.output_for_node(ASSET_KEY)
    path = report_path(report_id)
    payload = json.loads(store.files[path])
    assert result.success
    assert payload["run_id"] == result.run_id
    assert payload["asset_key"] == ASSET_KEY
    assert payload["row_count"] == 2
    assert payload["rows_sha256"] == rows_sha256(rows_payload())
    assert payload["rows"] == rows_payload()
    assert store.writes == [True]
    events = result.asset_materializations_for_node(ASSET_KEY)
    assert [event.asset_key.to_user_string() for event in events] == [ASSET_KEY]
    assert result.asset_materializations_for_node("synthetic_rows")


def test_materialize_uses_an_ephemeral_instance(monkeypatch):
    created: list[dg.DagsterInstance] = []
    real = dg.DagsterInstance.ephemeral

    def wrapped():
        instance = real()
        created.append(instance)
        return instance

    def forbid_durable_instance(*_args, **_kwargs):
        raise AssertionError("materialize opened a durable Dagster instance")

    monkeypatch.setattr(dg.DagsterInstance, "ephemeral", staticmethod(wrapped))
    monkeypatch.setattr(
        dg.DagsterInstance, "get", staticmethod(forbid_durable_instance)
    )
    bind_store(Fake())
    materialize_report()
    assert len(created) == 1
    assert created[0].is_ephemeral
    assert created[0].telemetry_enabled is False


def test_reader_checks_the_run_asset_rows_and_hash():
    store = Fake()
    bind_store(store)
    result = materialize_report()
    report_id = result.output_for_node(ASSET_KEY)
    line = verify_report(report_id, result.run_id)
    data = render_report(
        report_id=report_id,
        run_id=result.run_id,
        asset_key=ASSET_KEY,
        rows=rows_payload(),
    )
    assert line == (
        f"match {report_path(report_id)} run_id={result.run_id} "
        f"asset_key={ASSET_KEY} row_count=2 rows_sha256={rows_sha256(rows_payload())} "
        f"sha256={sha256(data)} bytes={len(data)}"
    )
    with pytest.raises(RecipeError) as denied:
        put_report(open_store("ro"), report_id, b'{"replaced":true}\n')
    assert denied.value.code == "read_only"
    assert store.files[report_path(report_id)] == data


def test_failed_upload_fails_the_asset_and_stores_nothing():
    store = Fake(fail=True)
    bind_store(store)
    with pytest.raises(Exception, match="storage unavailable"):
        materialize_report()
    assert store.files == {}


def test_create_only_conflict_keeps_the_first_bytes():
    store = Fake()
    bind_store(store)
    result = materialize_report()
    report_id = result.output_for_node(ASSET_KEY)
    path = report_path(report_id)
    original = store.files[path]
    with pytest.raises(RecipeError) as conflict:
        overwrite_report(report_id)
    assert conflict.value.code == "exists"
    assert store.files[path] == original
    assert verify_report(report_id, result.run_id).startswith("match ")


def test_report_path_rejects_a_filesystem_id():
    with pytest.raises(RecipeError):
        report_path("fs_123")
    with pytest.raises(RecipeError):
        load_report(Fake(), "../alice/" + "ab" * 8)


def test_two_processes_match_then_preserve_and_delete(tmp_path, monkeypatch):
    monkeypatch.setenv("REPORT_STORE", "dir")
    monkeypatch.setenv("REPORT_DIR", str(tmp_path))
    monkeypatch.setenv("DAGSTER_HOME", str(tmp_path / "dagster-live"))
    assert live_roundtrip() == 0
    assert list(Path(tmp_path).rglob("*.json")) == []
