# Clean up one finished job without deleting the other

Two agent jobs can share one filesystem. Job A finishes. A recursive
delete of its directory can also remove job B.

mountable.io is a persistent filesystem for AI agents. This example
writes a completion manifest for job A: the exact paths it created and
the ETag of each file. Cleanup deletes a listed file only when the path
is inside A's prefix and the ETag still matches. Job B stays. A changed
file stays. Empty directories under A's prefix are removed one at a
time. Nothing is deleted recursively.

The hosted service is paid. The example and the client are open source.
Pinned: `mountable-sdk` 0.3.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=job-cleanup-01).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/job-cleanup
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export RUN="demo-$(date +%s)"
```

`RUN` is 1–40 characters: a lowercase letter or digit, then letters,
digits or hyphens. Use a new value each time.

## Write both jobs

```sh
uv run python cleanup.py write-a --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
uv run python cleanup.py write-b --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

Job A writes two files and `complete.json`:

- `jobs/$RUN/a/out/report.txt` is `job-a report`
- `jobs/$RUN/a/out/notes.txt` is `job-a notes`
- `jobs/$RUN/a/complete.json` lists those two paths and their ETags

Job B writes `jobs/$RUN/b/keep.txt` with `job-b keep`. The manifest does
not name that file.

## Clean up job A

```sh
uv run python cleanup.py cleanup-a --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

The command prints JSON. Unchanged files inside `jobs/$RUN/a` are
deleted with `If-Match`. `jobs/$RUN/b/keep.txt` stays. A path outside
A's prefix is refused, even if the manifest names it. A file whose ETag
changed stays, `partial` is true, and `complete.json` stays so you can
see what remains.

Empty directories under A's prefix are deleted one at a time. A
directory that still has a file stays. The files API answers
`409 directory_not_empty` and the contents are left as they are.

## See both prefixes

```sh
uv run python cleanup.py show --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

## Remove job B when you mean to

```sh
uv run python cleanup.py cleanup-b --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

This deletes only `jobs/$RUN/b/keep.txt`, then removes that directory
if it is empty. It does not read A's manifest.

## Run the check

```sh
uv run python cleanup.py live --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

The command runs the writes and the cleanup as separate processes. It
prints `b_survived=yes` and `a_absent=yes`, revokes the sessions this
run opened, and checks that job B is still present. Revoking a session
does not delete data. It then removes job B and prints
`caps_unchanged=yes`. It does not print the API key, and it does not
change the filesystem's cap or quotas.

A delete removes the name. Storage and file-count charges for that
content stop after the deletion day. `retained_bytes` can still include
that content until reclamation. At the
[USD usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=job-cleanup-01),
storage costs $0.20/GiB-month and downloads cost $0.02/GiB. These files
are a few hundred bytes.

The guide is <https://mountable.io/docs/job-cleanup>.

## Offline checks

```sh
uv run --locked pytest -q
uv run --locked ruff check .
uv run --locked ruff format --check .
```

The tests call the released SDK against a local fake API. They make no
paid cloud calls. They check that job B survives, a changed ETag is
kept, a path outside the prefix is refused, an extra file keeps its
directory, and a failed delete stops and stays visible.
