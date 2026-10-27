"""Real local Git and released SDK over fake HTTP; no cloud calls."""

import json
from contextlib import contextmanager

import httpx
import pytest
from mountable_sdk import Mountable, MountableError

from handoff import (
    MAX_BYTES,
    checked_read,
    cleanup,
    digest,
    git_demo,
    remote_path,
    upload,
    validate,
)


@contextmanager
def store():
    files = {}
    modes = {}
    requests = []
    fail_put = None

    def serve(request):
        requests.append(request)
        if request.url.path == "/api/v1/mount-sessions":
            token = f"mtblst_fake{len(modes)}"
            modes[token] = json.loads(request.content)["mode"]
            return httpx.Response(
                201, json={"token": token, "expires_at": "2099-01-01T00:00:00Z"}
            )
        path = request.url.path.split("/files/", 1)[-1]
        mode = modes[request.headers["Authorization"].removeprefix("Bearer ")]
        if request.method != "GET" and mode == "ro":
            return httpx.Response(403, json={"detail": {"code": "read_only"}})
        if request.method == "PUT":
            assert request.headers["If-None-Match"] == "*"
            if fail_put:
                return httpx.Response(
                    fail_put, json={"detail": {"code": "fixture_failure"}}
                )
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
        if path not in files:
            return httpx.Response(404, json={"detail": {"code": "not_found"}})
        if request.method == "DELETE":
            assert request.headers["If-Match"] == '"' + digest(files[path]) + '"'
            del files[path]
            return httpx.Response(204)
        return httpx.Response(
            200,
            content=files[path],
            headers={
                "ETag": '"' + digest(files[path]) + '"',
                "Last-Modified": "Tue, 27 Oct 2026 00:00:00 GMT",
            },
        )

    def set_failure(status):
        nonlocal fail_put
        fail_put = status

    with Mountable(api_key="mtbl_fixture", base_url="https://files.example") as client:
        client._http.close()
        client._http = httpx.Client(transport=httpx.MockTransport(serve))
        yield client, files, requests, set_failure


def report():
    return (
        json.dumps(
            {"source_commit": "a" * 40, "check": "total([3, -2, 4]) == 7", "result": 7},
            sort_keys=True,
        )
        + "\n"
    ).encode()


def test_real_git_diff_commits_and_report(tmp_path, capsys):
    target = tmp_path / "report.json"
    git_demo(target)
    data = target.read_bytes()
    body = validate(data)
    assert len(body["source_commit"]) == 40
    output = capsys.readouterr().out
    assert "-    return sum(values)" in output
    assert "+    return sum(value for value in values if value >= 0)" in output
    assert "Ignore negative values\nAdd total\n" in output
    with pytest.raises(FileExistsError):
        git_demo(target)
    assert target.read_bytes() == data


def test_sdk_handoff_repeat_refused_read_only_and_cleanup():
    data = report()
    with store() as (client, files, requests, _):
        writer = client.filesystem("fixture")
        upload(writer, "test1", data)
        with pytest.raises(MountableError) as error:
            upload(writer, "test1", data.replace(b'"result": 7', b'"result": 7 '))
        assert error.value.status == 412
        reader = client.filesystem("fixture", "ro")
        assert checked_read(reader, "test1", digest(data)).data == data
        with pytest.raises(MountableError) as error:
            upload(reader, "test1", data)
        assert error.value.status == 403
        cleanup(writer, "test1", digest(data))
        assert files == {}
        with pytest.raises(MountableError) as error:
            checked_read(reader, "test1", digest(data))
        assert error.value.status == 404
        assert json.loads(requests[3].content)["mode"] == "ro"


def test_changed_bytes_are_not_deleted():
    data = report()
    with store() as (client, files, _, _):
        writer = client.filesystem("fixture")
        upload(writer, "test1", data)
        files[remote_path("test1")] = data + b" "
        with pytest.raises(ValueError, match="checksum"):
            cleanup(writer, "test1", digest(data))
        assert files


@pytest.mark.parametrize("run", ["../other", "/absolute", "", "a" * 41])
def test_bad_paths_are_rejected(run):
    with pytest.raises(ValueError):
        remote_path(run)


@pytest.mark.parametrize(
    "data",
    [
        b"x" * (MAX_BYTES + 1),
        b"{}",
        b"[]",
        report().replace(b'"result": 7', b'"result": true'),
    ],
)
def test_bad_report_is_rejected_before_upload(data):
    with store() as (client, files, requests, _):
        with pytest.raises(ValueError):
            upload(client.filesystem("fixture"), "test1", data)
        assert not requests and not files


@pytest.mark.parametrize("status", [402, 500])
def test_failed_upload_is_not_retried(status):
    with store() as (client, _, requests, fail):
        fail(status)
        with pytest.raises(MountableError) as error:
            upload(client.filesystem("fixture"), "test1", report())
        assert error.value.status == status
        assert sum(r.method == "PUT" for r in requests) == 1
