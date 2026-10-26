"""Offline checks: real LangGraph nodes, a fake filesystem, no network."""

from types import SimpleNamespace

import pytest
from mountable_sdk import MountableError

from graph import build_graphs, note_path, run_graph


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


def test_handoff_reads_the_filesystem_not_the_checkpoint_text():
    store = Notes(read_data=b"from-the-filesystem")
    handoff, _resume = build_graphs(store)
    _thread, result = run_graph(
        handoff, {"path": "notes/brief.md", "text": "planned body"}
    )
    assert store.writes == [("notes/brief.md", "planned body")]
    assert store.reads == ["notes/brief.md"]
    assert result["text"] == "from-the-filesystem"
    assert result["error"] == ""


def test_read_node_is_entered_with_empty_text():
    store = Notes()
    handoff, _resume = build_graphs(store)
    thread_id, result = run_graph(
        handoff, {"path": "notes/brief.md", "text": "planned body"}
    )
    history = list(
        handoff.get_state_history({"configurable": {"thread_id": thread_id}})
    )
    reader_steps = [step for step in history if "read_note" in step.next]
    assert reader_steps
    assert all(step.values.get("text") == "" for step in reader_steps)
    assert result["text"] == "planned body"


def test_fresh_checkpointer_reads_the_note_from_an_earlier_run():
    store = Notes()
    handoff, _first_resume = build_graphs(store)
    run_graph(handoff, {"path": "notes/brief.md", "text": "still here"})
    _handoff, resume = build_graphs(store)
    _thread, result = run_graph(resume, {"path": "notes/brief.md"})
    assert result["text"] == "still here"
    assert result["error"] == ""
    assert store.writes == [("notes/brief.md", "still here")]


@pytest.mark.parametrize(
    "path",
    ["../secret.md", "/notes/brief.md", "notes/../brief.md", "notes/a.txt", "brief.md"],
)
def test_rejected_paths_never_touch_the_store(path):
    store = Notes()
    handoff, resume = build_graphs(store)
    for graph, state in (
        (handoff, {"path": path, "text": "x"}),
        (resume, {"path": path}),
    ):
        _thread, result = run_graph(graph, state)
        assert result["error"]
        assert result["text"] == ""
    assert store.writes == []
    assert store.reads == []


def test_oversized_note_is_refused_before_a_write():
    store = Notes()
    handoff, _resume = build_graphs(store)
    _thread, result = run_graph(
        handoff, {"path": "notes/brief.md", "text": "x" * (16_384 + 1)}
    )
    assert result["error"] == "note is larger than 16 KiB"
    assert store.writes == []


def test_spending_cap_stops_without_a_second_write():
    class Capped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(
                402, "filesystem_spending_cap_reached", "private balance"
            )

    store = Capped()
    handoff, _resume = build_graphs(store)
    _thread, result = run_graph(handoff, {"path": "notes/brief.md", "text": "hello"})
    assert store.writes == [("notes/brief.md", "hello")]
    assert store.reads == []
    assert "filesystem_spending_cap_reached" in result["error"]
    assert "private balance" not in result["error"]
    assert result["text"] == ""


def test_unknown_read_does_not_talk_about_writing_again():
    class Dropped(Notes):
        def read(self, path):
            self.reads.append(path)
            raise MountableError(None, "network_error", "connection reset")

    store = Dropped()
    _handoff, resume = build_graphs(store)
    _thread, result = run_graph(resume, {"path": "notes/brief.md"})
    assert result["error"] == "network_error: outcome unknown."
    assert "writing" not in result["error"]


def test_unknown_write_tells_the_caller_to_read_before_retrying():
    class Dropped(Notes):
        def write(self, path, data, *, if_none_match):
            self.writes.append((path, data))
            raise MountableError(None, "network_error", "connection reset")

    store = Dropped()
    handoff, _resume = build_graphs(store)
    _thread, result = run_graph(handoff, {"path": "notes/brief.md", "text": "hello"})
    assert "outcome unknown" in result["error"]
    assert store.reads == []


def test_existing_note_is_not_replaced():
    store = Notes({"notes/brief.md": b"already"})
    handoff, _resume = build_graphs(store)
    _thread, result = run_graph(handoff, {"path": "notes/brief.md", "text": "new"})
    assert "precondition_failed" in result["error"]
    assert store.files["notes/brief.md"] == b"already"
    assert store.reads == []


def test_resume_reports_a_read_error_without_the_server_message():
    class Missing(Notes):
        def read(self, path):
            self.reads.append(path)
            raise MountableError(404, "not_found", "private path detail")

    store = Missing()
    _handoff, resume = build_graphs(store)
    _thread, result = run_graph(resume, {"path": "notes/brief.md"})
    assert result["error"] == "not_found"
    assert result["text"] == ""


def test_note_path_accepts_a_plain_markdown_name():
    assert note_path("notes/brief.md") == "notes/brief.md"
