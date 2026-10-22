"""Runs both notebooks against a local fake of the files API.

Nothing reaches Mountable. Needs mountable-sdk and marimo:

    (cd sdk/python && uv run --locked --with marimo==0.25.2 python ../../examples/notebooks/test_notebooks.py)
"""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import threading
import unittest
import zlib
from email.utils import formatdate
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import unquote, urlsplit

HERE = Path(__file__).resolve().parent
API_KEY = "mtbl_notebooktestsecret"
FS = "fs_notebooks"
PREFIX = f"/api/v1/filesystems/{FS}/"
UTM = "utm_source=data-science-and-ml-workspaces&utm_medium=notebook&utm_content="


class FakeAPI:
    """Token sessions, whole-file reads and writes, and listings."""

    def __init__(self) -> None:
        self.files: dict[str, bytes] = {}
        self.calls: list[str] = []
        api = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args: object) -> None:
                pass

            def _reply(self, status: int, body: bytes, headers: dict[str, str]) -> None:
                self.send_response(status)
                for name, value in headers.items():
                    self.send_header(name, value)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _json(self, status: int, value: object) -> None:
                body = json.dumps(value).encode()
                self._reply(status, body, {"Content-Type": "application/json"})

            def _serve(self) -> None:
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                path = urlsplit(self.path).path
                api.calls.append(f"{self.command} {path}")
                auth = self.headers.get("Authorization", "")
                if self.command == "POST" and path == "/api/v1/mount-sessions":
                    assert auth == f"Bearer {API_KEY}", auth
                    request = json.loads(body)
                    assert request["credential"] == "token", request
                    return self._json(
                        201,
                        {
                            "id": "s1",
                            "filesystem_id": FS,
                            "mode": request["mode"],
                            "state": "active",
                            "expires_at": "2999-01-01T00:00:00Z",
                            "token": "mtblst_notebooktoken",
                        },
                    )
                assert auth == "Bearer mtblst_notebooktoken", auth
                kind, _, rest = path.removeprefix(PREFIX).partition("/")
                name = "/" + unquote(rest)
                if kind == "files" and self.command == "PUT":
                    status = 200 if name in api.files else 201
                    api.files[name] = body
                    return self._json(status, info(name, body))
                if kind == "files" and self.command == "GET" and name in api.files:
                    data = api.files[name]
                    return self._reply(
                        200,
                        data,
                        {"ETag": etag(data), "Last-Modified": formatdate(usegmt=True)},
                    )
                if kind == "list" and self.command == "GET":
                    below = sorted(p for p in api.files if p.startswith(name + "/"))
                    if below:
                        return self._json(
                            200,
                            {
                                "entries": [info(p, api.files[p]) for p in below],
                                "next": None,
                            },
                        )
                self._json(
                    404, {"detail": {"code": "not_found", "message": "no such file"}}
                )

            do_GET = do_PUT = do_POST = do_DELETE = _serve

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self._server.server_address[1]}"
        threading.Thread(target=self._server.serve_forever, daemon=True).start()

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


def etag(data: bytes) -> str:
    return f'"{zlib.crc32(data):08x}"'


def info(name: str, data: bytes) -> dict[str, object]:
    return {
        "path": name,
        "type": "file",
        "size": len(data),
        "etag": etag(data),
        "modified_at": "2026-10-22T12:00:00Z",
    }


def run_quickstart() -> str:
    """Executes the notebook's code cells like a fresh kernel; returns stdout."""
    notebook = json.loads((HERE / "mountable_quickstart.ipynb").read_text())
    namespace: dict[str, object] = {"__name__": "__main__"}
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        for cell in notebook["cells"]:
            if cell["cell_type"] != "code":
                continue
            source = "".join(cell["source"])
            code = compile(
                source if not source.startswith("%") else "", "<cell>", "exec"
            )
            if "mount" in cell["metadata"].get("tags", []):
                continue  # needs FUSE and the CLI; compiled above
            exec(code, namespace)
    return out.getvalue()


class Notebooks(unittest.TestCase):
    def setUp(self) -> None:
        self.api = FakeAPI()
        self.addCleanup(self.api.close)
        env = {
            "MOUNTABLE_API_KEY": API_KEY,
            "MOUNTABLE_API_URL": self.api.url,
            "MOUNTABLE_FS_ID": FS,
        }
        for name in [*env, "MOUNTABLE_ORG_ID", "MOUNTABLE_WEBHOOK_URL"]:
            self.addCleanup(os.environ.pop, name, None)
            os.environ.pop(name, None)
        os.environ.update(env)

    def test_quickstart_resumes_after_a_kernel_restart(self) -> None:
        first = run_quickstart()
        self.assertIn("Saved datasets/houses.csv (200 rows)", first)
        self.assertIn("Computed features", first)
        self.assertIn("Starting at epoch 0 of 30", first)
        checkpoint = json.loads(self.api.files["/checkpoints/latest.json"])
        self.assertEqual(checkpoint["epoch"], 30)
        self.assertLess(checkpoint["loss"], 150)  # the noise alone is about 100

        self.api.files["/checkpoints/latest.json"] = json.dumps(
            {**checkpoint, "epoch": 12}
        ).encode()  # as if the kernel died after epoch 12
        second = run_quickstart()
        self.assertIn("Read datasets/houses.csv from Mountable", second)
        self.assertIn("Loaded cached features", second)
        self.assertIn("Starting at epoch 12 of 30", second)
        self.assertEqual(
            json.loads(self.api.files["/checkpoints/latest.json"])["epoch"], 30
        )
        self.assertNotIn(API_KEY, first + second)

    def test_marimo_saves_an_artifact_and_creates_no_webhook_unasked(self) -> None:
        sys.path.insert(0, str(HERE))
        self.addCleanup(sys.path.remove, str(HERE))
        from mountable_marimo import app

        app.run()
        summary = json.loads(self.api.files["/artifacts/weather.summary.json"])
        self.assertEqual(summary["dataset"], "/datasets/weather.csv")
        self.assertEqual(summary["rows"], 5)
        self.assertEqual(
            [c["column"] for c in summary["columns"]], ["day", "temp_c", "rain_mm"]
        )
        self.assertFalse([c for c in self.api.calls if "webhooks" in c])

    def test_notebooks_ship_clean_with_tagged_links(self) -> None:
        notebook = json.loads((HERE / "mountable_quickstart.ipynb").read_text())
        for cell in notebook["cells"]:
            if cell["cell_type"] == "code":
                self.assertEqual(cell["outputs"], [])
                self.assertIsNone(cell["execution_count"])
        for path, content in [
            ("mountable_quickstart.ipynb", "nb-01-quickstart"),
            ("mountable_marimo.py", "nb-02-marimo"),
        ]:
            text = (HERE / path).read_text()
            links = [
                part.split(")")[0] for part in text.split("https://mountable.io")[1:]
            ]
            self.assertTrue(links, path)
            for link in links:
                self.assertIn(UTM + content, link, path)


if __name__ == "__main__":
    unittest.main()
