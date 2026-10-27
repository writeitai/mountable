# Read a Prefect task report from a fresh flow process

A Prefect flow builds a small synthetic report, uploads the closed file with
`mountable-sdk`, and returns an id. Stop that process and start another one.
The reader flow receives the id, opens a read-only handle, and checks the
same size and SHA-256.

The id is the task's return value. The file is not a Prefect result, a
cache, or saved orchestration state. This example does not resume a flow
run. The filesystem id and the API key stay in the environment. Tasks pass
the report id only.

This community example is maintained by mountable.io, independently of
Prefect. It is not an official Prefect integration. The hosted mountable.io
service is paid. The example and client are open source.

Pinned: `prefect` 3.9.0 and `mountable-sdk` 0.3.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=prefect-01-task-report).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/prefect-task-report
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export REPORT_STORE=mountable
```

The report belongs to the fixture user `alice`. Your backend would choose
its own user, and it would still choose the filesystem and the path.

For one filesystem and a hard monthly spend cap per end user, follow the
[capped workspace guide](https://mountable.io/docs/capped-workspace-per-user?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=prefect-01-task-report).
This recipe uses one existing filesystem.

## Write the report

```sh
uv run python report.py write
```

A successful run prints one line, `report_id=`, and 16 lowercase hex
characters. That id is not a filesystem id and it is not a path. The flow
runs two tasks. The first task chooses the id. The second task writes
`reports/alice/<id>.txt` and returns the id. The write uses
`If-None-Match: *`, so an existing file is left as it is.

The report text looks like this, with that run's id:

```text
report_id: <id>
user: alice
kind: synthetic
item,quantity
alpha,1
beta,2
```

If the upload fails, the task fails and the command exits without a
`report_id=` line.

## Read it from a fresh process

In a new shell, with the same environment:

```sh
uv run python report.py read --report-id "$ID"
```

The reader flow passes only that id into its task. The task opens a
read-only handle. A matching run prints one line: `match`, the path, the
SHA-256 and the size of the stored file. The task prints that line only
when those bytes are the report the writer uploaded.

A second create of the same id does not replace the file:

```sh
uv run python report.py overwrite --report-id "$ID"
```

That command exits with an error. The first report is unchanged. A
read-only handle cannot write it either:

```sh
uv run python report.py read-only-write --report-id "$ID"
```

One command does the same check against the configured store. It writes,
reads from a new process, rejects a replacement, rejects a read-only
write, and deletes the file:

```sh
uv run python report.py live
```

It does not print the API key.

## Clean up

```sh
uv run python report.py cleanup --report-id "$ID"
```

This deletes that one file. A file that is already gone is left gone.

## Offline checks

```sh
uv run pytest -q
uv run ruff check .
uv run ruff format --check .
```

The tests call the real `@flow` and `@task` functions against a fake
store, and they also run two local processes over a temporary directory.
They make no paid cloud calls. They check the hash, a failed upload, and
that a second create leaves the first bytes in place.

Sources: [write and run a workflow](https://docs.prefect.io/v3/how-to-guides/workflows/write-and-run),
checked against Prefect 3.9.0 on 2026-10-27.
