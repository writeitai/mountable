"""Exercise the Agents SDK tool invocation pipeline without a model or network."""

import asyncio
import json
from types import SimpleNamespace

import pytest
from agents.tool_context import ToolContext
from mountable_sdk import MountableError

from workspace import MAX_NOTE_BYTES, Workspace, read_note, write_note


class Notes:
    def __init__(self):
        self.files = {}
        self.paths = []

    def write(self, path, text, *, if_none_match):
        assert if_none_match
        self.paths.append(path)
        if path in self.files:
            raise MountableError(412, "precondition_failed", "private detail")
        self.files[path] = text.encode()

    def read(self, path):
        return SimpleNamespace(data=self.files[path])


def invoke(tool, workspace, arguments):
    return asyncio.run(
        tool.on_invoke_tool(
            ToolContext(
                context=workspace,
                tool_name=tool.name,
                tool_call_id="test-call",
                tool_arguments=json.dumps(arguments),
            ),
            json.dumps(arguments),
        )
    )


def test_users_remain_separate_and_new_context_keeps_files():
    alice, bob = Notes(), Notes()
    a, b = Workspace(alice, "/note.txt"), Workspace(bob, "/note.txt")
    assert invoke(write_note, a, {"text": "Alice's note"}) == "Note created."
    assert invoke(write_note, b, {"text": "Bob's note"}) == "Note created."
    assert invoke(read_note, Workspace(alice, "/note.txt"), {}) == "Alice's note"
    assert invoke(read_note, b, {}) == "Bob's note"
    assert "already exists" in invoke(write_note, a, {"text": "replacement"})
    assert invoke(read_note, a, {}) == "Alice's note"


def test_schemas_expose_no_identity_or_path_and_rogue_args_cannot_redirect():
    assert set(write_note.params_json_schema["properties"]) == {"text"}
    assert read_note.params_json_schema["properties"] == {}
    notes = Notes()
    workspace = Workspace(notes, "/chosen.txt")
    # Even a provider ignoring additionalProperties cannot redirect the tool.
    invoke(
        write_note,
        workspace,
        {"text": "safe", "filesystem_id": "other-user", "path": "/secret"},
    )
    assert notes.paths == ["/chosen.txt"]


def test_spending_cap_stops_run_instead_of_returning_error_to_model():
    class Capped:
        def write(self, *args, **kwargs):
            raise MountableError(402, "filesystem_spending_cap_reached", "secret")

    with pytest.raises(MountableError) as error:
        invoke(write_note, Workspace(Capped(), "/note.txt"), {"text": "hello"})
    assert error.value.status == 402
    assert error.value.code == "filesystem_spending_cap_reached"


def test_multibyte_note_limit_and_binary_read():
    notes = Notes()
    workspace = Workspace(notes, "/note.txt")
    assert "exceeds" in invoke(write_note, workspace, {"text": "é" * MAX_NOTE_BYTES})
    assert notes.files == {}
    notes.files["/note.txt"] = b"x" * (MAX_NOTE_BYTES + 1)
    with pytest.raises(ValueError):
        invoke(read_note, workspace, {})
    notes.files["/note.txt"] = b"\xff"
    with pytest.raises(UnicodeDecodeError):
        invoke(read_note, workspace, {})


def scripted_model():
    from agents.models.interface import Model, ModelResponse
    from agents.usage import Usage
    from openai.types.responses import (
        ResponseFunctionToolCall,
        ResponseOutputMessage,
        ResponseOutputText,
    )

    class ScriptedModel(Model):
        def __init__(self):
            self.step = 0

        async def get_response(self, *args, **kwargs):
            self.step += 1
            if self.step <= 2:
                item = ResponseFunctionToolCall(
                    type="function_call",
                    call_id=f"call-{self.step}",
                    name="write_note" if self.step == 1 else "read_note",
                    arguments='{"text":"saved note"}' if self.step == 1 else "{}",
                )
            else:
                item = ResponseOutputMessage(
                    type="message",
                    id="final",
                    role="assistant",
                    status="completed",
                    content=[
                        ResponseOutputText(
                            type="output_text", text="Saved note.", annotations=[]
                        )
                    ],
                )
            return ModelResponse(output=[item], usage=Usage(), response_id=None)

        async def stream_response(self, *args, **kwargs):
            raise NotImplementedError
            yield  # Model's streaming interface is an async generator.

    return ScriptedModel()


def test_runner_completes_both_tools_without_an_api_key(capsys):
    from workspace import run

    notes = Notes()
    asyncio.run(run(notes, "/chosen.txt", "saved note", scripted_model()))
    assert notes.files == {"/chosen.txt": b"saved note"}
    output = json.loads(capsys.readouterr().out)
    assert output["tools"] == ["read_note", "write_note"]


def test_runner_cap_error_is_sanitized_at_cli(monkeypatch, capsys):
    from agents.exceptions import UserError

    import workspace

    class Capped:
        def write(self, *args, **kwargs):
            raise MountableError(
                402, "filesystem_spending_cap_reached", "PRIVATE provider detail"
            )

    def capped_run():
        asyncio.run(workspace.run(Capped(), "/note.txt", "hello", scripted_model()))

    with pytest.raises(UserError) as caught:
        capped_run()
    assert isinstance(caught.value.__cause__, MountableError)
    assert caught.value.__cause__.status == 402
    assert caught.value.__cause__.code == "filesystem_spending_cap_reached"
    capsys.readouterr()
    monkeypatch.setattr(workspace, "main", capped_run)
    with pytest.raises(SystemExit) as stopped:
        workspace.cli()
    assert stopped.value.code == 1
    output = capsys.readouterr()
    assert json.loads(output.out) == {
        "error": "filesystem_spending_cap_reached",
        "status": 402,
    }
    assert "PRIVATE" not in output.out + output.err
