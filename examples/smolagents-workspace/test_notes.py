"""Offline checks: real smolagents tools, a fake filesystem, no network."""

import pytest
from mountable_sdk import MountableError

from notes import build_agent, call_tool, note_path, read_note, write_note


class Notes:
    def __init__(self, files=None):
        self.files = dict(files or {})
        self.writes = []
        self.reads = []

    def write(self, path, data, *, if_none_match):
        assert if_none_match is True
        self.writes.append((path, data))
        if path in self.files:
            raise MountableError(412, "precondition_failed", "private detail")
        self.files[path] = data.encode()

    def read(self, path):
        self.reads.append(path)
        return type("File", (), {"data": self.files[path]})()


def test_write_then_read_uses_the_tool_call_and_no_model():
    store = Notes()
    agent = build_agent(store, "notes/brief.md")
    assert call_tool(agent, "write_note", {"text": "from-the-filesystem"}) == "stored"
    assert call_tool(agent, "read_note", {}) == "from-the-filesystem"
    assert agent.model.calls == 0
    assert store.writes == [("notes/brief.md", "from-the-filesystem")]
    assert store.reads == ["notes/brief.md"]


def test_fresh_process_reads_the_note_from_an_earlier_run():
    store = Notes()
    assert write_note(store, "notes/brief.md", "still here") == "stored"
    assert read_note(store, "notes/brief.md") == "still here"
    assert store.writes == [("notes/brief.md", "still here")]


@pytest.mark.parametrize(
    ("name", "arguments"),
    [
        ("write_note", {"text": 12}),
        ("write_note", {}),
        ("write_note", {"text": "hello", "path": "notes/other.md"}),
        ("read_note", {"text": "hello"}),
    ],
)
def test_schema_rejection_never_touches_the_store(name, arguments):
    store = Notes()
    agent = build_agent(store, "notes/brief.md")
    result = call_tool(agent, name, arguments)
    assert result.startswith("error:")
    assert store.writes == []
    assert store.reads == []


def test_existing_note_is_not_replaced():
    store = Notes({"notes/brief.md": b"already"})
    result = write_note(store, "notes/brief.md", "new")
    assert "precondition_failed" in result
    assert "private detail" not in result
    assert store.files["notes/brief.md"] == b"already"


def test_read_only_session_cannot_write():
    class ReadOnly(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(403, "read_only", "private session")

    store = ReadOnly({"notes/brief.md": b"already"})
    agent = build_agent(store, "notes/brief.md")
    result = call_tool(agent, "write_note", {"text": "new"})
    assert result == "error: read_only: this session cannot write."
    assert "private session" not in result
    assert store.files["notes/brief.md"] == b"already"
    assert store.reads == []


def test_oversized_note_is_refused_before_a_write():
    store = Notes()
    result = write_note(store, "notes/brief.md", "x" * (16_384 + 1))
    assert result == "error: note is larger than 16 KiB"
    assert store.writes == []


@pytest.mark.parametrize(
    "path",
    ["../secret.md", "/notes/brief.md", "notes/../brief.md", "notes/a.txt", "brief.md"],
)
def test_rejected_paths_never_build_a_tool(path):
    store = Notes()
    with pytest.raises(ValueError, match="notes/brief.md"):
        build_agent(store, path)
    assert store.writes == []
    assert store.reads == []


def test_note_path_accepts_a_plain_markdown_name():
    assert note_path("notes/brief.md") == "notes/brief.md"


def test_unknown_write_tells_the_caller_to_read_before_retrying():
    class Dropped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(None, "network_error", "connection reset")

    store = Dropped()
    result = write_note(store, "notes/brief.md", "hello")
    assert "Read the note before writing again" in result
    assert "connection reset" not in result
    assert store.reads == []


def test_spending_cap_stops_without_a_second_write():
    class Capped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(
                402, "filesystem_spending_cap_reached", "private balance"
            )

    store = Capped()
    result = write_note(store, "notes/brief.md", "hello")
    assert store.writes == [("notes/brief.md", "hello")]
    assert "filesystem_spending_cap_reached" in result
    assert "private balance" not in result
    assert store.reads == []
