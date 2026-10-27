"""Transfer a closed, trusted LlamaIndex JSON snapshot through mountable.io."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path
from tempfile import TemporaryDirectory

from llama_index.core import (
    Document,
    StorageContext,
    VectorStoreIndex,
    load_index_from_storage,
)
from llama_index.core.embeddings import BaseEmbedding
from mountable_sdk import Filesystem, Mountable, MountableError

FILES = (
    "docstore.json",
    "index_store.json",
    "graph_store.json",
    "default__vector_store.json",
    "image__vector_store.json",
)
MAX_BYTES = 1_048_576
FORMAT = "mountable-llamaindex-snapshot-v1"
VERSION = "0.14.25"
EXPECTED = "The synthetic observatory stores its telescope in the blue dome."


class LocalEmbedding(BaseEmbedding):
    """Fixed two-dimensional fixture, not a semantic embedding model."""

    def _get_text_embedding(self, text: str) -> list[float]:
        return [1.0, 0.0] if "telescope" in text.lower() else [0.0, 1.0]

    def _get_query_embedding(self, query: str) -> list[float]:
        return self._get_text_embedding(query)

    async def _aget_query_embedding(self, query: str) -> list[float]:
        return self._get_query_embedding(query)


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def prefix(run: str) -> str:
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,39}", run):
        raise ValueError("run must be 1-40 lowercase letters, digits or hyphens")
    return f"llamaindex/{run}"


def build_snapshot(directory: Path) -> dict[str, bytes]:
    index = VectorStoreIndex.from_documents(
        [
            Document(text=EXPECTED, id_="observatory"),
            Document(text="The synthetic bakery opens at six.", id_="bakery"),
        ],
        embed_model=LocalEmbedding(),
        show_progress=False,
    )
    index.set_index_id("synthetic-observatory")
    index.storage_context.persist(persist_dir=str(directory))
    # persist() returns after its JSON files have been closed.
    if {p.name for p in directory.iterdir()} != set(FILES):
        raise ValueError("Unexpected LlamaIndex artifact set")
    files = {name: (directory / name).read_bytes() for name in FILES}
    if any(len(data) > MAX_BYTES for data in files.values()):
        raise ValueError("Snapshot file exceeds 1 MiB")
    return files


def publish(fs: Filesystem, run: str) -> str:
    base = prefix(run)
    with TemporaryDirectory() as directory:
        files = build_snapshot(Path(directory))
    manifest = json.dumps(
        {
            "format": FORMAT,
            "llama_index_core": VERSION,
            "files": {
                name: {"size": len(data), "sha256": digest(data)}
                for name, data in files.items()
            },
        },
        sort_keys=True,
    ).encode()
    for name, data in files.items():
        fs.write(f"{base}/{name}", data, if_none_match=True)
        print(f"uploaded {name} {len(data)} {digest(data)}", flush=True)
    # A failed upload may leave partial files. Never retry with the same run.
    fs.write(f"{base}/manifest.json", manifest, if_none_match=True)
    return digest(manifest)


def restore(fs: Filesystem, run: str, manifest_sha256: str) -> str:
    base = prefix(run)
    if not re.fullmatch(r"[a-f0-9]{64}", manifest_sha256):
        raise ValueError("Provide the digest from the trusted publisher")
    raw = fs.read(f"{base}/manifest.json").data
    if len(raw) > 4096 or digest(raw) != manifest_sha256:
        raise ValueError("Manifest checksum or size mismatch")
    manifest = json.loads(raw)
    if (
        manifest.get("format") != FORMAT
        or manifest.get("llama_index_core") != VERSION
        or set(manifest.get("files", {})) != set(FILES)
    ):
        raise ValueError("Unexpected snapshot format, version or paths")
    entries = manifest["files"]
    for entry in entries.values():
        if (
            not isinstance(entry, dict)
            or type(entry.get("size")) is not int
            or not 0 < entry["size"] <= MAX_BYTES
            or not isinstance(entry.get("sha256"), str)
            or not re.fullmatch(r"[a-f0-9]{64}", entry["sha256"])
        ):
            raise ValueError("Invalid snapshot size or checksum")
    with TemporaryDirectory() as directory:
        for name in FILES:
            data = fs.read(f"{base}/{name}").data
            entry = entries[name]
            if len(data) != entry["size"] or digest(data) != entry["sha256"]:
                raise ValueError(f"Artifact checksum or size mismatch: {name}")
            json.loads(data)
            (Path(directory) / name).write_bytes(data)
        # Nothing is loaded until every required file passes validation.
        context = StorageContext.from_defaults(persist_dir=directory)
        index = load_index_from_storage(context, embed_model=LocalEmbedding())
        hits = index.as_retriever(similarity_top_k=1).retrieve(
            "Where is the telescope?"
        )
        result = hits[0].node.get_content()
        if result != EXPECTED:
            raise ValueError("Unexpected retrieved document")
        return result


def cleanup(fs: Filesystem, run: str) -> None:
    base = prefix(run)
    # Known names only: never trust the remote manifest for delete paths.
    for name in ("manifest.json", *FILES):
        path = f"{base}/{name}"
        try:
            file = fs.read(path)
            if not file.info.etag:
                raise ValueError("Cannot safely delete a file without its ETag")
            fs.delete(path, if_match=file.info.etag)
        except MountableError as error:
            if error.code != "not_found":
                raise
    # Empty directory deletes cannot remove another run's files.
    for path in (base, "llamaindex"):
        try:
            fs.delete(path)
        except MountableError as error:
            if error.code not in ("not_found", "directory_not_empty"):
                raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("publish", "query", "cleanup"))
    parser.add_argument("--filesystem", required=True)
    parser.add_argument("--run", required=True)
    parser.add_argument("--manifest-sha256")
    args = parser.parse_args()
    try:
        prefix(args.run)
    except ValueError as error:
        parser.error(str(error))
    if args.command == "query" and not args.manifest_sha256:
        parser.error("query requires --manifest-sha256 from the trusted publisher")
    try:
        with Mountable() as client:
            mode = "ro" if args.command == "query" else "rw"
            fs = client.filesystem(args.filesystem, mode=mode)
            if args.command == "publish":
                print("manifest_sha256=" + publish(fs, args.run))
            elif args.command == "query":
                print(restore(fs, args.run, args.manifest_sha256))
            else:
                cleanup(fs, args.run)
    except MountableError as error:
        # Never echo server details or credentials. Never retry unknown writes.
        parser.exit(1, f"{error.code}: inspect this run before retrying a write.\n")
    except (ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Snapshot refused: {error}\n")


if __name__ == "__main__":
    main()
