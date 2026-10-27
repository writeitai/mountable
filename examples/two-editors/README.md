# Refuse a stale edit, then write the task file again on purpose

Two agent jobs can read the same short task file. If the second job writes
the whole file from the copy it remembers, it erases the first job's change.

mountable.io is a persistent filesystem for AI agents. This example sends
the ETag from that read with the write. The second write is refused, and
the first edit stays. The second job reads the file again and sets only
the field it owns.

The second write is a choice in your code. The example does not merge the
two copies. It uses the [files API](https://mountable.io/docs/files-api?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=two-editors-01-stale-edit#conditional-writes)
through `mountable-sdk`. The hosted service is paid. The example and the
client are open source.

Pinned: `mountable-sdk` 0.3.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=two-editors-01-stale-edit).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/two-editors
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export RUN="demo-$(date +%s)"
```

`RUN` is 1–40 characters: a lowercase letter or digit, then letters, digits
or hyphens. Use a new value each time. The task file is
`etag-edit-$RUN.txt`.

## Read version A in two processes

```sh
uv run python edit.py seed --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
uv run python edit.py read --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
uv run python edit.py read --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

`seed` creates the file only when the path is absent. A second create is
refused, and the existing bytes stay. Each `read` is its own process. Both
print version A and the same ETag:

```text
status: open
note: ready
```

## Let one editor commit, and refuse the stale write

Editor one sets `status` to `review`, using the ETag both processes read.
Replace `ETAG_A` with the `etag=` value from the read.

```sh
uv run python edit.py commit --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN" --if-match "$ETAG_A" --editor one
```

That file is version B:

```text
status: review
note: ready
```

Editor two still has version A. Its write would put `status` back to `open`
and add its own line. Send the same ETag:

```sh
uv run python edit.py commit --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN" --if-match "$ETAG_A" --editor two
```

The command exits with an error and prints `412 precondition_failed`.
Version B is unchanged. Read it again to see that.

## Read again, then set only your field

Editor two reads the current file and sets `editor` to `two`. It keeps
`status` as it is now. It does not write the copy it remembered.

```sh
uv run python edit.py reconcile --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN" --editor two
```

Version C:

```text
status: review
note: ready
editor: two
```

A later create of the same path is still refused:

```sh
uv run python edit.py create-again --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

## Clean up

Pass the ETag from the latest read. A different ETag leaves the file in
place.

```sh
uv run python edit.py cleanup --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN" --if-match "$ETAG"
```

One command runs the same steps as separate processes, then deletes the
file:

```sh
uv run python edit.py live --filesystem "$MOUNTABLE_FILESYSTEM_ID" --run "$RUN"
```

It prints `stale: 412`, `preserved: B`, `reconciled: C` and `collision: 412`
when the checks pass. It does not print the API key.

At the [USD usage prices](https://mountable.io/docs/billing), storage costs
$0.20/GiB-month and downloads cost $0.02/GiB. The task file is under 100
bytes. Retained data can remain billable until garbage collection after
deletion.

## Offline checks

```sh
uv run pytest -q
uv run ruff check .
uv run ruff format --check .
```

The tests call the released SDK against a local fake API. They make no
paid cloud calls. They check the refused write, the reread, the create
collision and a cleanup that leaves a changed file alone.
