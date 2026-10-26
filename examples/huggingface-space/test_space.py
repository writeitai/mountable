"""Tests the Space helpers against a fake filesystem. Nothing reaches mountable.io.

    python3 -m unittest test_space -v
"""

import unittest

import app


class FakeFile:
    def __init__(self, data: bytes):
        self.data = data


class NotFound(Exception):
    code = "not_found"


class FakeFS:
    def __init__(self):
        self.files = {}

    def write(self, path, text):
        self.files[path] = text.encode() if isinstance(text, str) else text

    def read(self, path):
        if path not in self.files:
            raise NotFound()
        return FakeFile(self.files[path])


class SpaceTest(unittest.TestCase):
    def test_save_then_read_survives_new_handle(self):
        fs = FakeFS()
        self.assertIn("Saved", app.save_note("hello from the agent", fs=fs))
        # The same store read back stands in for a Space restart.
        self.assertEqual(app.read_note(fs=fs), "hello from the agent\n")

    def test_empty_note_rejected(self):
        self.assertEqual(app.save_note("  ", fs=FakeFS()), "Write a note first.")

    def test_long_note_rejected(self):
        self.assertIn("Keep the note under", app.save_note("x" * 2001, fs=FakeFS()))

    def test_missing_note_message(self):
        self.assertEqual(
            app.read_note(fs=FakeFS()), "No note yet. Save one, then restart the Space."
        )


if __name__ == "__main__":
    unittest.main()
