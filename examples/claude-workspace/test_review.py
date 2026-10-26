"""Offline checks for the real SDK tool and the reviewer's access boundary."""

import asyncio
from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from review import READ_TOOL, USAGE_TOOL, note_tool, review_options


def test_note_tool_uses_backend_path_even_with_hostile_arguments():
    paths = []

    def read(path):
        paths.append(path)
        return SimpleNamespace(data=b"Check empty input next.")

    filesystem = SimpleNamespace(id="chosen-fs", read=read)
    result = asyncio.run(
        note_tool(filesystem, "/chosen-note.txt").handler(
            {"filesystem_id": "someone-else", "path": "/secrets"}
        )
    )
    assert paths == ["/chosen-note.txt"]
    assert result == {"content": [{"type": "text", "text": "Check empty input next."}]}


@pytest.mark.parametrize("data", [b"\xff", b"x" * 16_385])
def test_non_text_or_oversized_note_is_not_sent(data):
    filesystem = SimpleNamespace(read=lambda path: SimpleNamespace(data=data))
    result = asyncio.run(note_tool(filesystem, "/note").handler({}))
    assert result["isError"] is True
    assert data.decode("utf-8", errors="replace") not in result["content"][0]["text"]


def test_provider_error_does_not_disclose_credentials():
    def read(path):
        raise MountableError(402, "payment_required", "private server detail")

    result = asyncio.run(note_tool(SimpleNamespace(read=read), "/note").handler({}))
    assert result == {
        "content": [{"type": "text", "text": "payment_required"}],
        "isError": True,
    }


@pytest.mark.parametrize(
    ("name", "args", "decision"),
    [
        (READ_TOOL, {}, "allow"),
        (READ_TOOL, {"path": "/secrets"}, "deny"),
        (USAGE_TOOL, {"filesystem_id": "chosen-fs", "org_id": "chosen-org"}, "allow"),
        (USAGE_TOOL, {"filesystem_id": "other-fs", "org_id": "chosen-org"}, "deny"),
        (USAGE_TOOL, {"filesystem_id": "chosen-fs", "org_id": "other-org"}, "deny"),
        ("mcp__mountable__create_filesystem", {"name": "new"}, "deny"),
        ("Bash", {"command": "env"}, "deny"),
    ],
)
def test_hook_checks_every_call(name, args, decision):
    options = review_options(SimpleNamespace(id="chosen-fs"), "/note", "chosen-org")
    assert options.tools == []
    assert options.setting_sources == []
    assert options.strict_mcp_config
    hook = options.hooks["PreToolUse"][0].hooks[0]
    result = asyncio.run(hook({"tool_name": name, "tool_input": args}, None, None))
    assert result["hookSpecificOutput"]["permissionDecision"] == decision
