"""Actual Kernel dispatch and released SDK over fake HTTP, without model calls."""

import asyncio
import hashlib
import json
from contextlib import contextmanager

import httpx
import pytest
from mountable_sdk import Mountable

from notes import NoteError, build_kernel, cleanup, invoke_note, note_path

PATH = "sk-note-test.md"
TEXT = "Review complete. Read the stored file."


@contextmanager
def fixture():
    files, modes, requests = {}, {}, []
    failure = None

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
            return httpx.Response(
                403, json={"detail": {"code": "read_only", "message": "private detail"}}
            )
        if request.method == "PUT":
            assert request.headers["If-None-Match"] == "*"
            if failure == "network":
                raise httpx.ReadError("private detail", request=request)
            if failure or path in files:
                status, code = (
                    failure or 412,
                    "precondition_failed"
                    if not failure
                    else "filesystem_spending_cap_reached",
                )
                return httpx.Response(
                    status, json={"detail": {"code": code, "message": "private detail"}}
                )
            files[path] = request.content
            return httpx.Response(
                201,
                json={
                    "path": path,
                    "type": "file",
                    "size": len(request.content),
                    "etag": "fixture",
                    "modified_at": "2026-10-27T00:00:00Z",
                },
            )
        if path not in files:
            return httpx.Response(404, json={"detail": {"code": "not_found"}})
        if request.method == "DELETE":
            assert request.headers["If-Match"] == '"fixture"'
            del files[path]
            return httpx.Response(204)
        return httpx.Response(
            200,
            content=files[path],
            headers={
                "ETag": '"fixture"',
                "Last-Modified": "Tue, 27 Oct 2026 00:00:00 GMT",
            },
        )

    def fail(value):
        nonlocal failure
        failure = value

    with Mountable(api_key="mtbl_fixture", base_url="https://files.example") as client:
        client._http.close()
        client._http = httpx.Client(transport=httpx.MockTransport(serve))
        yield client, files, requests, fail


def invoke(kernel, name, args=None):
    return asyncio.run(invoke_note(kernel, name, args or {}))


def test_real_metadata_dispatch_and_fresh_kernel():
    with fixture() as (client, files, requests, _):
        kernel = build_kernel(client.filesystem("fixture"), PATH)
        assert kernel.services == {}
        metadata = kernel.get_full_list_of_function_metadata()
        assert {m.name: [p.name for p in m.parameters] for m in metadata} == {
            "write_note": ["text"],
            "read_note": [],
        }
        param = next(m for m in metadata if m.name == "write_note").parameters[0]
        assert param.is_required and param.schema_data["type"] == "string"
        assert invoke(kernel, "write_note", {"text": TEXT}) == "stored"
        fresh = build_kernel(client.filesystem("fixture", "ro"), PATH)
        assert invoke(fresh, "read_note") == TEXT
        assert files[PATH] == TEXT.encode()
        assert sum(r.method == "PUT" for r in requests) == 1


def test_conflict_and_read_only_are_errors_and_preserve_first_bytes():
    with fixture() as (client, files, _, _):
        writer = build_kernel(client.filesystem("fixture"), PATH)
        invoke(writer, "write_note", {"text": TEXT})
        with pytest.raises(NoteError, match="412") as error:
            invoke(writer, "write_note", {"text": "replacement"})
        assert "private detail" not in str(error.value)
        reader = build_kernel(client.filesystem("fixture", "ro"), PATH)
        with pytest.raises(NoteError, match="403"):
            invoke(reader, "write_note", {"text": "replacement"})
        assert files[PATH] == TEXT.encode()


@pytest.mark.parametrize(
    "args",
    [
        {},
        {"text": 12},
        {"text": ""},
        {"text": "é" * 8193},
        {"text": TEXT, "path": "other"},
        {"text": TEXT, "filesystem": "other"},
    ],
)
def test_bad_arguments_touch_no_storage(args):
    with fixture() as (client, _, requests, _):
        kernel = build_kernel(client.filesystem("fixture"), PATH)
        with pytest.raises(ValueError):
            asyncio.run(invoke_note(kernel, "write_note", args))
        assert requests == []


@pytest.mark.parametrize(
    "path",
    ["../other", "/sk-note-test.md", "notes/a.md", "sk-note-../a.md", "sk-note-.md"],
)
def test_bad_backend_paths(path):
    with pytest.raises(ValueError):
        note_path(path)


@pytest.mark.parametrize("failure", [402, "network"])
def test_failed_write_is_an_error_without_retry(failure):
    with fixture() as (client, files, requests, fail):
        fail(failure)
        kernel = build_kernel(client.filesystem("fixture"), PATH)
        with pytest.raises(NoteError) as error:
            invoke(kernel, "write_note", {"text": TEXT})
        assert "private detail" not in str(error.value)
        assert sum(r.method == "PUT" for r in requests) == 1
        assert files == {}


@pytest.mark.parametrize("body", [b"\xff", b"x" * 16385])
def test_invalid_stored_text_is_an_error(body):
    with fixture() as (client, files, _, _):
        files[PATH] = body
        with pytest.raises(NoteError):
            invoke(build_kernel(client.filesystem("fixture", "ro"), PATH), "read_note")


def test_cleanup_requires_hash_and_etag():
    with fixture() as (client, files, _, _):
        store = client.filesystem("fixture")
        files[PATH] = TEXT.encode()
        with pytest.raises(NoteError):
            cleanup(store, PATH, "0" * 64)
        assert files
        cleanup(store, PATH, hashlib.sha256(TEXT.encode()).hexdigest())
        assert not files
