"""smolagents tools that keep one note on a mountable.io filesystem.

The application chooses the filesystem and the note path. Both tools are real
smolagents tools, and this example calls ToolCallingAgent.execute_tool_call.
It does not call a model, and no tool argument can select a filesystem or a path.
"""

from __future__ import annotations

import argparse
import os
import re
from typing import Protocol

os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

from mountable_sdk import MountableError
from smolagents import Model, ToolCallingAgent, tool
from smolagents.monitoring import LogLevel
from smolagents.utils import AgentToolCallError

MAX_NOTE_BYTES = 16_384
_NOTE_PATH = re.compile(r"notes/[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\.md")


class NoteFile(Protocol):
    @property
    def data(self) -> bytes: ...


class NoteStore(Protocol):
    def write(self, path: str, data: str, *, if_none_match: bool) -> object: ...

    def read(self, path: str) -> NoteFile: ...


class UnusedModel(Model):
    """Stands in for a model. execute_tool_call never asks it to generate."""

    def __init__(self) -> None:
        super().__init__()
        self.calls = 0

    def generate(
        self,
        messages,
        stop_sequences=None,
        response_format=None,
        tools_to_call_from=None,
        **kwargs,
    ):
        del messages, stop_sequences, response_format, tools_to_call_from, kwargs
        self.calls += 1
        raise RuntimeError("this example does not call a model")


def note_path(path: str) -> str:
    """Accept one Markdown note under notes/, not a filesystem id or a path escape."""
    if not isinstance(path, str) or _NOTE_PATH.fullmatch(path) is None:
        raise ValueError("use a path like notes/brief.md")
    return path


def _refused(error: MountableError, *, writing: bool) -> str:
    if error.status is None:
        if writing:
            return "network_error: outcome unknown. Read the note before writing again."
        return "network_error: outcome unknown."
    if error.status == 412:
        return "precondition_failed: the note already exists. Read it instead."
    if error.status == 403 and error.code == "read_only":
        return "read_only: this session cannot write."
    if error.code in {"filesystem_spending_cap_reached", "spending_cap_reached"}:
        return f"{error.code}: this user's billable mountable.io work stops."
    return error.code


def build_agent(store: NoteStore, path: str) -> ToolCallingAgent:
    """Bind one path, then return an agent whose only job is to call these tools."""
    chosen = note_path(path)

    @tool
    def write_note(text: str) -> str:
        """Create the note the application already chose. Never replace an existing note.

        Args:
            text: The note as UTF-8 text, at most 16 KiB.
        """
        if len(text.encode("utf-8")) > MAX_NOTE_BYTES:
            return "error: note is larger than 16 KiB"
        try:
            store.write(chosen, text, if_none_match=True)
        except MountableError as error:
            return f"error: {_refused(error, writing=True)}"
        return "stored"

    @tool
    def read_note() -> str:
        """Read the note the application already chose."""
        try:
            data = store.read(chosen).data
        except MountableError as error:
            return f"error: {_refused(error, writing=False)}"
        if len(data) > MAX_NOTE_BYTES:
            return "error: note is larger than 16 KiB"
        try:
            return data.decode("utf-8")
        except UnicodeError:
            return "error: note is not UTF-8 text"

    return ToolCallingAgent(
        tools=[write_note, read_note],
        model=UnusedModel(),
        verbosity_level=LogLevel.ERROR,
        add_base_tools=False,
    )


def call_tool(agent: ToolCallingAgent, name: str, arguments: dict) -> str:
    """Run one tool through smolagents' tool-call path. A bad schema is an error string."""
    try:
        result = agent.execute_tool_call(name, arguments)
    except AgentToolCallError as error:
        return f"error: {error}"
    return str(result).strip()


def write_note(store: NoteStore, path: str, text: str) -> str:
    return call_tool(build_agent(store, path), "write_note", {"text": text})


def read_note(store: NoteStore, path: str) -> str:
    return call_tool(build_agent(store, path), "read_note", {})


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("write", "read"))
    parser.add_argument("--filesystem", required=True)
    parser.add_argument("--path", required=True)
    parser.add_argument("--text", default="")
    args = parser.parse_args()
    note_path(args.path)
    if args.command == "write" and not args.text:
        raise SystemExit("error: pass the note text with --text")

    from mountable_sdk import Mountable

    mode = "rw" if args.command == "write" else "ro"
    with Mountable() as client:
        store = client.filesystem(args.filesystem, mode)
        if args.command == "write":
            output = write_note(store, args.path, args.text)
        else:
            output = read_note(store, args.path)
    print(f"path={args.path}")
    if output.startswith("error:"):
        print(output)
        raise SystemExit(1)
    print(output)


if __name__ == "__main__":
    main()
