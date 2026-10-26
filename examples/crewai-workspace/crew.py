"""A CrewAI crew that hands one note through a mountable.io filesystem.

The application chooses the filesystem and the note path. This example does
not call a model provider. A scripted stand-in tells the writer to store the
note and the reader to read that file. The writer's answer is the word
"stored", so the reader does not receive the note body from the previous task.
"""

from __future__ import annotations

import argparse
import os
import re
from typing import Any, Protocol

os.environ.setdefault("OTEL_SDK_DISABLED", "true")

from crewai import Agent, Crew, Process, Task
from crewai.llms.base_llm import BaseLLM
from crewai.tools import BaseTool
from mountable_sdk import MountableError
from pydantic import BaseModel, ConfigDict, PrivateAttr

MAX_NOTE_BYTES = 16_384
_NOTE_PATH = re.compile(r"notes/[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\.md")


class NoteFile(Protocol):
    @property
    def data(self) -> bytes: ...


class NoteStore(Protocol):
    def write(self, path: str, data: str, *, if_none_match: bool) -> object: ...

    def read(self, path: str) -> NoteFile: ...


class EmptyInput(BaseModel):
    """The application already chose the file. The tool takes no arguments."""


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


class WriteNoteTool(BaseTool):
    model_config = ConfigDict(arbitrary_types_allowed=True)
    name: str = "write_note"
    description: str = "Store this crew's note on the shared filesystem. Call it once."
    args_schema: type[BaseModel] = EmptyInput
    store: Any
    path: str
    text: str

    def _run(self) -> str:
        try:
            path = note_path(self.path)
            text = note_text(self.text)
        except ValueError as error:
            return f"error: {error}"
        try:
            self.store.write(path, text, if_none_match=True)
        except MountableError as error:
            return f"error: {_refused(error, writing=True)}"
        return "stored"


class ReadNoteTool(BaseTool):
    model_config = ConfigDict(arbitrary_types_allowed=True)
    name: str = "read_note"
    description: str = "Read this crew's note from the shared filesystem."
    args_schema: type[BaseModel] = EmptyInput
    store: Any
    path: str

    def _run(self) -> str:
        try:
            path = note_path(self.path)
        except ValueError as error:
            return f"error: {error}"
        try:
            data = self.store.read(path).data
        except MountableError as error:
            return f"error: {_refused(error, writing=False)}"
        if len(data) > MAX_NOTE_BYTES:
            return "error: note is larger than 16 KiB"
        try:
            return data.decode("utf-8")
        except UnicodeError:
            return "error: note is not UTF-8 text"


class ScriptedNoteLLM(BaseLLM):
    """Stand-in that asks each agent to use its one file tool. No network."""

    model: str = "scripted-note-handoff"
    _steps: dict[int, int] = PrivateAttr(default_factory=dict)
    _write_error: str | None = PrivateAttr(default=None)

    def get_input_tokens_count(
        self,
        messages: str | list,
        tools: list | None = None,
        response_model: type[BaseModel] | None = None,
    ) -> int:
        del messages, tools, response_model
        return 1

    def call(
        self,
        messages: str | list,
        tools: list | None = None,
        callbacks: list | None = None,
        available_functions: dict | None = None,
        from_task: object | None = None,
        from_agent: object | None = None,
        response_model: type[BaseModel] | None = None,
    ) -> str:
        del callbacks, available_functions, from_task, response_model
        names = _tool_names(tools) or _tool_names(getattr(from_agent, "tools", None))
        key = id(from_agent)
        step = self._steps.get(key, 0)
        self._steps[key] = step + 1
        if "write_note" in names:
            if step == 0:
                return (
                    "Thought: store the note on the shared filesystem\n"
                    "Action: write_note\n"
                    "Action Input: {}"
                )
            observed = _last_observation(messages)
            if observed != "stored":
                self._write_error = observed or "error: the note was not stored"
                return (
                    "Thought: the note was not stored\n"
                    f"Final Answer: {self._write_error}"
                )
            return "Thought: the note is stored\nFinal Answer: stored"
        if "read_note" in names:
            if self._write_error:
                return (
                    "Thought: the writer did not store the note\n"
                    f"Final Answer: {self._write_error}"
                )
            if step == 0:
                return (
                    "Thought: read the note from the shared filesystem\n"
                    "Action: read_note\n"
                    "Action Input: {}"
                )
            observed = _last_observation(messages)
            if not observed:
                observed = "error: the file tool returned nothing"
            return f"Thought: the file holds the note\nFinal Answer: {observed}"
        return "Thought: this agent has no file tool\nFinal Answer: error: no file tool"


