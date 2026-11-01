"""Refuse a stale edit of one task file, then write again on purpose.

Two editors read version A. Editor one writes version B with that ETag.
Editor two still holds A's ETag, so its write is refused and B stays.
Editor two reads the file again and writes version C by setting only the
field it owns. Nothing here merges the two proposed copies.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from typing import Any

from mountable_sdk import Filesystem, Mountable, MountableError

RUN_CHARS = set("abcdefghijklmnopqrstuvwxyz0123456789-")
EDITORS = {"one", "two"}
VERSION_A = b"status: open\nnote: ready\n"
VERSION_B = b"status: review\nnote: ready\n"
VERSION_C = b"status: review\nnote: ready\neditor: two\n"


class RecipeError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


def task_path(run: str) -> str:
    if (
        not run
        or len(run) > 40
        or run[0] == "-"
        or any(char not in RUN_CHARS for char in run)
    ):
        raise RecipeError(
            "bad_run",
            "The run id must be 1-40 characters: a lowercase letter or digit, then letters, digits or hyphens.",
        )
    return f"etag-edit-{run}.txt"


def quoted(etag: str) -> str:
    value = etag.strip()
    if value.startswith('"') or value.startswith("W/"):
        return value
    return f'"{value}"'


def parse(data: bytes) -> dict[str, str]:
    try:
        text = data.decode()
    except UnicodeError as exc:
        raise RecipeError("bad_file", "The task file is not UTF-8.") from exc
    fields: dict[str, str] = {}
    if not text.endswith("\n"):
        raise RecipeError("bad_file", "The task file has an unexpected shape.")
    for line in text.splitlines():
        key, separator, value = line.partition(": ")
        if not separator or key in fields:
            raise RecipeError("bad_file", "The task file has an unexpected shape.")
        fields[key] = value
    if set(fields) - {"status", "note", "editor"}:
        raise RecipeError("bad_file", "The task file has an unexpected shape.")
    if fields.get("note") != "ready" or fields.get("status") not in {"open", "review"}:
        raise RecipeError("bad_file", "The task file has an unexpected shape.")
    if "editor" in fields and fields["editor"] != "two":
        raise RecipeError("bad_file", "The task file has an unexpected shape.")
    return fields


def render(fields: dict[str, str]) -> bytes:
    lines = [f"status: {fields['status']}", f"note: {fields['note']}"]
    if "editor" in fields:
        lines.append(f"editor: {fields['editor']}")
    return ("".join(line + "\n" for line in lines)).encode()


def version_name(fields: dict[str, str]) -> str | None:
    if fields == {"status": "open", "note": "ready"}:
        return "A"
    if fields == {"status": "review", "note": "ready"}:
        return "B"
    if fields == {"status": "review", "note": "ready", "editor": "two"}:
        return "C"
    return None


def proposal_from_a(editor: str) -> bytes:
    """The whole file this editor would write from the copy of A it remembers."""
    if editor == "one":
        return VERSION_B
    if editor == "two":
        return b"status: open\nnote: ready\neditor: two\n"
    raise RecipeError("bad_editor", "The editor must be one or two.")


def apply_owned(fields: dict[str, str], editor: str) -> dict[str, str]:
    """Copy the file just read and set only this editor's field.

    Editor one owns status. Editor two owns editor. Other fields stay as
    they were read. This is not a merge of two proposed documents.
    """
    updated = dict(fields)
    if editor == "one":
        updated["status"] = "review"
    elif editor == "two":
        updated["editor"] = "two"
    else:
        raise RecipeError("bad_editor", "The editor must be one or two.")
    return updated


def show(path: str, etag: str, data: bytes) -> None:
    fields = parse(data)
    print(f"version={version_name(fields) or 'unknown'}")
    print(f"path={path}")
    print(f"etag={etag}")
    print(f"bytes={len(data)}")
    print("---")
    print(data.decode(), end="")


def seed(fs: Filesystem, run: str) -> Any:
    path = task_path(run)
    return fs.write(path, VERSION_A, if_none_match=True)


def commit(fs: Filesystem, run: str, etag: str, editor: str) -> Any:
    return fs.write(task_path(run), proposal_from_a(editor), if_match=etag)


def reconcile(fs: Filesystem, run: str, editor: str) -> Any:
    path = task_path(run)
    current = fs.read(path)
    if not current.info.etag:
        raise RecipeError("missing_etag", "The file has no ETag.")
    updated = apply_owned(parse(current.data), editor)
    return fs.write(path, render(updated), if_match=current.info.etag)


def cleanup(fs: Filesystem, run: str, etag: str) -> str:
    path = task_path(run)
    current = fs.read(path)
    if not current.info.etag or quoted(current.info.etag) != quoted(etag):
        raise RecipeError("stale", "The ETag changed. The file was left in place.")
    if version_name(parse(current.data)) is None:
        raise RecipeError(
            "unexpected",
            "The file is not a known task version. It was left in place.",
        )
    fs.delete(path, if_match=current.info.etag)
    return path


def _open(filesystem: str, mode: str) -> tuple[Mountable, Filesystem]:
    client = Mountable()
    return client, client.filesystem(filesystem, mode)


def _print_account(label: str, filesystem: str) -> None:
    with Mountable() as client:
        info = client.get_filesystem(filesystem)
        usage = client.filesystem_usage(filesystem)
    print(
        f"limits {label} spending_cap_cents={info.spending_cap_cents} "
        f"byte_quota={info.byte_quota} entry_quota={info.entry_quota} "
        f"mutations_per_minute={info.mutations_per_minute}",
        file=sys.stderr,
    )
    print(
        f"usage {label} retained_bytes={usage.retained_bytes} "
        f"entries={usage.entries} bytes_read_30d={usage.bytes_read_30d} "
        f"bytes_written_30d={usage.bytes_written_30d} "
        f"reserved_cents={usage.reserved_cents} currency={usage.currency}",
        file=sys.stderr,
    )


def _session_ids(filesystem: str) -> set[str]:
    with Mountable() as client:
        return {item.id for item in client.list_mount_sessions(filesystem)}


def _revoke_new(filesystem: str, before: set[str]) -> int:
    with Mountable() as client:
        fresh = [
            item.id
            for item in client.list_mount_sessions(filesystem)
            if item.id not in before
        ]
        for session_id in fresh:
            client.revoke_mount_session(session_id)
    return len(fresh)


def _spawn(args: list[str]) -> subprocess.CompletedProcess[str]:
    print("$ python edit.py " + " ".join(args))
    result = subprocess.run(
        [sys.executable, str(__file__), *args],
        cwd=os.path.dirname(__file__) or None,
        env=os.environ.copy(),
        text=True,
        capture_output=True,
        check=False,
    )
    if result.stdout:
        print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
    if result.stderr:
        print(
            result.stderr,
            end="" if result.stderr.endswith("\n") else "\n",
            file=sys.stderr,
        )
    return result


def _headers(stdout: str) -> dict[str, str]:
    head = stdout.split("\n---\n", 1)[0]
    headers: dict[str, str] = {}
    for line in head.splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        headers[key] = value
    return headers


def _body(stdout: str) -> str:
    parts = stdout.split("\n---\n", 1)
    return parts[1] if len(parts) == 2 else ""


def _recover(filesystem: str, run: str) -> None:
    """Delete a known task version left behind by a failed live run."""
    client, fs = _open(filesystem, "rw")
    try:
        try:
            current = fs.read(task_path(run))
        except MountableError as exc:
            if exc.status == 404:
                return
            raise
        if not current.info.etag:
            print("recover_left_file_without_etag", file=sys.stderr)
            return
        try:
            known = version_name(parse(current.data)) is not None
        except RecipeError:
            known = False
        if not known:
            print("recover_left_unfamiliar_file", file=sys.stderr)
            return
        fs.delete(task_path(run), if_match=current.info.etag)
        print(f"recovered {task_path(run)}", file=sys.stderr)
    finally:
        client.close()


def live(filesystem: str, run: str) -> int:
    """Run the two editors as separate processes, then delete the file."""
    task_path(run)
    before = _session_ids(filesystem)
    _print_account("before", filesystem)
    finished = False
    try:
        seeded = _spawn(["seed", "--filesystem", filesystem, "--run", run])
        if seeded.returncode != 0 or _headers(seeded.stdout).get("version") != "A":
            print("seed_failed", file=sys.stderr)
            return 1
        etag_a = _headers(seeded.stdout)["etag"]
        for _reader in ("one", "two"):
            read = _spawn(["read", "--filesystem", filesystem, "--run", run])
            if (
                read.returncode != 0
                or _headers(read.stdout).get("version") != "A"
                or _headers(read.stdout).get("etag") != etag_a
                or _body(read.stdout).encode() != VERSION_A
            ):
                print("read_failed", file=sys.stderr)
                return 1
        wrote = _spawn(
            [
                "commit",
                "--filesystem",
                filesystem,
                "--run",
                run,
                "--if-match",
                etag_a,
                "--editor",
                "one",
            ]
        )
        if wrote.returncode != 0 or _headers(wrote.stdout).get("version") != "B":
            print("commit_failed", file=sys.stderr)
            return 1
        etag_b = _headers(wrote.stdout)["etag"]
        if etag_b == etag_a or _body(wrote.stdout).encode() != VERSION_B:
            print("commit_failed", file=sys.stderr)
            return 1
        stale = _spawn(
            [
                "commit",
                "--filesystem",
                filesystem,
                "--run",
                run,
                "--if-match",
                etag_a,
                "--editor",
                "two",
            ]
        )
        if stale.returncode != 2 or "412 precondition_failed" not in stale.stderr:
            print("stale_write_was_not_refused", file=sys.stderr)
            return 1
        kept = _spawn(["read", "--filesystem", filesystem, "--run", run])
        if (
            kept.returncode != 0
            or _headers(kept.stdout).get("version") != "B"
            or _headers(kept.stdout).get("etag") != etag_b
            or _body(kept.stdout).encode() != VERSION_B
        ):
            print("version_b_changed", file=sys.stderr)
            return 1
        merged = _spawn(
            ["reconcile", "--filesystem", filesystem, "--run", run, "--editor", "two"]
        )
        if (
            merged.returncode != 0
            or _headers(merged.stdout).get("version") != "C"
            or _body(merged.stdout).encode() != VERSION_C
        ):
            print("reconcile_failed", file=sys.stderr)
            return 1
        etag_c = _headers(merged.stdout)["etag"]
        again = _spawn(["create-again", "--filesystem", filesystem, "--run", run])
        if again.returncode != 2 or "412 precondition_failed" not in again.stderr:
            print("create_collision_was_not_refused", file=sys.stderr)
            return 1
        still = _spawn(["read", "--filesystem", filesystem, "--run", run])
        if (
            still.returncode != 0
            or _body(still.stdout).encode() != VERSION_C
            or _headers(still.stdout).get("etag") != etag_c
        ):
            print("version_c_changed", file=sys.stderr)
            return 1
        removed = _spawn(
            ["cleanup", "--filesystem", filesystem, "--run", run, "--if-match", etag_c]
        )
        if removed.returncode != 0:
            print("cleanup_failed", file=sys.stderr)
            return 1
        gone = _spawn(["read", "--filesystem", filesystem, "--run", run])
        if gone.returncode == 0 or "404" not in gone.stderr:
            print("file_still_present", file=sys.stderr)
            return 1
        print("stale: 412")
        print("preserved: B")
        print("reconciled: C")
        print("collision: 412")
        print(f"deleted {task_path(run)}")
        print("absent: 404")
        finished = True
        return 0
    finally:
        if not finished:
            try:
                _recover(filesystem, run)
            except (MountableError, RecipeError, OSError) as exc:
                print(f"recover_failed: {exc}", file=sys.stderr)
        revoked = _revoke_new(filesystem, before)
        print(f"revoked_sessions={revoked}", file=sys.stderr)
        _print_account("after", filesystem)


def _fail(exc: BaseException) -> int:
    if isinstance(exc, MountableError) and exc.status == 412:
        current = exc.current_etag if exc.current_etag is not None else "null"
        print(f"412 precondition_failed current_etag={current}", file=sys.stderr)
        return 2
    if isinstance(exc, MountableError):
        print(f"{exc.status} {exc.code}", file=sys.stderr)
        return 1
    if isinstance(exc, RecipeError):
        print(f"{exc.code}: {exc}", file=sys.stderr)
        return 2
    print(f"failed: {exc}", file=sys.stderr)
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("seed", "read", "create-again"):
        command = sub.add_parser(name)
        command.add_argument("--filesystem", required=True)
        command.add_argument("--run", required=True)
    for name in ("commit", "reconcile"):
        command = sub.add_parser(name)
        command.add_argument("--filesystem", required=True)
        command.add_argument("--run", required=True)
        command.add_argument("--editor", required=True, choices=sorted(EDITORS))
        if name == "commit":
            command.add_argument("--if-match", required=True)
    cleanup_command = sub.add_parser("cleanup")
    cleanup_command.add_argument("--filesystem", required=True)
    cleanup_command.add_argument("--run", required=True)
    cleanup_command.add_argument("--if-match", required=True)
    live_command = sub.add_parser("live")
    live_command.add_argument("--filesystem", required=True)
    live_command.add_argument("--run", required=True)
    args = parser.parse_args(argv)
    try:
        task_path(args.run)
        if args.command == "live":
            return live(args.filesystem, args.run)
        mode = "ro" if args.command == "read" else "rw"
        client, fs = _open(args.filesystem, mode)
        try:
            path = task_path(args.run)
            if args.command == "seed":
                info = seed(fs, args.run)
                show(path, info.etag or "", VERSION_A)
            elif args.command == "read":
                current = fs.read(path)
                show(path, current.info.etag or "", current.data)
            elif args.command == "commit":
                info = commit(fs, args.run, args.if_match, args.editor)
                show(path, info.etag or "", proposal_from_a(args.editor))
            elif args.command == "reconcile":
                reconcile(fs, args.run, args.editor)
                current = fs.read(path)
                show(path, current.info.etag or "", current.data)
            elif args.command == "create-again":
                seed(fs, args.run)
                show(path, "", VERSION_A)
            else:
                removed = cleanup(fs, args.run, args.if_match)
                print(f"deleted {removed}")
        finally:
            client.close()
        return 0
    except Exception as exc:  # noqa: BLE001 - the CLI maps storage failures to an exit code
        return _fail(exc)


if __name__ == "__main__":
    raise SystemExit(main())
