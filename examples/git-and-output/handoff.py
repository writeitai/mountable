"""Keep source history in Git and hand a generated report to a fresh worker."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

from mountable_sdk import Filesystem, Mountable, MountableError

MAX_BYTES = 4096
RUN = re.compile(r"[a-z0-9][a-z0-9-]{0,39}")
SHA256 = re.compile(r"[a-f0-9]{64}")
SOURCE_BEFORE = "def total(values):\n    return sum(values)\n"
SOURCE_AFTER = (
    "def total(values):\n    return sum(value for value in values if value >= 0)\n"
)


def remote_path(run: str) -> str:
    if RUN.fullmatch(run) is None:
        raise ValueError("run must be 1-40 lowercase letters, digits or hyphens")
    return f"git-output-{run}.json"


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def validate(data: bytes) -> dict:
    if len(data) > MAX_BYTES:
        raise ValueError("report exceeds 4096 bytes")
    report = json.loads(data)
    if (
        not isinstance(report, dict)
        or set(report) != {"source_commit", "check", "result"}
        or not isinstance(report["source_commit"], str)
        or re.fullmatch(r"[a-f0-9]{40}", report["source_commit"]) is None
        or report["check"] != "total([3, -2, 4]) == 7"
        or type(report["result"]) is not int
        or report["result"] != 7
    ):
        raise ValueError("unexpected report schema or result")
    return report


def git_demo(report_path: Path) -> None:
    # A disposable repository on LOCAL disk. No mounts, remote writes or hooks.
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update(
        GIT_CONFIG_NOSYSTEM="1",
        GIT_CONFIG_GLOBAL=os.devnull,
        GIT_AUTHOR_DATE="2026-01-01T00:00:00Z",
        GIT_COMMITTER_DATE="2026-01-01T00:00:00Z",
    )
    with tempfile.TemporaryDirectory(prefix="git-output-") as temporary:
        root = Path(temporary)

        def git(*args: str) -> str:
            return subprocess.check_output(
                [
                    "git",
                    "-c",
                    "user.name=Example",
                    "-c",
                    "user.email=example@example.invalid",
                    "-c",
                    "commit.gpgsign=false",
                    "-c",
                    f"core.hooksPath={os.devnull}",
                    "-c",
                    "core.autocrlf=false",
                    *args,
                ],
                cwd=root,
                env=env,
                text=True,
            )

        git("init", "-q", "--initial-branch=main", "--object-format=sha1")
        (root / "total.py").write_text(SOURCE_BEFORE)
        git("add", "total.py")
        git("commit", "-qm", "Add total")
        git("switch", "-qc", "ignore-negative")
        (root / "total.py").write_text(SOURCE_AFTER)
        print("$ git diff -- total.py")
        print(git("diff", "--no-ext-diff", "--", "total.py"), end="")
        git("add", "total.py")
        git("commit", "-qm", "Ignore negative values")
        print("$ git log --format=%s -2")
        print(git("log", "--format=%s", "-2"), end="")
        commit = git("rev-parse", "HEAD").strip()
        result = int(
            subprocess.check_output(
                [
                    sys.executable,
                    "-c",
                    "from total import total; print(total([3, -2, 4]))",
                ],
                cwd=root,
                text=True,
            )
        )
        data = (
            json.dumps(
                {
                    "source_commit": commit,
                    "check": "total([3, -2, 4]) == 7",
                    "result": result,
                },
                sort_keys=True,
            )
            + "\n"
        ).encode()
        validate(data)
        # Refuse an existing local report too.
        with report_path.open("xb") as output:
            output.write(data)
        print(f"report bytes={len(data)} sha256={digest(data)}")
        print(data.decode(), end="")


def upload(fs: Filesystem, run: str, data: bytes) -> None:
    validate(data)
    path = remote_path(run)
    fs.write(path, data, if_none_match=True)
    print(f"stored {path} bytes={len(data)} sha256={digest(data)}")


def checked_read(fs: Filesystem, run: str, expected: str):
    if SHA256.fullmatch(expected) is None:
        raise ValueError("sha256 must be 64 lowercase hex characters")
    file = fs.read(remote_path(run))
    validate(file.data)
    if digest(file.data) != expected:
        raise ValueError("report checksum differs; file left unchanged")
    return file


def cleanup(fs: Filesystem, run: str, expected: str) -> None:
    file = checked_read(fs, run, expected)
    if not file.info.etag:
        raise ValueError("missing ETag; file left unchanged")
    fs.delete(remote_path(run), if_match=file.info.etag)
    print(f"deleted {remote_path(run)}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    demo = sub.add_parser("git-demo")
    demo.add_argument("--report", type=Path, required=True)
    for name in ("upload", "read", "cleanup"):
        command = sub.add_parser(name)
        command.add_argument("--filesystem", required=True)
        command.add_argument("--run", required=True)
        if name == "upload":
            command.add_argument("--report", type=Path, required=True)
        else:
            command.add_argument("--sha256", required=True)
    args = parser.parse_args()
    try:
        if args.command == "git-demo":
            git_demo(args.report)
            return 0
        remote_path(args.run)
        # The trusted job chooses the filesystem and run, never model output.
        with Mountable() as client:
            fs = client.filesystem(
                args.filesystem, "ro" if args.command == "read" else "rw"
            )
            if args.command == "upload":
                with args.report.open("rb") as source:
                    data = source.read(MAX_BYTES + 1)
                upload(fs, args.run, data)
            elif args.command == "read":
                file = checked_read(fs, args.run, args.sha256)
                print(
                    f"match {remote_path(args.run)} bytes={len(file.data)} sha256={digest(file.data)}"
                )
                print(file.data.decode(), end="")
            else:
                cleanup(fs, args.run, args.sha256)
        return 0
    except MountableError as error:
        print(f"{error.status} {error.code}", file=sys.stderr)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"{type(error).__name__}: {error}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
