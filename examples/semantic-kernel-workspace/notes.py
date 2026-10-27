"""Backend-bound Semantic Kernel native functions over a mountable.io file.

No model service is registered. The CLI invokes the actual Kernel dispatcher.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import logging
import re
from typing import Annotated, Protocol

from mountable_sdk import MountableError
from semantic_kernel import Kernel
from semantic_kernel.exceptions import KernelInvokeException
from semantic_kernel.functions import KernelArguments, kernel_function

MAX_NOTE_BYTES = 16_384
_NOTE_PATH = re.compile(r"sk-note-[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}\.md")


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
    """A sanitized failure, never a successful function result."""


def note_path(path: str) -> str:
    if not isinstance(path, str) or _NOTE_PATH.fullmatch(path) is None:
        raise ValueError("use a unique root filename like sk-note-review.md")
    return path


def note_text(text: str) -> str:
    if not isinstance(text, str) or not 0 < len(text.encode("utf-8")) <= MAX_NOTE_BYTES:
        raise ValueError("note must be nonempty UTF-8 text, at most 16 KiB")
    return text


def storage_failure(error: MountableError) -> NoteError:
    if error.status is None:
        return NoteError("network_error: outcome unknown; read before writing again")
    return NoteError(f"storage refused: HTTP {error.status} {error.code}")


class NotesPlugin:
    def __init__(self, store: NoteStore, path: str) -> None:
        self._store = store
        self._path = note_path(path)

    @kernel_function(
        description="Create the application-selected note without replacing it."
    )
    def write_note(self, text: Annotated[str, "Note text, at most 16 KiB"]) -> str:
        text = note_text(text)
        try:
            self._store.write(self._path, text, if_none_match=True)
        except MountableError as error:
            raise storage_failure(error) from None
        return "stored"

    @kernel_function(description="Read the application-selected note.")
    def read_note(self) -> str:
        try:
            data = self._store.read(self._path).data
        except MountableError as error:
            raise storage_failure(error) from None
        if len(data) > MAX_NOTE_BYTES:
            raise NoteError("stored note exceeds 16 KiB")
        try:
            return data.decode("utf-8")
        except UnicodeError:
            raise NoteError("stored note is not UTF-8") from None


def build_kernel(store: NoteStore, path: str) -> Kernel:
    kernel = Kernel()
    kernel.add_plugin(NotesPlugin(store, path), plugin_name="notes")
    return kernel


async def invoke_note(kernel: Kernel, name: str, arguments: dict) -> str:
    """Validate the small public argument boundary, then use Kernel.invoke.

    KernelArguments accepts extra keys; schema metadata alone is not an
    authorization boundary. Refuse extras before handing them to the kernel.
    """
    expected = {"write_note": {"text"}, "read_note": set()}
    if name not in expected or set(arguments) != expected[name]:
        raise ValueError("pass only text to write_note, or no arguments to read_note")
    if name == "write_note":
        note_text(arguments["text"])
    try:
        result = await kernel.invoke(
            plugin_name="notes",
            function_name=name,
            arguments=KernelArguments(**arguments),
        )
    except KernelInvokeException as error:
        # Semantic Kernel wraps native failures. Preserve our sanitized reason.
        cause = error.__cause__
        while cause is not None:
            if isinstance(cause, NoteError):
                raise NoteError(str(cause)) from None
            cause = cause.__cause__
        raise NoteError("invocation failed; read before retrying a write") from None
    if result is None:
        raise NoteError("invocation returned no result")
    return str(result)


def cleanup(store: NoteStore, path: str, sha256: str) -> None:
    """Application-only cleanup; never exposed as a kernel function."""
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

    # Framework error logging must not dump invocation arguments into CLI logs.
    logging.getLogger("semantic_kernel").setLevel(logging.CRITICAL)
    with Mountable() as client:
        store = client.filesystem(
            args.filesystem, "ro" if args.command == "read" else "rw"
        )
        if args.command == "cleanup":
            cleanup(store, args.path, args.sha256)
            output = "deleted"
        else:
            kernel = build_kernel(store, args.path)
            name = "write_note" if args.command == "write" else "read_note"
            output = await invoke_note(
                kernel, name, {"text": args.text} if args.command == "write" else {}
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
