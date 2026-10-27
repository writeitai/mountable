"""Real DSPy and released SDK over a fake Files API; no model or cloud calls."""

import json
import os
import subprocess
import sys
import threading
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from mountable_sdk import Mountable, MountableError

import evaluation as recipe

RUN = "0123456789abcdef"
BASE = recipe.prefix(RUN)


@contextmanager
def api(fail_upload=None):
    files, requests = {}, []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            self.handle_request()

        def do_PUT(self):
            self.handle_request()

        def do_GET(self):
            self.handle_request()

        def do_DELETE(self):
            self.handle_request()

        def respond(self, status, data=b"", **headers):
            self.send_response(status)
            self.send_header("Content-Length", str(len(data)))
            for key, value in headers.items():
                self.send_header(key.replace("_", "-"), value)
            self.end_headers()
            self.wfile.write(data)

        def error(self, status, code):
            self.respond(status, json.dumps({"detail": {"code": code}}).encode())

        def handle_request(self):
            data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            requests.append((self.command, self.path, dict(self.headers), data))
            if self.path == "/api/v1/mount-sessions":
                body = json.loads(data)
                assert body["filesystem_id"] == "fs_fixture"
                self.respond(
                    201,
                    recipe.encode(
                        {
                            "token": "mtblst_" + body["mode"],
                            "expires_at": "2099-01-01T00:00:00Z",
                        }
                    ),
                )
                return
            path = self.path.split("/files/", 1)[-1]
            if self.command in ("PUT", "DELETE") and self.headers[
                "Authorization"
            ].endswith("_ro"):
                self.error(403, "read_only")
                return
            if self.command == "PUT":
                assert self.headers["If-None-Match"] == "*"
                if path.endswith(str(fail_upload)):
                    self.error(503, "unavailable")
                elif path in files:
                    self.error(412, "precondition_failed")
                else:
                    files[path] = data
                    self.respond(
                        201,
                        recipe.encode(
                            {
                                "path": path,
                                "type": "file",
                                "size": len(data),
                                "etag": recipe.digest(data),
                                "modified_at": "2026-10-27T00:00:00Z",
                            }
                        ),
                    )
                return
            if self.command == "DELETE":
                if path in files:
                    assert (
                        self.headers["If-Match"]
                        == '"' + recipe.digest(files[path]) + '"'
                    )
                    del files[path]
                self.respond(204)
                return
            if path not in files:
                self.error(404, "not_found")
                return
            self.respond(
                200,
                files[path],
                ETag='"' + recipe.digest(files[path]) + '"',
                Last_Modified="Tue, 27 Oct 2026 00:00:00 GMT",
            )

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_port}"
    try:
        with Mountable(api_key="mtbl_test_only", base_url=url) as client:
            yield client, files, requests, url
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def test_real_evaluate_and_fresh_process_reader():
    with api() as (client, files, requests, url):
        env = {
            **os.environ,
            "MOUNTABLE_API_KEY": "mtbl_test_only",
            "MOUNTABLE_API_URL": url,
            "MOUNTABLE_FILESYSTEM_ID": "fs_fixture",
        }

        def command(*args):
            return subprocess.run(
                [sys.executable, str(Path(recipe.__file__)), *args],
                env=env,
                capture_output=True,
                text=True,
                timeout=60,
            )

        wrote = command("write", "--run", RUN)
        assert wrote.returncode == 0, wrote.stderr
        sha = wrote.stdout.strip().split("manifest_sha256=")[-1]
        assert sum(map(len, files.values())) < recipe.MAX_BYTES
        puts = [r for r in requests if r[0] == "PUT"]
        assert puts[-1][1].endswith("manifest.json")
        read = command("read", "--run", RUN, "--manifest-sha256", sha)
        assert read.returncode == 0, read.stderr
        assert read.stdout == "match cases=3 score_percent=66.67\n"
        again = command("write", "--run", RUN)
        assert again.returncode != 0 and "precondition_failed" in again.stderr
        denied = command("ro-write", "--run", RUN)
        assert denied.returncode != 0 and "read_only:" in denied.stderr
        assert recipe.verify(client.filesystem("fs_fixture", "ro"), RUN, sha) == 66.67
        assert command("cleanup", "--run", RUN).returncode == 0
        assert not files
        gone = command("read", "--run", RUN, "--manifest-sha256", sha)
        assert gone.returncode != 0 and "not_found" in gone.stderr


