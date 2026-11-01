"""Real pinned Haystack and SDK, with an offline fake HTTPS file service."""

import io
import json
import subprocess
import sys
from contextlib import contextmanager
from pathlib import Path

import httpx
import pytest
from haystack import Pipeline
from haystack.core.errors import DeserializationError
from mountable_sdk import Mountable, MountableError

import handoff


@contextmanager
def store():
    files, requests, modes = {}, [], {}

    def serve(request):
        requests.append(request)
        if request.url.path == "/api/v1/mount-sessions":
            body = json.loads(request.content)
            assert body["filesystem_id"] == "fixture"
            token = "mtblst_fake_" + str(len(modes))
            modes[token] = body["mode"]
            return httpx.Response(
                201, json={"token": token, "expires_at": "2099-01-01T00:00:00Z"}
            )
        path = request.url.path.split("/files/", 1)[-1]
        if request.method in ("PUT", "DELETE"):
            token = request.headers["authorization"].removeprefix("Bearer ")
            if modes[token] == "ro":
                return httpx.Response(403, json={"detail": {"code": "read_only"}})
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
                    "etag": handoff.digest(request.content),
                    "modified_at": "2026-10-27T00:00:00Z",
                },
            )
        if request.method == "DELETE":
            if path == "haystack":
                assert not any(p.startswith("haystack/") for p in files)
            else:
                assert (
                    request.headers["If-Match"]
                    == '"' + handoff.digest(files[path]) + '"'
                )
                del files[path]
            return httpx.Response(204)
        if path not in files:
            return httpx.Response(404, json={"detail": {"code": "not_found"}})
        return httpx.Response(
            200,
            content=files[path],
            headers={
                "ETag": '"' + handoff.digest(files[path]) + '"',
                "Last-Modified": "Tue, 27 Oct 2026 00:00:00 GMT",
            },
        )

    with Mountable(api_key="mtbl_fixture", base_url="https://files.example") as client:
        client._http.close()
        client._http = httpx.Client(transport=httpx.MockTransport(serve))
        yield client, files, requests


def test_real_dump_upload_load_run_and_etag_cleanup():
    with store() as (client, files, requests):
        fs = client.filesystem("fixture")
        sha, result = handoff.publish(fs, "one")
        assert result == handoff.EXPECTED
        assert list(files) == ["haystack/one.yaml"]
        assert (
            handoff.restore(client.filesystem("fixture", mode="ro"), "one", sha)
            == result
        )
        sessions = [json.loads(r.content) for r in requests if r.method == "POST"]
        assert [s["mode"] for s in sessions] == ["rw", "ro"]
        handoff.cleanup(fs, "one", sha)
        assert files == {}
        with pytest.raises(MountableError) as absent:
            fs.read("haystack/one.yaml")
        assert absent.value.status == 404


def test_repeated_create_and_readonly_write_preserve_original():
    with store() as (client, files, _):
        fs = client.filesystem("fixture")
        handoff.publish(fs, "one")
        original = files.copy()
        with pytest.raises(MountableError) as duplicate:
            handoff.publish(fs, "one")
        assert duplicate.value.status == 412
        with pytest.raises(MountableError) as readonly:
            handoff.publish(client.filesystem("fixture", mode="ro"), "two")
        assert readonly.value.status == 403
        assert readonly.value.code == "read_only"
        assert files == original


@pytest.mark.parametrize("bad", [b"broken: [", b"", b"x" * (handoff.MAX_BYTES + 1)])
def test_changed_corrupt_or_oversize_bytes_refused_before_load(bad, monkeypatch):
    with store() as (client, files, _):
        fs = client.filesystem("fixture")
        sha, _ = handoff.publish(fs, "one")
        files["haystack/one.yaml"] = bad
        monkeypatch.setattr(
            Pipeline, "load", lambda *a, **k: pytest.fail("must not load")
        )
        with pytest.raises(ValueError, match="checksum or size"):
            handoff.restore(fs, "one", sha)


def test_matching_hash_does_not_authorize_other_executable_config(monkeypatch):
    with store() as (client, files, _):
        fs = client.filesystem("fixture")
        handoff.publish(fs, "one")
        bad = b"components:\n  run:\n    type: subprocess.Popen\n"
        files["haystack/one.yaml"] = bad
        monkeypatch.setattr(
            Pipeline, "load", lambda *a, **k: pytest.fail("must not load")
        )
        with pytest.raises(ValueError, match="known pipeline"):
            handoff.restore(fs, "one", handoff.digest(bad))


def test_haystack_default_security_rejects_nontrusted_module():
    config = (
        handoff.known_yaml()
        .decode()
        .replace(
            "haystack.components.preprocessors.document_cleaner.DocumentCleaner",
            "subprocess.Popen",
        )
    )
    with pytest.raises(DeserializationError):
        Pipeline.load(io.StringIO(config))


def test_allowlist_extension_refused(monkeypatch):
    monkeypatch.setenv("HAYSTACK_DESERIALIZATION_ALLOWLIST", "custom.*")
    with pytest.raises(ValueError, match="default trusted-module"):
        handoff.validate(handoff.known_yaml(), handoff.digest(handoff.known_yaml()))


def test_wrong_cleanup_hash_preserves_file_and_makes_no_delete():
    with store() as (client, files, requests):
        fs = client.filesystem("fixture")
        handoff.publish(fs, "one")
        original = files.copy()
        with pytest.raises(ValueError):
            handoff.cleanup(fs, "one", "0" * 64)
        assert original == files
        assert not any(r.method == "DELETE" for r in requests)


@pytest.mark.parametrize(
    "run", ["../outside", "/absolute", "nested/id", "", "A", "a" * 41]
)
def test_invalid_run_makes_no_request(run):
    with store() as (client, _, requests):
        with pytest.raises(ValueError):
            handoff.publish(client.filesystem("fixture"), run)
        assert requests == []


def test_yaml_rebuild_and_load_in_another_interpreter():
    code = """
import handoff
from haystack import Pipeline
raw = bytes.fromhex(input())
handoff.validate(raw, handoff.digest(raw))
print(handoff.run_pipeline(Pipeline.loads(raw)))
"""
    result = subprocess.run(
        [sys.executable, "-c", code],
        cwd=Path(__file__).parent,
        input=handoff.known_yaml().hex(),
        text=True,
        capture_output=True,
        check=True,
    )
    assert result.stdout.strip() == repr(handoff.EXPECTED)
