"""Offline checks: fake filesystem, real Polars and DuckDB, no network."""

from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from handoff import (
    RecipeError,
    check_dataset,
    cleanup_dataset,
    dataset_path,
    expected_totals,
    upload_dataset,
    write_parquet_bytes,
)


class Fake:
    def __init__(self):
        self.files = {}

    def write(self, path, data, *, if_none_match):
        assert if_none_match is True
        if path in self.files:
            raise MountableError(412, "precondition_failed", "exists")
        self.files[path] = data

    def read(self, path):
        return SimpleNamespace(data=self.files[path])

    def delete(self, path):
        if path not in self.files:
            raise MountableError(404, "not_found", "gone")
        del self.files[path]


def test_write_check_cleanup_roundtrip(tmp_path):
    store = Fake()
    line = upload_dataset(store, "run1", write_parquet_bytes())
    assert "parquet/run1/daily.parquet" in line
    out = check_dataset(store, "run1", tmp_path)
    rows, units = expected_totals()
    assert out[-1].startswith(f"parquet rows={rows} units_sum={units}")
    cleanup_dataset(store, "run1")
    assert store.files == {}
    cleanup_dataset(store, "run1")  # already gone stays gone


def test_second_upload_refused():
    store = Fake()
    data = write_parquet_bytes()
    upload_dataset(store, "run1", data)
    with pytest.raises(MountableError):
        upload_dataset(store, "run1", data)


@pytest.mark.parametrize("run", ["../x", "/x", "Run1", "", "a" * 41])
def test_bad_run_ids_rejected(run):
    with pytest.raises(RecipeError):
        dataset_path(run)
