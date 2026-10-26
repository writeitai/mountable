"""LangGraph nodes that share one mountable.io filesystem across runs.

The application chooses the filesystem and the note path. This example uses
an in-memory checkpointer. The writer returns empty text, so the next node
receives no note body in its input and reads the file. A later process, with
a new in-memory checkpointer, reads the same path.
"""

from __future__ import annotations

import argparse
import re
import uuid
from typing import Protocol

from langgraph.checkpoint.memory import MemorySaver
from langgraph.graph import END, START, StateGraph
from mountable_sdk import MountableError
from typing_extensions import TypedDict

MAX_NOTE_BYTES = 16_384
_NOTE_PATH = re.compile(r"notes/[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\.md")


class NoteState(TypedDict, total=False):
    path: str
    text: str
    error: str


class NoteFile(Protocol):
    @property
    def data(self) -> bytes: ...


class NoteStore(Protocol):
    def write(self, path: str, data: str, *, if_none_match: bool) -> object: ...

    def read(self, path: str) -> NoteFile: ...


def note_path(path: str) -> str:
    """Accept one Markdown note under notes/, not a filesystem id or a path escape."""
    if not isinstance(path, str) or _NOTE_PATH.fullmatch(path) is None:
        raise ValueError("use a path like notes/brief.md")
    return path


def note_text(text: str) -> str:
    if len(text.encode("utf-8")) > MAX_NOTE_BYTES:
        raise ValueError("note is larger than 16 KiB")
    return text


def _refused(error: MountableError, *, writing: bool) -> str:
    if error.status is None:
        if writing:
            return "network_error: outcome unknown. Read the note before writing again."
        return "network_error: outcome unknown."
    if error.status == 412:
        return "precondition_failed: the note already exists. Read it instead."
    if error.code in {"filesystem_spending_cap_reached", "spending_cap_reached"}:
        return f"{error.code}: this user's billable mountable.io work stops."
    return error.code


def build_graphs(store: NoteStore):
    """Return (handoff, resume). Each graph has its own in-memory checkpointer."""

    def write_note(state: NoteState) -> dict:
        try:
            path = note_path(state["path"])
            text = note_text(state["text"])
        except (KeyError, ValueError) as error:
            return {"text": "", "error": str(error)}
        try:
            store.write(path, text, if_none_match=True)
        except MountableError as error:
            return {"text": "", "error": _refused(error, writing=True)}
        # read_note receives empty text. Earlier checkpoint steps still hold the input.
        return {"text": "", "error": ""}

    def read_note(state: NoteState) -> dict:
        try:
            path = note_path(state["path"])
        except (KeyError, ValueError) as error:
            return {"text": "", "error": str(error)}
        try:
            data = store.read(path).data
        except MountableError as error:
            return {"text": "", "error": _refused(error, writing=False)}
        if len(data) > MAX_NOTE_BYTES:
            return {"text": "", "error": "note is larger than 16 KiB"}
        try:
            text = data.decode("utf-8")
        except UnicodeError:
            return {"text": "", "error": "note is not UTF-8 text"}
        return {"text": text, "error": ""}

    def after_write(state: NoteState) -> str:
        return "read_note" if not state.get("error") else END

    handoff = StateGraph(NoteState)
    handoff.add_node("write_note", write_note)
    handoff.add_node("read_note", read_note)
    handoff.add_edge(START, "write_note")
    handoff.add_conditional_edges("write_note", after_write, ["read_note", END])
    handoff.add_edge("read_note", END)

    resume = StateGraph(NoteState)
    resume.add_node("read_note", read_note)
    resume.add_edge(START, "read_note")
    resume.add_edge("read_note", END)
    return (
        handoff.compile(checkpointer=MemorySaver()),
        resume.compile(checkpointer=MemorySaver()),
    )


def run_graph(graph, state: NoteState) -> tuple[str, NoteState]:
    thread_id = uuid.uuid4().hex
    result = graph.invoke(state, config={"configurable": {"thread_id": thread_id}})
    return thread_id, result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("handoff", "resume"))
    parser.add_argument("--filesystem", required=True)
    parser.add_argument("--path", required=True)
    parser.add_argument("--text", default="")
    args = parser.parse_args()

    from mountable_sdk import Mountable

    mode = "rw" if args.command == "handoff" else "ro"
    with Mountable() as client:
        store = client.filesystem(args.filesystem, mode)
        handoff, resume = build_graphs(store)
        graph = handoff if args.command == "handoff" else resume
        state: NoteState = {"path": args.path}
        if args.command == "handoff":
            state["text"] = args.text
        thread_id, result = run_graph(graph, state)
    error = result.get("error") or ""
    print(f"thread_id={thread_id}")
    print(f"path={args.path}")
    if error:
        print(f"error={error}")
        raise SystemExit(1)
    print(result.get("text", ""))


if __name__ == "__main__":
    main()
