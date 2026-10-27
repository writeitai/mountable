"""Delete one finished job's files without touching another job.

Two synthetic jobs share one filesystem and own different prefixes. Job A
records the exact files it created and the ETag of each one. Cleanup deletes
a listed file only when its path is inside A's prefix and the ETag still
matches. Job B stays. A changed file stays. Empty directories under A's
prefix are removed one at a time. Nothing is deleted recursively.

Deletion removes the name. Storage and file-count charges for that
content stop after the deletion day. retained_bytes can still include
it until reclamation. Revoking a session does not delete data.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from typing import Any

from mountable_sdk import Mountable, MountableError

MAX_FILES = 32
MAX_FILE_BYTES = 1024 * 1024
RUN_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,39}$")
REPORT_NAME = "out/report.txt"
NOTES_NAME = "out/notes.txt"
MANIFEST_NAME = "complete.json"
KEEP_NAME = "keep.txt"
REPORT_BYTES = b"job-a report\n"
NOTES_BYTES = b"job-a notes\n"
KEEP_BYTES = b"job-b keep\n"


class CleanupError(Exception):
    def __init__(self, code: str, message: str) -> None:
        self.code = code
        super().__init__(message)


def check_run(run: str) -> str:
    if not RUN_RE.fullmatch(run):
        raise CleanupError(
            "bad_run",
            "RUN is 1–40 characters: a lowercase letter or digit, then letters, digits or hyphens.",
        )
    return run


def prefix_a(run: str) -> str:
    return f"jobs/{check_run(run)}/a"


def prefix_b(run: str) -> str:
    return f"jobs/{check_run(run)}/b"


def manifest_path(run: str) -> str:
    return f"{prefix_a(run)}/{MANIFEST_NAME}"


def file_a(run: str, name: str) -> str:
    return f"{prefix_a(run)}/{name}"


def file_b(run: str) -> str:
    return f"{prefix_b(run)}/{KEEP_NAME}"


def _allowlisted(path: str, prefix: str) -> bool:
    if path == prefix or not path.startswith(prefix + "/"):
        return False
    parts = path.split("/")
    return bool(parts) and all(part not in ("", ".", "..") for part in parts)


def _parents(path: str, prefix: str) -> list[str]:
    parts = path.split("/")
    found: list[str] = []
    for index in range(len(parts) - 1, 0, -1):
        parent = "/".join(parts[:index])
        if parent == prefix or parent.startswith(prefix + "/"):
            found.append(parent)
        else:
            break
    return found


def _etag(value: str | None) -> str:
    if not value:
        return ""
    text = value.strip()
    if text.startswith("W/"):
        text = text[2:].strip()
    if len(text) >= 2 and text[0] == text[-1] == '"':
        text = text[1:-1]
    return text


def _same_etag(left: str | None, right: str | None) -> bool:
    return bool(left) and _etag(left) == _etag(right)


def _current(fs: Any, path: str) -> tuple[str | None, int | None, str | None]:
    """Return etag, size and type for one name by listing its parent.

    A listing has no file body, so this check does not download the bytes.
    The filesystem root is the parent of a single path segment.
    """
    if not path or path in {".", ".."}:
        return None, None, None
    parent, name = path.rsplit("/", 1) if "/" in path else ("", path)
    if not name or name in {".", ".."}:
        return None, None, None
    after: str | None = None
    for _page in range(5):
        page = fs.list(parent, after=after, limit=1000)
        for entry in page.entries:
            entry_name = entry.path.rstrip("/").rsplit("/", 1)[-1]
            if entry_name == name:
                return entry.etag, entry.size, entry.type
        if not page.next:
            return None, None, None
        after = page.next
    raise CleanupError("list_truncated", f"Listing {parent} did not finish.")


def write_job_a(fs: Any, run: str) -> dict[str, Any]:
    files = (
        (file_a(run, REPORT_NAME), REPORT_BYTES),
        (file_a(run, NOTES_NAME), NOTES_BYTES),
    )
    recorded: list[dict[str, str]] = []
    for path, body in files:
        info = fs.write(path, body, if_none_match=True)
        if not info.etag:
            raise CleanupError("missing_etag", f"{path} has no ETag.")
        recorded.append({"path": path, "etag": info.etag})
    payload = json.dumps({"job": "a", "files": recorded}, indent=2).encode() + b"\n"
    info = fs.write(manifest_path(run), payload, if_none_match=True)
    if not info.etag:
        raise CleanupError("missing_etag", "The manifest has no ETag.")
    return {"manifest": manifest_path(run), "etag": info.etag, "files": recorded}


def write_job_b(fs: Any, run: str) -> dict[str, str]:
    info = fs.write(file_b(run), KEEP_BYTES, if_none_match=True)
    if not info.etag:
        raise CleanupError("missing_etag", "Job B's file has no ETag.")
    return {"path": file_b(run), "etag": info.etag}


def _read_manifest(fs: Any, run: str) -> tuple[bytes, str, list[dict[str, str]]]:
    path = manifest_path(run)
    try:
        current = fs.read(path)
    except MountableError as error:
        if error.code == "not_found":
            raise CleanupError(
                "no_manifest", "Job A has no completion manifest."
            ) from error
        raise
    if not current.info.etag:
        raise CleanupError("missing_etag", "The manifest has no ETag.")
    try:
        parsed = json.loads(current.data)
        rows = parsed["files"]
    except (json.JSONDecodeError, KeyError, TypeError) as error:
        raise CleanupError(
            "bad_manifest",
            "The manifest is not a list of path and ETag.",
        ) from error
    if not isinstance(rows, list):
        raise CleanupError(
            "bad_manifest", "The manifest is not a list of path and ETag."
        )
    files: list[dict[str, str]] = []
    for row in rows:
        if (
            not isinstance(row, dict)
            or not isinstance(row.get("path"), str)
            or not isinstance(row.get("etag"), str)
            or not row["etag"]
        ):
            raise CleanupError(
                "bad_manifest", "A manifest row needs a path and an ETag."
            )
        files.append({"path": row["path"], "etag": row["etag"]})
    return current.data, current.info.etag, files


def _delete_file(fs: Any, path: str, etag: str) -> dict[str, str] | None:
    try:
        fs.delete(path, if_match=etag)
    except MountableError as error:
        if error.code == "not_found":
            return {"path": path, "reason": "already_absent"}
        if error.code == "precondition_failed":
            return {
                "path": path,
                "reason": "etag_changed",
                "current_etag": error.current_etag or "",
            }
        raise
    return None


def _remove_empty(
    fs: Any, directories: list[str]
) -> tuple[list[str], list[dict[str, str]]]:
    removed: list[str] = []
    kept: list[dict[str, str]] = []
    ordered = sorted(
        set(directories), key=lambda item: (item.count("/"), item), reverse=True
    )
    for path in ordered:
        try:
            _current_etag, _size, kind = _current(fs, path)
        except MountableError as error:
            if error.code == "not_found":
                continue
            raise
        if kind is None:
            continue
        if kind != "directory":
            kept.append({"path": path, "reason": "not_a_directory"})
            continue
        try:
            fs.delete(path)
        except MountableError as error:
            if error.code == "not_found":
                continue
            if error.code == "directory_not_empty":
                kept.append({"path": path, "reason": "not_empty"})
                continue
            raise
        removed.append(path)
    return removed, kept


def cleanup_job_a(fs: Any, run: str) -> dict[str, Any]:
    """Delete unchanged files named by A's manifest, then empty A directories.

    A path outside A's prefix is refused. A different ETag is left in place.
    The manifest stays when anything was kept or a delete failed, so the
    partial result is still on the filesystem.
    """
    _data, manifest_etag, files = _read_manifest(fs, run)
    prefix = prefix_a(run)
    report: dict[str, Any] = {
        "deleted": [],
        "preserved": [],
        "refused": [],
        "already_absent": [],
        "directories_removed": [],
        "directories_kept": [],
        "manifest": "kept",
        "partial": False,
        "error": None,
    }
    if len(files) > MAX_FILES:
        report["partial"] = True
        report["error"] = "too_many"
        report["refused"].append({"path": manifest_path(run), "reason": "too_many"})
        return report
    seen: set[str] = set()
    candidates: list[dict[str, str]] = []
    for row in files:
        path = row["path"]
        if path in seen or path == manifest_path(run) or not _allowlisted(path, prefix):
            reason = "duplicate" if path in seen else "outside_allowlist"
            if path == manifest_path(run):
                reason = "manifest_path"
            report["refused"].append({"path": path, "reason": reason})
            continue
        seen.add(path)
        candidates.append(row)
    if report["refused"]:
        report["partial"] = True
    directories: list[str] = []
    for row in candidates:
        path, etag = row["path"], row["etag"]
        try:
            current_etag, size, kind = _current(fs, path)
        except MountableError as error:
            if error.code == "not_found":
                report["already_absent"].append(path)
                directories.extend(_parents(path, prefix))
                continue
            report["partial"] = True
            report["error"] = error.code
            break
        if kind is None:
            report["already_absent"].append(path)
            directories.extend(_parents(path, prefix))
            continue
        if kind != "file" or not current_etag:
            report["preserved"].append({"path": path, "reason": "not_a_file"})
            report["partial"] = True
            continue
        if size is not None and size > MAX_FILE_BYTES:
            report["preserved"].append({"path": path, "reason": "too_large"})
            report["partial"] = True
            continue
        if not _same_etag(current_etag, etag):
            report["preserved"].append(
                {"path": path, "reason": "etag_changed", "current_etag": current_etag}
            )
            report["partial"] = True
            continue
        try:
            held = _delete_file(fs, path, current_etag)
        except MountableError as error:
            report["partial"] = True
            report["error"] = error.code
            report["preserved"].append({"path": path, "reason": "delete_failed"})
            break
        if held is None:
            report["deleted"].append(path)
            directories.extend(_parents(path, prefix))
            continue
        if held["reason"] == "already_absent":
            report["already_absent"].append(path)
            directories.extend(_parents(path, prefix))
            continue
        report["preserved"].append(held)
        report["partial"] = True
    if not report["partial"] and report["error"] is None:
        try:
            fs.delete(manifest_path(run), if_match=manifest_etag)
        except MountableError as error:
            report["partial"] = True
            report["error"] = error.code
            report["preserved"].append(
                {"path": manifest_path(run), "reason": "manifest_changed"}
            )
        else:
            report["manifest"] = "removed"
            report["deleted"].append(manifest_path(run))
            directories.extend(_parents(manifest_path(run), prefix))
    try:
        removed, kept = _remove_empty(fs, directories)
    except MountableError as error:
        report["partial"] = True
        report["error"] = report["error"] or error.code
        return report
    report["directories_removed"] = removed
    report["directories_kept"] = kept
    if any(item["reason"] == "not_a_directory" for item in kept):
        report["partial"] = True
    return report


def cleanup_job_b(fs: Any, run: str) -> dict[str, Any]:
    """Delete B's one known file when its ETag is still the one we can read."""
    path = file_b(run)
    report: dict[str, Any] = {
        "deleted": [],
        "preserved": [],
        "already_absent": [],
        "directories_removed": [],
        "directories_kept": [],
        "partial": False,
        "error": None,
    }
    try:
        current_etag, size, kind = _current(fs, path)
    except MountableError as error:
        if error.code != "not_found":
            report["error"] = error.code
            report["partial"] = True
            return report
        current_etag = None
        size = None
        kind = None
    if kind is None:
        report["already_absent"].append(path)
        return report
    elif kind != "file" or not current_etag:
        report["preserved"].append({"path": path, "reason": "not_a_file"})
        report["partial"] = True
    elif size is not None and size > MAX_FILE_BYTES:
        report["preserved"].append({"path": path, "reason": "too_large"})
        report["partial"] = True
    else:
        try:
            held = _delete_file(fs, path, current_etag)
        except MountableError as error:
            report["error"] = error.code
            report["partial"] = True
            return report
        if held is None:
            report["deleted"].append(path)
        else:
            report["preserved"].append(held)
            report["partial"] = True
    if report["error"] is None and not report["partial"]:
        root = f"jobs/{check_run(run)}"
        removed, kept = _remove_empty(
            fs,
            [prefix_b(run), root, "jobs"],
        )
        report["directories_removed"] = removed
        report["directories_kept"] = kept
        if any(item["reason"] == "not_a_directory" for item in kept):
            report["partial"] = True
    return report


