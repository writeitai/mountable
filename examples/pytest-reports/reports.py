"""Upload a closed pytest JUnit report so a fresh process can read it.

``run`` executes the synthetic suite in ``sample_tests/``, writes the JUnit
XML and a short failure note to ``--dir``, then uploads both closed files.
It returns pytest's exit code even when the upload fails, so a failing test
still fails the calling job. ``check`` is a fresh process: it downloads both
files read-only, parses the JUnit XML and compares SHA-256 with ``--dir``.
``cleanup`` deletes both files. Credentials stay in the caller's secrets;
the API key is never printed.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import subprocess
import sys
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Protocol

MAX_BYTES = 1_048_576
_RUN = re.compile(r"[a-z0-9][a-z0-9-]{0,39}")
ROOT = Path(__file__).parent


class FileLike(Protocol):
    @property
    def data(self) -> bytes: ...


class Store(Protocol):
    def write(self, path: str, data: bytes, *, if_none_match: bool) -> object: ...
    def read(self, path: str) -> FileLike: ...
    def delete(self, path: str) -> None: ...


class RecipeError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


def report_paths(run: str) -> dict[str, str]:
    if not isinstance(run, str) or _RUN.fullmatch(run) is None:
        raise RecipeError(
            "bad_run",
            "The run id must be 1 to 40 lowercase letters, digits or hyphens, "
            "and start with a letter or digit.",
        )
    return {
        "junit": f"pytest/{run}/junit.xml",
        "failure": f"pytest/{run}/failure.txt",
    }


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def parse_junit(data: bytes) -> tuple[int, int]:
    """Return (tests, failures+errors); reject anything that is not JUnit."""
    try:
        root = ET.fromstring(data)
    except ET.ParseError as exc:
        raise RecipeError("bad_junit", "The JUnit file does not parse.") from exc
    suites = [root] if root.tag == "testsuite" else list(root.iter("testsuite"))
    if not suites:
        raise RecipeError("bad_junit", "The JUnit file has no testsuite.")
    tests = sum(int(s.get("tests", "0")) for s in suites)
    bad = sum(int(s.get("failures", "0")) + int(s.get("errors", "0")) for s in suites)
    return tests, bad


def failure_note(junit: bytes, exit_code: int) -> bytes:
    tests, bad = parse_junit(junit)
    text = (
        f"pytest exit code: {exit_code}\n"
        f"tests: {tests}\nfailures and errors: {bad}\n"
        "Synthetic suite: test_addition_fails_on_purpose fails on purpose.\n"
    )
    return text.encode()


def _bounded(name: str, data: bytes) -> bytes:
    if len(data) > MAX_BYTES:
        raise RecipeError("too_large", f"The {name} is larger than 1 MiB.")
    return data


def upload_reports(
    store: Store, run: str, junit: bytes, failure: bytes
) -> dict[str, str]:
    paths = report_paths(run)
    out = {}
    for name, data in (("junit", junit), ("failure", failure)):
        _bounded(name, data)
        store.write(paths[name], data, if_none_match=True)
        out[name] = f"{paths[name]} sha256={sha256(data)} bytes={len(data)}"
    return out


def check_reports(store: Store, run: str, local: Path) -> list[str]:
    paths = report_paths(run)
    lines = []
    for name, filename in (("junit", "junit.xml"), ("failure", "failure.txt")):
        remote = store.read(paths[name]).data
        local_bytes = (local / filename).read_bytes()
        if remote != local_bytes:
            raise RecipeError("mismatch", f"The {name} bytes do not match.")
        lines.append(f"match {paths[name]} sha256={sha256(remote)} bytes={len(remote)}")
    tests, bad = parse_junit((local / "junit.xml").read_bytes())
    lines.append(f"junit tests={tests} failures_and_errors={bad}")
    return lines


def cleanup_reports(store: Store, run: str) -> list[str]:
    paths = report_paths(run)
    done = []
    for name in ("junit", "failure"):
        try:
            store.delete(paths[name])
        except Exception as exc:
            if getattr(exc, "status", None) != 404:
                raise
        done.append(f"deleted {paths[name]}")
    return done


def run_suite(out_dir: Path) -> tuple[int, bytes, bytes]:
    out_dir.mkdir(parents=True, exist_ok=True)
    junit_path = out_dir / "junit.xml"
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "pytest",
            str(ROOT / "sample_tests"),
            f"--junitxml={junit_path}",
            "-q",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    sys.stdout.write(proc.stdout)
    sys.stderr.write(proc.stderr)
    junit = junit_path.read_bytes()
    parse_junit(junit)
    note = failure_note(junit, proc.returncode)
    (out_dir / "failure.txt").write_bytes(note)
    return proc.returncode, junit, note


def _store(mode: str):  # pragma: no cover - live path, offline tests inject fakes
    from mountable_sdk import Mountable

    fs_id = _arg_filesystem()
    return Mountable().filesystem(fs_id, mode)  # type: ignore[arg-type]


_FS_ID = ""


def _arg_filesystem() -> str:
    if not _FS_ID:
        raise RecipeError("bad_filesystem", "Pass --filesystem FS_ID.")
    return _FS_ID


def main(argv: list[str] | None = None) -> int:
    global _FS_ID
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("run", "check", "cleanup"):
        p = sub.add_parser(name)
        p.add_argument("--filesystem", required=True)
        p.add_argument("--run", required=True)
        p.add_argument("--dir", default="out")
    args = parser.parse_args(argv)
    _FS_ID = args.filesystem
    out_dir = Path(args.dir)
    try:
        report_paths(args.run)
        if args.command == "run":
            code, junit, note = run_suite(out_dir)
            try:
                for line in upload_reports(
                    _store("rw"), args.run, junit, note
                ).values():
                    print(line)
            except Exception as exc:  # noqa: BLE001 - preserve pytest exit code
                print(f"upload_failed: {exc}", file=sys.stderr)
            return code
        if args.command == "check":
            for line in check_reports(_store("ro"), args.run, out_dir):
                print(line)
            return 0
        for line in cleanup_reports(_store("rw"), args.run):
            print(line)
        return 0
    except RecipeError as exc:
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
