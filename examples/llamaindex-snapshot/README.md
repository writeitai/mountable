# Reload a LlamaIndex snapshot from mountable.io

Build a tiny index, persist its closed JSON files, upload them through
`mountable-sdk`, then download and load them in a new process. A fixed local
embedding fixture retrieves the telescope document from two synthetic documents.
It makes no hosted embedding or LLM call and generates no answer text.

This community recipe is maintained by mountable.io, separately from LlamaIndex.
The client is Apache-2.0; mountable.io is a paid hosted filesystem.
It uses LlamaIndex's [persist/load API](https://developers.llamaindex.ai/python/framework/module_guides/storing/save_load/).

## Run

Use Python 3.11+ and uv. You need a mountable.io account with a saved payment
method, an existing filesystem, and an API key that can access it. Keep
`MOUNTABLE_API_KEY` in your private environment. Your trusted backend chooses
the filesystem and a new run id, never a model or untrusted caller.

```sh
uv sync --locked
export FS_ID="your-filesystem-id"
export RUN="snapshot1"
uv run python snapshot.py publish --filesystem "$FS_ID" --run "$RUN"
# Save the printed manifest_sha256 in your trusted job metadata.
export MANIFEST_SHA256="paste-the-64-character-digest-here"
uv run python snapshot.py query --filesystem "$FS_ID" --run "$RUN" \
  --manifest-sha256 "$MANIFEST_SHA256"
uv run python snapshot.py cleanup --filesystem "$FS_ID" --run "$RUN"
```

The query command starts a new process with a read-only SDK handle and an empty
temporary directory. It verifies the manifest against the publisher's digest,
then all five required file paths, sizes and SHA-256 hashes before loading.
It prints `The synthetic observatory stores its telescope in the blue dome.`
Run ids are 1–40 lowercase letters, digits or hyphens, starting with a letter
or digit. Files live under `llamaindex/<run>/`.

## Boundaries

- Use only your own trusted snapshots and keep the digest outside that storage
  prefix. Hashes check integrity, not who authored a file. JSON parsing does not
  make arbitrary LlamaIndex content trustworthy. No pickle or archive is loaded.
- The pinned LlamaIndex version writes five JSON artifacts. Each is at most
  1 MiB, plus a manifest at most 4 KiB. The SDK buffers each download before
  validation; this recipe is for trusted, small artifacts, not hostile uploads.
- Persist finishes locally before uploading. Each remote file is create-only.
  The manifest is written last. This is a frozen snapshot, not a multi-file
  transaction, a shared writable index, a vector-store adapter or an online
  vector database. Readers check the whole set before loading.
- After a timeout or partial upload, inspect the unique prefix before retrying.
  Clean up that run and choose a new id; a second publish cannot overwrite it.
- Restore with the same dependency versions and embedding configuration. The
  fixture's two-dimensional vectors prove a handoff, not retrieval quality.
- Cleanup deletes known files using their ETags, then only empty directories.
  Concurrent readers may fail during cleanup. Coordinate their lifetime.

Storage, downloads and file operations follow [usage prices](https://mountable.io/docs/billing).
A filesystem spending cap applies to mountable.io usage, not other providers.
This example creates no filesystem, account, credits or subscription. Deletion
does not undo billed usage; retained bytes can remain until garbage collection.

## Offline checks

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

Tests call real LlamaIndex and the released SDK over fake HTTP. They check
retrieval, a read-only session, missing/corrupt files, a changed manifest,
invalid paths/sizes/version, create-only reuse, partial upload and cleanup.
