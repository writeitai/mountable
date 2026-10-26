"""Offline checks: a real CrewAI crew, a fake filesystem, no network."""

from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from crew import note_path, run_crew


class Notes:
    def __init__(self, files=None, read_data=None):
        self.files = dict(files or {})
        self.read_data = read_data
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
        if self.read_data is not None:
            return SimpleNamespace(data=self.read_data)
        return SimpleNamespace(data=self.files[path])


def test_handoff_reads_the_filesystem_not_the_writer_answer():
    store = Notes(read_data=b"from-the-filesystem")
    result = run_crew(store, "notes/brief.md", "planned body", read_only=False)
    assert store.writes == [("notes/brief.md", "planned body")]
    assert store.reads == ["notes/brief.md"]
    assert result == "from-the-filesystem"


def test_fresh_crew_reads_the_note_from_an_earlier_run():
    store = Notes()
    run_crew(store, "notes/brief.md", "still here", read_only=False)
    result = run_crew(store, "notes/brief.md", None, read_only=True)
    assert result == "still here"
    assert store.writes == [("notes/brief.md", "still here")]
    assert store.reads == ["notes/brief.md", "notes/brief.md"]


@pytest.mark.parametrize(
    "path",
    ["../secret.md", "/notes/brief.md", "notes/../brief.md", "notes/a.txt", "brief.md"],
)
def test_rejected_paths_never_touch_the_store(path):
    store = Notes()
    handoff = run_crew(store, path, "x", read_only=False)
    resume = run_crew(store, path, None, read_only=True)
    assert handoff.startswith("error:")
    assert resume.startswith("error:")
    assert store.writes == []
    assert store.reads == []


def test_oversized_note_is_refused_before_a_write():
    store = Notes()
    result = run_crew(store, "notes/brief.md", "x" * (16_384 + 1), read_only=False)
    assert result == "error: note is larger than 16 KiB"
    assert store.writes == []
    assert store.reads == []


def test_spending_cap_stops_without_a_second_write_or_a_read():
    class Capped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(
                402, "filesystem_spending_cap_reached", "private balance"
            )

    store = Capped()
    result = run_crew(store, "notes/brief.md", "hello", read_only=False)
    assert store.writes == [("notes/brief.md", "hello")]
    assert store.reads == []
    assert "filesystem_spending_cap_reached" in result
    assert "private balance" not in result


def test_unknown_read_does_not_talk_about_writing_again():
    class Dropped(Notes):
        def read(self, path):
            self.reads.append(path)
            raise MountableError(None, "network_error", "connection reset")

    store = Dropped({"notes/brief.md": b"already"})
    result = run_crew(store, "notes/brief.md", None, read_only=True)
    assert result == "error: network_error: outcome unknown."
    assert "writing" not in result


def test_unknown_write_tells_the_caller_to_read_before_retrying():
    class Dropped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(None, "network_error", "connection reset")

    store = Dropped()
    result = run_crew(store, "notes/brief.md", "hello", read_only=False)
    assert "outcome unknown" in result
    assert "Read the note before writing again" in result
    assert store.reads == []


def test_existing_note_is_not_replaced():
    store = Notes({"notes/brief.md": b"already"})
    result = run_crew(store, "notes/brief.md", "new", read_only=False)
    assert "precondition_failed" in result
    assert store.files["notes/brief.md"] == b"already"
    assert store.reads == []


def test_resume_reports_a_read_error_without_the_server_message():
    class Missing(Notes):
        def read(self, path):
            self.reads.append(path)
            raise MountableError(404, "not_found", "private path detail")

    store = Missing()
    result = run_crew(store, "notes/brief.md", None, read_only=True)
    assert result == "error: not_found"


def test_note_path_accepts_a_plain_markdown_name():
    assert note_path("notes/brief.md") == "notes/brief.md"
