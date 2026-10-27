"""Real LlamaIndex and released SDK over fake HTTP. No cloud or model calls."""

import json
import subprocess
import sys
from contextlib import contextmanager

import httpx
import pytest
from mountable_sdk import Mountable, MountableError

import snapshot
from snapshot import EXPECTED, FILES, MAX_BYTES, cleanup, digest, publish, restore


@contextmanager
def store():
    files = {}
    requests = []

    def serve(request):
        requests.append(request)
        if request.url.path == "/api/v1/mount-sessions":
            return httpx.Response(
                201,
                json={
                    "token": "mtblst_fake",
                    "expires_at": "2099-01-01T00:00:00Z",
                },
            )
        path = request.url.path.split("/files/", 1)[-1]
        if request.method == "PUT":
            assert request.headers["If-None-Match"] == "*"
            if path in files:
                return httpx.Response(
                    412, json={"detail": {"code": "precondition_failed"}}
                )
            files[path] = request.content
            return httpx.Response(
                201,
                json={
                    "path": path,
                    "type": "file",
                    "size": len(request.content),
                    "etag": digest(request.content),
                    "modified_at": "2026-10-27T00:00:00Z",
                },
            )
        if request.method == "DELETE":
            if path in files:
                assert request.headers["If-Match"] == '"' + digest(files[path]) + '"'
                del files[path]
            return httpx.Response(204)
        if path not in files:
            return httpx.Response(404, json={"detail": {"code": "not_found"}})
        return httpx.Response(
            200,
            content=files[path],
            headers={
                "ETag": '"' + digest(files[path]) + '"',
                "Last-Modified": "Tue, 27 Oct 2026 00:00:00 GMT",
            },
        )

    with Mountable(
        api_key="mtbl_test_only", base_url="https://files.example"
    ) as client:
        client._http.close()
        client._http = httpx.Client(transport=httpx.MockTransport(serve))
        yield client, files, requests


def test_real_persist_upload_download_load_retrieval_and_cleanup():
    with store() as (client, files, requests):
        sha = publish(client.filesystem("fs_fixture"), "test1")
        assert len(files) == 6
        puts = [r for r in requests if r.method == "PUT"]
        assert puts[-1].url.path.endswith("manifest.json")
        assert (
            restore(client.filesystem("fs_fixture", mode="ro"), "test1", sha)
            == EXPECTED
        )
        sessions = [json.loads(r.content) for r in requests if r.method == "POST"]
        assert sessions[-1]["mode"] == "ro"
        assert sessions[-1]["filesystem_id"] == "fs_fixture"
        cleanup(client.filesystem("fs_fixture"), "test1")
        assert not files


@pytest.mark.parametrize("name", FILES)
@pytest.mark.parametrize("change", ["missing", "corrupt"])
def test_missing_or_corrupt_artifact_is_refused_before_load(name, change, monkeypatch):
    with store() as (client, files, _):
        fs = client.filesystem("fs_fixture")
        sha = publish(fs, "test1")
        if change == "missing":
            del files[f"llamaindex/test1/{name}"]
        else:
            files[f"llamaindex/test1/{name}"] = b"{}"

        def forbidden(*args, **kwargs):
            pytest.fail("LlamaIndex must not load incomplete data")

        monkeypatch.setattr(snapshot, "load_index_from_storage", forbidden)
        with pytest.raises((ValueError, MountableError)):
            restore(fs, "test1", sha)


def test_manifest_digest_is_checked_before_artifacts_are_read():
    with store() as (client, files, requests):
        fs = client.filesystem("fs_fixture")
        sha = publish(fs, "test1")
        files["llamaindex/test1/manifest.json"] += b" "
        requests.clear()
        with pytest.raises(ValueError, match="Manifest checksum"):
            restore(fs, "test1", sha)
        assert len(requests) == 1


@pytest.mark.parametrize("change", ["paths", "oversize", "version"])
def test_manifest_schema_refused_before_artifact_read(change):
    with store() as (client, files, requests):
        fs = client.filesystem("fs_fixture")
        publish(fs, "test1")
        manifest = json.loads(files["llamaindex/test1/manifest.json"])
        if change == "paths":
            manifest["files"]["../outside.json"] = manifest["files"].pop(FILES[0])
        elif change == "oversize":
            manifest["files"][FILES[0]]["size"] = MAX_BYTES + 1
        else:
            manifest["llama_index_core"] = "other"
        raw = json.dumps(manifest).encode()
        files["llamaindex/test1/manifest.json"] = raw
        requests.clear()
        with pytest.raises(ValueError):
            restore(fs, "test1", digest(raw))
        assert len(requests) == 1


def test_repeated_run_cannot_replace_existing_snapshot():
    with store() as (client, files, _):
        fs = client.filesystem("fs_fixture")
        publish(fs, "test1")
        before = files.copy()
        with pytest.raises(MountableError, match="precondition_failed"):
            publish(fs, "test1")
        assert files == before


@pytest.mark.parametrize(
    "run", ["../outside", "/absolute", "nested/id", "", "A", "a" * 41]
)
def test_bad_run_never_sends_network_requests(run):
    with store() as (client, _, requests):
        with pytest.raises(ValueError):
            publish(client.filesystem("fs_fixture"), run)
        assert requests == []


def test_partial_upload_has_no_manifest(monkeypatch):
    with store() as (client, files, _):
        fs = client.filesystem("fs_fixture")
        write = fs.write

        def fail(path, data, **kwargs):
            if path.endswith(FILES[1]):
                raise MountableError(402, "filesystem_spending_cap_reached", "private")
            return write(path, data, **kwargs)

        monkeypatch.setattr(fs, "write", fail)
        with pytest.raises(MountableError):
            publish(fs, "test1")
        assert list(files) == ["llamaindex/test1/" + FILES[0]]
        cleanup(fs, "test1")
        assert not files


def test_invalid_cli_run_is_a_clean_argument_error():
    result = subprocess.run(
        [
            sys.executable,
            "snapshot.py",
            "publish",
            "--filesystem",
            "fixture",
            "--run",
            "../outside",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 2
    assert "run must be" in result.stderr
    assert "Traceback" not in result.stderr