@pytest.mark.parametrize("name", (*recipe.FILES, "manifest.json"))
@pytest.mark.parametrize("change", ("missing", "corrupt"))
def test_missing_and_changed_records_refused(name, change):
    with api() as (client, files, _, _url):
        fs = client.filesystem("fs_fixture")
        sha = recipe.publish(fs, RUN)
        path = f"{BASE}/{name}"
        if change == "missing":
            del files[path]
        else:
            files[path] += b" "
        with pytest.raises((ValueError, MountableError)):
            recipe.verify(fs, RUN, sha)


@pytest.mark.parametrize("name", (*recipe.FILES, "manifest.json"))
def test_upload_failure_never_completes_bundle(name):
    with api(fail_upload=name) as (client, files, _, _url):
        fs = client.filesystem("fs_fixture")
        with pytest.raises(MountableError):
            recipe.publish(fs, RUN)
        assert f"{BASE}/manifest.json" not in files
        recipe.cleanup(fs, RUN)
        assert not files


@pytest.mark.parametrize(
    "change", ("paths", "oversize", "bool-size", "version", "list")
)
def test_manifest_schema_refused_before_reading_data(change):
    with api() as (client, files, requests, _url):
        fs = client.filesystem("fs_fixture")
        recipe.publish(fs, RUN)
        manifest = json.loads(files[f"{BASE}/manifest.json"])
        if change == "paths":
            manifest["files"]["../outside"] = manifest["files"].pop(recipe.FILES[0])
        elif change == "oversize":
            manifest["files"][recipe.FILES[0]]["size"] = recipe.MAX_BYTES
        elif change == "bool-size":
            manifest["files"][recipe.FILES[0]]["size"] = True
        elif change == "version":
            manifest["dspy"] = "other"
        else:
            manifest = []
        raw = recipe.encode(manifest)
        files[f"{BASE}/manifest.json"] = raw
        requests.clear()
        with pytest.raises(ValueError):
            recipe.verify(fs, RUN, recipe.digest(raw))
        assert len(requests) == 1


@pytest.mark.parametrize(
    "change", ("score", "metric", "prediction", "duplicate", "schema")
)
def test_rehashed_bad_results_refused(change):
    with api() as (client, files, _, _url):
        fs = client.filesystem("fs_fixture")
        recipe.publish(fs, RUN)
        results = json.loads(files[f"{BASE}/results.json"])
        if change == "score":
            results["score_percent"] = 100.0
        elif change == "metric":
            results["cases"][2]["match"] = True
        elif change == "prediction":
            results["cases"][2]["prediction"] = "8"
        elif change == "duplicate":
            results["cases"][2] = results["cases"][1]
        else:
            results = []
        raw = recipe.encode(results)
        files[f"{BASE}/results.json"] = raw
        manifest = json.loads(files[f"{BASE}/manifest.json"])
        manifest["files"]["results.json"] = {
            "size": len(raw),
            "sha256": recipe.digest(raw),
        }
        raw = recipe.encode(manifest)
        files[f"{BASE}/manifest.json"] = raw
        with pytest.raises(ValueError):
            recipe.verify(fs, RUN, recipe.digest(raw))


def test_bad_run_never_makes_a_request():
    with api() as (client, _, requests, _url):
        with pytest.raises(ValueError):
            recipe.publish(client.filesystem("fs_fixture"), "../another-user")
        assert not requests
