#!/usr/bin/env python3
"""Hand two tiny files between already-mounted Linux and macOS filesystems."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import time

RESULT = b"Agent result: 3 checks passed.\n"
REPLY = b"Developer reply: reviewed.\n"


def wait_for(check, description: str, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            print(description, flush=True)
            return
        time.sleep(0.2)
    raise TimeoutError(f"no confirmation within {timeout:g}s: {description}")


def matches(path: Path, expected: bytes) -> bool:
    try:
        return path.read_bytes() == expected
    except FileNotFoundError:
        return False


def write(path: Path, content: bytes) -> None:
    with path.open("xb") as output:
        output.write(content)
        output.flush()
        os.fsync(output.fileno())


def run(role: str, root: Path, run_id: str, timeout: float) -> None:
    directory = root / f"handoff-{run_id}"
    result = directory / "result.txt"
    reviewed = directory / "reviewed.txt"
    reply = directory / "reply.txt"
    accepted = directory / "accepted.txt"
    if role == "linux":
        directory.mkdir()  # A fresh run ID prevents overwriting earlier work.
        write(result, RESULT)
        print("Linux wrote result.txt and fsynced it", flush=True)
        wait_for(
            lambda: not result.exists() and matches(reviewed, RESULT),
            "Linux saw the Mac rename with identical result bytes",
            timeout,
        )
        reviewed.unlink()
        wait_for(
            lambda: matches(reply, REPLY), "Linux read the Mac reply bytes", timeout
        )
        reply.rename(accepted)
        wait_for(
            lambda: not accepted.exists(),
            "Linux saw the Mac delete accepted.txt",
            timeout,
        )
    else:
        wait_for(
            lambda: matches(result, RESULT), "Mac read the Linux result bytes", timeout
        )
        result.rename(reviewed)
        wait_for(
            lambda: not reviewed.exists(), "Mac saw Linux delete reviewed.txt", timeout
        )
        write(reply, REPLY)
        print("Mac wrote reply.txt and fsynced it", flush=True)
        wait_for(
            lambda: not reply.exists() and matches(accepted, REPLY),
            "Mac saw the Linux rename with identical reply bytes",
            timeout,
        )
        accepted.unlink()
    print(f"{role}: handoff complete", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("role", choices=("linux", "mac"))
    parser.add_argument("mountpoint", type=Path)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--timeout", type=float, default=20)
    args = parser.parse_args()
    if not re.fullmatch(r"[a-z0-9-]{1,40}", args.run_id):
        parser.error("--run-id must contain 1–40 lowercase letters, digits or hyphens")
    if not 1 <= args.timeout <= 60:
        parser.error("--timeout must be between 1 and 60 seconds")
    run(args.role, args.mountpoint, args.run_id, args.timeout)
