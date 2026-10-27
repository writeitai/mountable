"""Offline checks: fake filesystem, real pytest subprocess, no network."""

from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from reports import (
    RecipeError,
    check_reports,
    cleanup_reports,
    failure_note,
    parse_junit,
    report_paths,
    run_suite,
    upload_reports,
)

JUNIT = (
    b'<?xml version="1.0"?><testsuites><testsuite tests="2" failures="1" '
    b'errors="0"><testcase classname="t" name="a"/>'
    b'<testcase classname="t" name="b"><failure>x</failure></testcase>'
    b"</testsuite></testsuites>"
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


def test_upload_check_cleanup_roundtrip(tmp_path):
    store = Fake()
    lines = upload_reports(store, "run1", JUNIT, failure_note(JUNIT, 1))
    assert "pytest/run1/junit.xml" in lines["junit"]
    (tmp_path / "junit.xml").write_bytes(JUNIT)
    (tmp_path / "failure.txt").write_bytes(failure_note(JUNIT, 1))
    out = check_reports(store, "run1", tmp_path)
    assert out[-1] == "junit tests=2 failures_and_errors=1"
    cleanup_reports(store, "run1")
    assert store.files == {}
    cleanup_reports(store, "run1")  # already gone stays gone


def test_second_upload_refused_and_mismatch_detected(tmp_path):
    store = Fake()
    upload_reports(store, "run1", JUNIT, b"note")
    with pytest.raises(MountableError):
        upload_reports(store, "run1", JUNIT, b"note")
    (tmp_path / "junit.xml").write_bytes(b"different")
    (tmp_path / "failure.txt").write_bytes(b"note")
    with pytest.raises(RecipeError, match="do not match"):
        check_reports(store, "run1", tmp_path)


def test_parse_junit_rejects_garbage():
    with pytest.raises(RecipeError):
        parse_junit(b"not xml")
    assert parse_junit(JUNIT) == (2, 1)


@pytest.mark.parametrize("run", ["../x", "/x", "Run1", "", "a" * 41])
def test_bad_run_ids_rejected(run):
    with pytest.raises(RecipeError):
        report_paths(run)


def test_real_suite_fails_and_reports_parse(tmp_path):
    code, junit, note = run_suite(tmp_path)
    assert code == 1  # the synthetic failing test fails the process
    assert parse_junit(junit) == (2, 1)
    assert b"pytest exit code: 1" in note
    assert (tmp_path / "failure.txt").read_bytes() == note
