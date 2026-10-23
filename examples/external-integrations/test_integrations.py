"""Real framework/SDK calls over fake HTTP; no model, cloud keys or billing."""

from __future__ import annotations

import json
import os
from contextlib import contextmanager

os.environ["MEM0_TELEMETRY"] = "false"

import httpx
import pytest
from mem0 import MemoryClient
from mountable_sdk import Mountable
from pydantic_ai.messages import ModelResponse, TextPart, ToolCallPart, ToolReturnPart
from pydantic_ai.models.function import FunctionModel

from mem0_snapshot import archive_user_memories
from pydantic_workspace import build_agent


@contextmanager
def filesystem_api(*, status=200, code="", content=b"saved note", timeout=False):
    requests = []

    def serve(request):
        requests.append(request)
        if request.url.path == "/api/v1/mount-sessions":
            return httpx.Response(
                201,
                json={"token": "mtblst_fake", "expires_at": "2099-01-01T00:00:00Z"},
            )
        if timeout and request.method == "PUT":
            raise httpx.ReadTimeout("fake response lost after write", request=request)
        if status >= 400:
            return httpx.Response(status, json={"detail": {"code": code}})
        if request.method == "GET":
            return httpx.Response(
                200,
                content=content,
                headers={
                    "ETag": '"one"',
                    "Last-Modified": "Fri, 23 Oct 2026 00:00:00 GMT",
                },
            )
        return httpx.Response(
            201,
            json={
                "path": "notes/summary.md",
                "type": "file",
                "size": len(request.content),
                "etag": '"one"',
                "modified_at": "2026-10-23T00:00:00Z",
            },
        )

    with Mountable(
        api_key="mtbl_test_only", base_url="https://files.example"
    ) as client:
        client._http.close()
        client._http = httpx.Client(transport=httpx.MockTransport(serve))
        yield client.filesystem("fs_authenticated_user"), requests


def model_for(tool, args, schemas):
    def respond(messages, info):
        schemas.extend(info.function_tools)
        for part in messages[-1].parts:
            if isinstance(part, ToolReturnPart):
                return ModelResponse(parts=[TextPart(str(part.content))])
        return ModelResponse(parts=[ToolCallPart(tool, args, tool_call_id="one")])

    return FunctionModel(respond)


def test_real_pydantic_tool_uses_backend_filesystem_and_create_only_header():
    schemas = []
    with filesystem_api() as (filesystem, requests):
        agent = build_agent(
            model_for("create_note", {"name": "summary.md", "text": "hello"}, schemas)
        )
        result = agent.run_sync("Save a note", deps=filesystem)
    assert result.output == "Created summary.md"
    assert len(requests) == 2
    assert json.loads(requests[0].content)["filesystem_id"] == "fs_authenticated_user"
    assert requests[1].url.path.endswith(
        "/fs_authenticated_user/files/notes/summary.md"
    )
    assert requests[1].headers["If-None-Match"] == "*"
    assert requests[1].content == b"hello"
    for schema in schemas:
        properties = schema.parameters_json_schema.get("properties", {})
        assert "filesystem_id" not in properties
        assert "api_key" not in properties


@pytest.mark.parametrize("name", ["../secret.md", "/secret.md", "nested/file.md"])
def test_tool_rejects_paths_before_network(name):
    with filesystem_api() as (filesystem, requests):
        agent = build_agent(model_for("read_note", {"name": name}, []))
        assert "Error:" in agent.run_sync("Read", deps=filesystem).output
    assert requests == []


def test_real_pydantic_tool_reads_utf8():
    with filesystem_api(content="Ahoj světe".encode()) as (filesystem, _):
        agent = build_agent(model_for("read_note", {"name": "summary.md"}, []))
        assert agent.run_sync("Read", deps=filesystem).output == "Ahoj světe"


@pytest.mark.parametrize(
    ("status", "code"),
    [(402, "filesystem_spending_cap_reached"), (412, "precondition_failed")],
)
def test_failed_write_is_reported_without_retry(status, code):
    with filesystem_api(status=status, code=code) as (filesystem, requests):
        agent = build_agent(
            model_for("create_note", {"name": "summary.md", "text": "hello"}, [])
        )
        assert code in agent.run_sync("Save", deps=filesystem).output
    assert len([r for r in requests if r.method == "PUT"]) == 1


def test_create_timeout_reports_unknown_outcome_without_repeating_write():
    with filesystem_api(timeout=True) as (filesystem, requests):
        agent = build_agent(
            model_for("create_note", {"name": "summary.md", "text": "hello"}, [])
        )
        output = agent.run_sync("Save", deps=filesystem).output
    assert "Outcome unknown" in output
    assert "Read it before" in output
    assert len([r for r in requests if r.method == "PUT"]) == 1


@contextmanager
def mem0_api(pages):
    requests = []

    def serve(request):
        requests.append(request)
        if request.url.path == "/v1/ping/":
            return httpx.Response(
                200, json={"org_id": "org_test", "project_id": "p_test"}
            )
        assert request.url.host == "memory.example"
        assert request.url.path == "/v3/memories/"
        assert json.loads(request.content)["filters"] == {"user_id": "user_42"}
        page = int(request.url.params["page"])
        return httpx.Response(200, json=pages[page - 1])

    with httpx.Client(transport=httpx.MockTransport(serve)) as http:
        client = MemoryClient(
            api_key="mem0_test", host="https://memory.example", client=http
        )
        yield client, requests


def test_real_mem0_pagination_archives_once_and_never_follows_next_url():
    pages = [
        {
            "results": [{"id": "m1", "memory": "prefers tea"}],
            "next": "https://evil.example",
        },
        {"results": [{"id": "m2", "memory": "writes Python"}], "next": None},
    ]
    with (
        mem0_api(pages) as (memory, requests),
        filesystem_api() as (filesystem, writes),
    ):
        assert (
            archive_user_memories(
                memory, filesystem, "user_42", "mem0-exports/one.json"
            )
            == 2
        )
    assert len(requests) == 3  # authentication plus two list pages
    puts = [r for r in writes if r.method == "PUT"]
    assert len(puts) == 1
    document = json.loads(puts[0].content)
    assert [r["id"] for r in document["records"]] == ["m1", "m2"]
    assert document["user_id"] == "user_42"
    assert "mem0_test" not in puts[0].content.decode()
    assert puts[0].headers["If-None-Match"] == "*"


@pytest.mark.parametrize(
    "pages",
    [
        [{"results": [{"id": "m1"}], "next": "next"}, {"results": [{"id": "m1"}]}],
        [{"results": [{"id": "m1", "user_id": "other"}]}],
        [{"results": [], "next": "next"}],
        [{"results": [{"id": "m1", "memory": "x" * 1_048_576}]}],
    ],
)
def test_partial_or_invalid_export_does_not_write(pages):
    with (
        mem0_api(pages) as (memory, _),
        filesystem_api() as (filesystem, requests),
        pytest.raises(ValueError),
    ):
        archive_user_memories(memory, filesystem, "user_42", "mem0-exports/one.json")
    assert requests == []
