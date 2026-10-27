"""Backend-bound Strands @tool file operations over a mountable.io file.

No Agent or model provider is created. The CLI invokes the tools through
Strands' validated tool-use stream (`DecoratedFunctionTool.stream`).
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import logging
import re
import uuid
from typing import Protocol

from mountable_sdk import MountableError
from strands import tool
from strands.tools.decorator import DecoratedFunctionTool

MAX_NOTE_BYTES = 16_384
_NOTE_PATH = re.compile(r"strands-note-[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}\.md")


class NoteFile(Protocol):
    @property
    def data(self) -> bytes: ...

    @property
    def info(self): ...


class NoteStore(Protocol):
    def write(self, path: str, data: str, *, if_none_match: bool) -> object: ...

    def read(self, path: str) -> NoteFile: ...

    def delete(self, path: str, *, if_match: str) -> None: ...


class NoteError(RuntimeError):
    """A sanitized failure, never a successful tool result."""


def note_path(path: str) -> str:
    if not isinstance(path, str) or _NOTE_PATH.fullmatch(path) is None:
        raise ValueError("use a unique root filename like strands-note-<id>.md")
    return path


def note_text(text: str) -> str:
    if not isinstance(text, str) or not 0 < len(text.encode("utf-8")) <= MAX_NOTE_BYTES:
        raise ValueError("note must be nonempty UTF-8 text, at most 16 KiB")
    return text


def storage_failure(error: MountableError) -> NoteError:
    if error.status is None:
        return NoteError("network_error: outcome unknown; read before writing again")
    return NoteError(f"storage refused: HTTP {error.status} {error.code}")


def build_tools(store: NoteStore, path: str) -> dict[str, DecoratedFunctionTool]:
    """Bind the backend-selected store and path; neither is a tool argument."""
    chosen = note_path(path)

    @tool
    def write_note(text: str) -> str:
        """Create the application-selected note without replacing it."""
        note_text(text)
        try:
            store.write(chosen, text, if_none_match=True)
        except MountableError as error:
            raise storage_failure(error) from None
        return "stored"

    @tool
    def read_note() -> str:
        """Read the application-selected note."""
        try:
            data = store.read(chosen).data
        except MountableError as error:
            raise storage_failure(error) from None
        if len(data) > MAX_NOTE_BYTES:
            raise NoteError("stored note exceeds 16 KiB")
        try:
            return data.decode("utf-8")
        except UnicodeError:
            raise NoteError("stored note is not UTF-8") from None

    return {"write_note": write_note, "read_note": read_note}


def tool_schemas(tools: dict[str, DecoratedFunctionTool]) -> dict[str, list[str]]:
    return {
        name: sorted(t.tool_spec["inputSchema"]["json"].get("properties", {}))
        for name, t in tools.items()
    }


async def invoke_tool(
    tools: dict[str, DecoratedFunctionTool], name: str, arguments: dict
) -> str:
    """Refuse caller-selected authority, then use the validated tool-use stream.

    Strands' input validation silently drops unknown keys, so schema metadata
    alone is not an authorization boundary. Reject extras before dispatch.
    """
    if name not in tools:
        raise ValueError("unknown tool")
    expected = set(tool_schemas(tools)[name])
    if set(arguments) != expected:
        raise ValueError(f"pass only {sorted(expected)} to {name}")
    if name == "write_note":
        note_text(arguments["text"])
    tool_use = {"toolUseId": uuid.uuid4().hex, "name": name, "input": arguments}
    result = None
    async for event in tools[name].stream(tool_use, {}):
        if isinstance(event, dict) and event.get("type") == "tool_result":
            result = event["tool_result"]
    if result is None:
        raise NoteError("invocation returned no result")
    text = "".join(part.get("text", "") for part in result.get("content", []))
    if result.get("status") != "success":
        # Our tools raise only sanitized NoteError/ValueError messages.
        reason = text.removeprefix("Error: ").removeprefix("NoteError - ")
        reason = reason.removeprefix("ValueError - ")
        raise NoteError(reason or "invocation failed; read before retrying a write")
    return text


def cleanup(store: NoteStore, path: str, sha256: str) -> None:
    """Application-only cleanup; never exposed as a tool."""
    chosen = note_path(path)
    if re.fullmatch(r"[0-9a-f]{64}", sha256) is None:
        raise ValueError("pass the expected note SHA-256")
    current = store.read(chosen)
    if len(current.data) > MAX_NOTE_BYTES:
        raise NoteError("refusing cleanup: file exceeds 16 KiB")
    if hashlib.sha256(current.data).hexdigest() != sha256 or not current.info.etag:
        raise NoteError("refusing cleanup: changed bytes or missing ETag")
    store.delete(chosen, if_match=current.info.etag)


async def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("write", "read", "cleanup"))
    parser.add_argument("--filesystem", required=True)
    parser.add_argument("--path", required=True)
    parser.add_argument("--text", default="")
    parser.add_argument("--sha256", default="")
    args = parser.parse_args()
    note_path(args.path)
    if args.command == "write":
        note_text(args.text)
    from mountable_sdk import Mountable

    logging.getLogger("strands").setLevel(logging.CRITICAL)
    with Mountable() as client:
        store = client.filesystem(
            args.filesystem, "ro" if args.command == "read" else "rw"
        )
        if args.command == "cleanup":
            cleanup(store, args.path, args.sha256)
            output = "deleted"
        else:
            tools = build_tools(store, args.path)
            name = "write_note" if args.command == "write" else "read_note"
            output = await invoke_tool(
                tools, name, {"text": args.text} if args.command == "write" else {}
            )
    print(output)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except (NoteError, MountableError, ValueError) as error:
        reason = (
            str(storage_failure(error))
            if isinstance(error, MountableError)
            else str(error)
        )
        raise SystemExit(f"error: {reason}") from None
