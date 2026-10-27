# Git source history and mountable.io output files

Keep reviewed source in Git. Hand a generated report to a later worker through
mountable.io, a persistent filesystem for AI agents. This example is maintained
by mountable.io. It uses local Git and the released Python SDK, with no model,
FUSE mount or remote Git write.

The fixture makes two commits on local disk, shows the source diff and writes a
110-byte synthetic JSON report. The report names the second commit. A writer
uploads that closed file. A fresh reader process opens the same filesystem
read-only and checks the report's SHA-256, schema and known result.

## Run

Use Git 2.29+, Python 3.11+ and uv. For the file handoff, use an existing
mountable.io account with a payment method, filesystem and API key. Usage is
billed at the [published prices](https://mountable.io/docs/billing). Keep
`MOUNTABLE_API_KEY` in your private environment; never pass it in an argument.
The local Git step and offline tests need no credentials.

```sh
uv sync --locked
uv run python handoff.py git-demo --report report.json
```

This creates a temporary local repository, prints the real diff and commit
subjects, then removes that repository. `report.json` stays on local disk. Its
`source_commit` identifies the fixture commit, not a commit in your own project.
A second invocation refuses to replace `report.json`. Use a new filename.

```sh
export FS_ID=your-filesystem-id
export RUN="demo-$(date +%s)"
export SHA=$(uv run python -c 'import hashlib; print(hashlib.sha256(open("report.json", "rb").read()).hexdigest())')
uv run python handoff.py upload --filesystem "$FS_ID" --run "$RUN" --report report.json
# The writer has exited. Start a fresh reader:
uv run python handoff.py read --filesystem "$FS_ID" --run "$RUN" --sha256 "$SHA"
uv run python handoff.py cleanup --filesystem "$FS_ID" --run "$RUN" --sha256 "$SHA"
```

Upload prints `stored git-output-<run>.json bytes=110 sha256=...`. Read prints
`match`, then the JSON with `result: 7` and the source commit. Cleanup prints
`deleted git-output-<run>.json`. Remove your local `report.json` when done.

## Boundaries

- A trusted job chooses the filesystem, run ID and expected checksum. The run ID
  is 1–40 lowercase letters, digits or hyphens. Reports are at most 4096 bytes.
- Upload uses `If-None-Match: *`. Reusing a run returns 412 and preserves the
  original file. Writes are not retried after failures or unknown outcomes;
  inspect the existing path before deciding what to do next.
- Read uses an SDK `ro` session. A job with a write-capable API key can request
  `rw` again. Give separate consumers appropriately limited credentials;
  selecting `ro` in code does not reduce that key's authority.
- Cleanup reads and checks the bytes before deleting with their ETag. Changed
  bytes or a changed version are left alone. A missing file returns 404.
- The checksum detects changed bytes relative to the expected value. It is not
  a signature or proof that a writer ran a particular source revision.
- SDK token sessions expire after an hour. Closing the client closes HTTP
  connections; it does not revoke sessions. A backend can revoke its sessions
  through the management API. The production qualification revoked every
  session it created.

Git supplies version history, diffs, branches, local/offline operations and
clones. Commit a report too when its history belongs with the code. The shared
filesystem is useful for this run's output with its own readers and deletion
schedule. See [the cookbook](https://mountable.io/docs/git-and-output) and
[filesystem contract](https://mountable.io/docs/limits).

## Offline checks

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

The tests execute real local Git and the released SDK over fake HTTP. They
check the diff, commit/report link, refused replacement, read-only denial,
changed-file preservation, bounded inputs, missing files, conditional deletion
and no write retry on a 402 or 500. No network or paid service is used.
