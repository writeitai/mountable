"""Offline checks: a fake store, a real DAG test, and a fresh process."""

import json
from pathlib import Path

import pytest

from report import (
    PUBLISH_DAG_ID,
    ReadOnly,
    RecipeError,
    live_roundtrip,
    load_report,
    put_report,
    render_report,
    report_path,
    run_dag,
    xcom_values,
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
    home = tmp_path / "airflow"
    monkeypatch.setenv("AIRFLOW_HOME", str(home))
    monkeypatch.setenv("AIRFLOW__CORE__LOAD_EXAMPLES", "false")
    monkeypatch.setenv(
        "AIRFLOW__CORE__DAGS_FOLDER", str(Path(__file__).parent / "dags")
    )
    monkeypatch.setenv("AIRFLOW__LOGGING__LOGGING_LEVEL", "ERROR")
    monkeypatch.setenv("REPORT_STORE", "dir")
    monkeypatch.setenv("REPORT_DIR", str(tmp_path / "reports"))
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    monkeypatch.delenv("MOUNTABLE_FILESYSTEM_ID", raising=False)


def test_create_only_keeps_the_first_bytes():
    store = Fake()
    report_id = "ab" * 8
    put_report(store, report_id, render_report(report_id))
    with pytest.raises(RecipeError) as conflict:
        put_report(store, report_id, b"replaced\n")
    assert conflict.value.code == "exists"
    assert store.files[report_path(report_id)] == render_report(report_id)
    assert store.writes == [True, True]


def test_failed_upload_stores_nothing():
    store = Fake(fail=True)
    with pytest.raises(RuntimeError, match="storage unavailable"):
        put_report(store, "cd" * 8, b"report\n")
    assert store.files == {}


def test_report_path_rejects_a_filesystem_id():
    with pytest.raises(RecipeError):
        report_path("fs_123")
    with pytest.raises(RecipeError):
        load_report(Fake(), "../alice/" + "ab" * 8)


def test_read_only_handle_cannot_write():
    store = Fake()
    report_id = "ef" * 8
    put_report(store, report_id, render_report(report_id))
    with pytest.raises(RecipeError) as denied:
        put_report(ReadOnly(store), report_id, b"nope\n")
    assert denied.value.code == "read_only"
    assert store.files[report_path(report_id)] == render_report(report_id)


def test_dag_passes_only_the_id_and_a_fresh_process_matches(tmp_path):
    assert live_roundtrip() == 0
    values = [json.loads(value) for value in xcom_values(PUBLISH_DAG_ID)]
    assert values
    assert all(isinstance(value, str) and len(value) == 16 for value in values)
    raw = "\n".join(xcom_values(PUBLISH_DAG_ID))
    assert "alpha,1" not in raw
    assert "item,quantity" not in raw
    assert list(Path(tmp_path).rglob("*.txt")) == []


def test_failed_upload_fails_the_dag(monkeypatch):
    monkeypatch.setenv("REPORT_STORE", "fail")
    result = run_dag(PUBLISH_DAG_ID)
    assert result.returncode != 0
    assert "storage unavailable" in (result.stdout + result.stderr)
    assert not any(line.startswith("report_id=") for line in result.stdout.splitlines())
    assert xcom_values(PUBLISH_DAG_ID) == []