def show(fs: Any, run: str) -> list[str]:
    lines: list[str] = []
    for path in (
        file_a(run, REPORT_NAME),
        file_a(run, NOTES_NAME),
        manifest_path(run),
        file_b(run),
    ):
        try:
            etag, _size, kind = _current(fs, path)
        except MountableError as error:
            if error.code != "not_found":
                raise
            etag, kind = None, None
        if etag is None and kind is None:
            lines.append(f"{path} absent")
        else:
            lines.append(f"{path} present type={kind} etag={etag}")
    return lines


def _open(filesystem: str) -> tuple[Mountable, Any]:
    client = Mountable()
    return client, client.filesystem(filesystem, "rw")


def _print_report(report: dict[str, Any]) -> None:
    print(json.dumps(report, indent=2, sort_keys=True))


def _limits(info: Any) -> tuple[Any, ...]:
    return (
        info.spending_cap_cents,
        info.byte_quota,
        info.entry_quota,
        info.mutations_per_minute,
    )


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
    print("$ python cleanup.py " + " ".join(args))
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


def _line_state(stdout: str, path: str) -> str | None:
    for line in stdout.splitlines():
        if line.startswith(path + " "):
            return line.split(" ", 1)[1]
    return None


def live(filesystem: str, run: str) -> int:
    """Write both jobs, clean A, prove B survives revoke, then remove B."""
    check_run(run)
    before = _session_ids(filesystem)
    with Mountable() as client:
        limits_before = _limits(client.get_filesystem(filesystem))
    _print_account("before", filesystem)
    try:
        wrote_a = _spawn(["write-a", "--filesystem", filesystem, "--run", run])
        wrote_b = _spawn(["write-b", "--filesystem", filesystem, "--run", run])
        if wrote_a.returncode != 0 or wrote_b.returncode != 0:
            print("write_failed", file=sys.stderr)
            return 1
        cleaned = _spawn(["cleanup-a", "--filesystem", filesystem, "--run", run])
        if cleaned.returncode != 0:
            print("cleanup_a_failed", file=sys.stderr)
            return 1
        try:
            body = json.loads(cleaned.stdout)
        except json.JSONDecodeError:
            print("cleanup_a_failed", file=sys.stderr)
            return 1
        if body.get("partial") or body.get("manifest") != "removed":
            print("cleanup_a_incomplete", file=sys.stderr)
            return 1
        shown = _spawn(["show", "--filesystem", filesystem, "--run", run])
        if shown.returncode != 0:
            print("show_failed", file=sys.stderr)
            return 1
        a_gone = all(
            _line_state(shown.stdout, path) == "absent"
            for path in (
                file_a(run, REPORT_NAME),
                file_a(run, NOTES_NAME),
                manifest_path(run),
            )
        )
        b_state = _line_state(shown.stdout, file_b(run)) or ""
        if not a_gone or not b_state.startswith("present"):
            print("ownership_failed", file=sys.stderr)
            return 1
        print("b_survived=yes")
        print("a_absent=yes")
        revoked = _revoke_new(filesystem, before)
        print(f"revoked_sessions={revoked}", file=sys.stderr)
        after_revoke = _spawn(["show", "--filesystem", filesystem, "--run", run])
        if after_revoke.returncode != 0:
            print("show_failed", file=sys.stderr)
            return 1
        still_gone = all(
            _line_state(after_revoke.stdout, path) == "absent"
            for path in (
                file_a(run, REPORT_NAME),
                file_a(run, NOTES_NAME),
                manifest_path(run),
            )
        )
        still_b = (_line_state(after_revoke.stdout, file_b(run)) or "").startswith(
            "present"
        )
        if not still_gone or not still_b:
            print("revoke_changed_files", file=sys.stderr)
            return 1
        print("after_revoke_b_survived=yes")
        print("after_revoke_a_absent=yes")
        cleaned_b = _spawn(["cleanup-b", "--filesystem", filesystem, "--run", run])
        if cleaned_b.returncode != 0:
            print("cleanup_b_failed", file=sys.stderr)
            return 1
        final = _spawn(["show", "--filesystem", filesystem, "--run", run])
        if final.returncode != 0 or any(
            (_line_state(final.stdout, path) or "").startswith("present")
            for path in (
                file_a(run, REPORT_NAME),
                file_a(run, NOTES_NAME),
                manifest_path(run),
                file_b(run),
            )
        ):
            print("final_cleanup_failed", file=sys.stderr)
            return 1
        _revoke_new(filesystem, before)
        with Mountable() as client:
            limits_after = _limits(client.get_filesystem(filesystem))
        if limits_before != limits_after:
            print("limits_changed", file=sys.stderr)
            return 1
        print("caps_unchanged=yes")
        _print_account("after", filesystem)
        print(
            "deletion_is_logical=yes revoke_does_not_delete=yes",
            file=sys.stderr,
        )
        return 0
    finally:
        _revoke_new(filesystem, before)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("write-a", "write-b", "cleanup-a", "cleanup-b", "show", "live"):
        command = sub.add_parser(name)
        command.add_argument("--filesystem", required=True)
        command.add_argument("--run", required=True)
    args = parser.parse_args(argv)
    try:
        check_run(args.run)
    except CleanupError as error:
        print(error, file=sys.stderr)
        return 2
    try:
        if args.command == "live":
            return live(args.filesystem, args.run)
        client, fs = _open(args.filesystem)
        try:
            if args.command == "write-a":
                _print_report(write_job_a(fs, args.run))
            elif args.command == "write-b":
                _print_report(write_job_b(fs, args.run))
            elif args.command == "cleanup-a":
                report = cleanup_job_a(fs, args.run)
                _print_report(report)
                if report["error"]:
                    return 1
            elif args.command == "cleanup-b":
                report = cleanup_job_b(fs, args.run)
                _print_report(report)
                if report["error"]:
                    return 1
            else:
                print("\n".join(show(fs, args.run)))
        finally:
            client.close()
    except CleanupError as error:
        print(f"{error.code}: {error}", file=sys.stderr)
        return 1
    except MountableError as error:
        print(f"{error.status or ''} {error.code}".strip(), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
