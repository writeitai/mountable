"""Offline checks: TestClient, a fake store, and two local server processes."""

import hashlib

import pytest
from fastapi.testclient import TestClient

from app import (
    RecipeError,
    create_app,
    live_roundtrip,
    publish_report,
    render_report,
    report_path,
)

ALICE = {"Authorization": "Bearer fixture-alice"}
BOB = {"Authorization": "Bearer fixture-bob"}


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
            raise RecipeError("exists", "already there")
        self.files[path] = data
        return path

    def read(self, path: str):
        if path not in self.files:
            raise FileNotFoundError(path)
        return type("File", (), {"data": self.files[path]})()

    def delete(self, path: str) -> None:
        self.files.pop(path, None)


def test_post_returns_id_only_after_the_bytes_match():
    store = Fake()
    client = TestClient(create_app(store))
    created = client.post("/reports", headers=ALICE)
    assert created.status_code == 201
    report_id = created.json()["id"]
    assert report_id not in store.files
    body = client.get(f"/reports/{report_id}", headers=ALICE)
    assert body.status_code == 200
    assert body.content == render_report("alice", report_id)
    assert (
        hashlib.sha256(body.content).hexdigest()
        == hashlib.sha256(render_report("alice", report_id)).hexdigest()
    )
    assert store.files == {f"reports/alice/{report_id}.txt": body.content}
    assert store.writes == [True]


def test_other_fixture_user_cannot_read_the_report():
    client = TestClient(create_app(Fake()))
    report_id = client.post("/reports", headers=ALICE).json()["id"]
    other = client.get(f"/reports/{report_id}", headers=BOB)
    assert other.status_code == 404
    assert other.json() == {"detail": "not_found"}
    assert "id" not in other.json()
    own = client.get(f"/reports/{report_id}", headers=ALICE)
    assert own.status_code == 200


def test_upload_failure_returns_no_report_id():
    client = TestClient(create_app(Fake(fail=True)))
    failed = client.post(
        "/reports",
        headers=ALICE,
        json={"filesystem_id": "fs_from_the_caller", "path": "/etc/passwd"},
    )
    assert failed.status_code == 503
    assert failed.json() == {"detail": "upload_failed"}
    assert "id" not in failed.json()


def test_missing_and_unknown_tokens_are_refused():
    client = TestClient(create_app(Fake()))
    assert client.post("/reports").status_code == 401
    assert (
        client.post(
            "/reports", headers={"Authorization": "Bearer someone-else"}
        ).status_code
        == 401
    )


@pytest.mark.parametrize(
    "report_id",
    ["../bob/" + "a" * 16, "a" * 15, "A" * 16, "reports/alice/" + "ab" * 8],
)
def test_caller_supplied_paths_are_not_read(report_id):
    store = Fake()
    store.files["reports/alice/" + "ab" * 8 + ".txt"] = b"secret"
    client = TestClient(create_app(store))
    response = client.get("/reports/" + report_id, headers=BOB)
    assert response.status_code == 404
    assert store.files["reports/alice/" + "ab" * 8 + ".txt"] == b"secret"


def test_report_path_rejects_a_filesystem_id_and_a_path():
    with pytest.raises(RecipeError):
        report_path("alice", "fs_123")
    with pytest.raises(RecipeError):
        report_path("mallory", "ab" * 8)


def test_publish_report_uses_the_backend_path():
    store = Fake()
    report_id = publish_report(store, "alice")
    assert list(store.files) == [f"reports/alice/{report_id}.txt"]


def test_two_server_processes_return_the_same_hash(tmp_path, monkeypatch):
    monkeypatch.setenv("REPORT_STORE", "dir")
    monkeypatch.setenv("REPORT_DIR", str(tmp_path))
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    assert live_roundtrip() == 0
    assert list(tmp_path.rglob("*.txt")) == []