def _tool_names(tools: list | None) -> set[str]:
    names: set[str] = set()
    for tool in tools or []:
        if isinstance(tool, dict):
            for key, value in tool.items():
                if key in {"name", "tool"} and isinstance(value, str):
                    names.add(value)
                elif hasattr(value, "name"):
                    names.add(str(value.name))
        elif hasattr(tool, "name"):
            names.add(str(tool.name))
    return names


def _last_observation(messages: str | list) -> str:
    """Return the tool result CrewAI appended to the assistant message."""
    chunks: list[str] = []
    if isinstance(messages, str):
        chunks.append(messages)
    else:
        for message in messages:
            if isinstance(message, str):
                chunks.append(message)
            elif isinstance(message, dict) and message.get("role") == "assistant":
                content = message.get("content", "")
                chunks.append(content if isinstance(content, str) else str(content))
    observed = ""
    for chunk in chunks:
        if "Observation:" in chunk:
            observed = chunk.rsplit("Observation:", 1)[-1].strip()
    return observed


def build_crew(
    store: NoteStore, path: str, text: str | None, *, read_only: bool
) -> Crew:
    """Return a two-agent handoff crew, or a one-agent reader when read_only."""
    llm = ScriptedNoteLLM(model="scripted-note-handoff")
    if read_only:
        reader = Agent(
            role="Note reader",
            goal="Read the shared note from the filesystem.",
            backstory="You read one file with read_note. You do not write.",
            tools=[ReadNoteTool(store=store, path=path)],
            llm=llm,
            verbose=False,
            max_iter=3,
            allow_delegation=False,
        )
        task = Task(
            description="Read the shared note. Do not invent its text.",
            expected_output="The note text from the filesystem.",
            agent=reader,
        )
        return Crew(
            agents=[reader],
            tasks=[task],
            process=Process.sequential,
            verbose=False,
            share_crew=False,
        )

    writer = Agent(
        role="Note writer",
        goal="Store the note on the shared filesystem.",
        backstory="You store one file with write_note. You do not read it back.",
        tools=[WriteNoteTool(store=store, path=path, text=text or "")],
        llm=llm,
        verbose=False,
        max_iter=3,
        allow_delegation=False,
    )
    reader = Agent(
        role="Note reader",
        goal="Read the shared note from the filesystem.",
        backstory="You read one file with read_note. The previous answer is not the note.",
        tools=[ReadNoteTool(store=store, path=path)],
        llm=llm,
        verbose=False,
        max_iter=3,
        allow_delegation=False,
    )
    write_task = Task(
        description="Store the note on the shared filesystem.",
        expected_output="The word stored.",
        agent=writer,
    )
    read_task = Task(
        description="Read the shared note from the filesystem. Do not invent its text.",
        expected_output="The note text from the filesystem.",
        agent=reader,
        context=[write_task],
    )
    return Crew(
        agents=[writer, reader],
        tasks=[write_task, read_task],
        process=Process.sequential,
        verbose=False,
        share_crew=False,
    )


def run_crew(store: NoteStore, path: str, text: str | None, *, read_only: bool) -> str:
    crew = build_crew(store, path, text, read_only=read_only)
    result = crew.kickoff()
    raw = getattr(result, "raw", result)
    return str(raw).strip()


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
        output = run_crew(
            store,
            args.path,
            args.text if args.command == "handoff" else None,
            read_only=args.command == "resume",
        )
    print(f"path={args.path}")
    if output.startswith("error:"):
        print(output)
        raise SystemExit(1)
    print(output)


if __name__ == "__main__":
    main()
