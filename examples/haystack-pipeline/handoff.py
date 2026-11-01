"""Save this trusted Haystack pipeline, then rerun it in a fresh process."""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import re
from pathlib import Path
from tempfile import TemporaryDirectory

# The fixture makes no telemetry or hosted-model calls.
os.environ["HAYSTACK_TELEMETRY_ENABLED"] = "false"

from haystack import Document, Pipeline  # noqa: E402
from haystack.components.preprocessors import (  # noqa: E402
    DocumentCleaner,
    DocumentSplitter,
)
from mountable_sdk import Filesystem, Mountable, MountableError  # noqa: E402

MAX_BYTES = 16_384
INPUT = (
    "  The synthetic observatory has a blue dome.  \n\nThe telescope opens at dusk.  "
)
EXPECTED = [
    "The synthetic observatory has a blue dome.",
    "The telescope opens at dusk.",
]


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def remote_path(run: str) -> str:
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,39}", run):
        raise ValueError("run must be 1-40 lowercase letters, digits or hyphens")
    return f"haystack/{run}.yaml"


def build_pipeline() -> Pipeline:
    pipeline = Pipeline()
    pipeline.add_component("clean", DocumentCleaner(strip_whitespaces=True))
    pipeline.add_component(
        "split",
        DocumentSplitter(split_by="word", split_length=7, split_overlap=0),
    )
    pipeline.connect("clean.documents", "split.documents")
    return pipeline


def run_pipeline(pipeline: Pipeline) -> list[str]:
    result = pipeline.run({"clean": {"documents": [Document(content=INPUT)]}})
    output = [doc.content.strip() for doc in result["split"]["documents"]]
    if output != EXPECTED:
        raise ValueError("Unexpected pipeline output")
    return output


def known_yaml() -> bytes:
    return build_pipeline().dumps().encode("utf-8")


def validate(data: bytes, sha256: str) -> None:
    if not re.fullmatch(r"[a-f0-9]{64}", sha256):
        raise ValueError("Use the SHA-256 from the trusted publisher")
    if not 0 < len(data) <= MAX_BYTES or digest(data) != sha256:
        raise ValueError("Pipeline checksum or size mismatch")
    # A matching hash alone does not make downloaded executable config safe.
    # This intentionally accepts only our locally defined, pinned fixture.
    if data != known_yaml():
        raise ValueError("Not this example's known pipeline configuration")
    if os.environ.get("HAYSTACK_DESERIALIZATION_ALLOWLIST"):
        raise ValueError("Leave Haystack's default trusted-module list unchanged")


def publish(fs: Filesystem, run: str) -> tuple[str, list[str]]:
    path = remote_path(run)
    pipeline = build_pipeline()
    output = run_pipeline(pipeline)
    with TemporaryDirectory() as directory:
        local = Path(directory) / "pipeline.yaml"
        with local.open("w", encoding="utf-8", newline="\n") as stream:
            pipeline.dump(stream)
        data = local.read_bytes()  # dump's file is closed before upload.
    sha = digest(data)
    validate(data, sha)
    fs.write(path, data, if_none_match=True)
    return sha, output


def restore(fs: Filesystem, run: str, sha256: str) -> list[str]:
    path = remote_path(run)
    data = fs.read(path).data
    validate(data, sha256)
    # Default allowlist, no callbacks, unsafe flag or custom marshaller.
    pipeline = Pipeline.load(io.StringIO(data.decode("utf-8")))
    return run_pipeline(pipeline)


def cleanup(fs: Filesystem, run: str, sha256: str) -> None:
    path = remote_path(run)
    file = fs.read(path)
    validate(file.data, sha256)
    if not file.info.etag:
        raise ValueError("Cannot safely delete without an ETag")
    fs.delete(path, if_match=file.info.etag)
    try:
        fs.delete("haystack")  # Empty-directory delete cannot remove other runs.
    except MountableError as error:
        if error.code not in ("not_found", "directory_not_empty"):
            raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("publish", "run", "cleanup"))
    parser.add_argument("--filesystem", required=True)
    parser.add_argument("--run", required=True)
    parser.add_argument("--sha256")
    args = parser.parse_args()
    try:
        remote_path(args.run)
        if args.command != "publish" and not re.fullmatch(
            r"[a-f0-9]{64}", args.sha256 or ""
        ):
            raise ValueError("run and cleanup require --sha256 from publish")
    except ValueError as error:
        parser.error(str(error))
    try:
        with Mountable() as client:
            fs = client.filesystem(
                args.filesystem, mode="ro" if args.command == "run" else "rw"
            )
            if args.command == "publish":
                sha, output = publish(fs, args.run)
                print("sha256=" + sha)
                print(json.dumps(output))
            elif args.command == "run":
                print(json.dumps(restore(fs, args.run, args.sha256)))
            else:
                cleanup(fs, args.run, args.sha256)
                print("deleted " + remote_path(args.run))
    except MountableError as error:
        parser.exit(1, f"{error.code}: inspect this run before retrying a write.\n")
    except ValueError as error:
        parser.exit(1, f"Pipeline refused: {error}\n")


if __name__ == "__main__":
    main()
