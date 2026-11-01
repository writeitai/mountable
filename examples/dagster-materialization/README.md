# Read a Dagster materialization report from a fresh process

A local Dagster run builds two tiny assets. `synthetic_rows` makes a
two-row table in memory. `materialization_report` closes a JSON report,
uploads that file with `mountable-sdk`, and returns an id. Stop that
process and start another one. The reader receives the id and the run id,
opens a read-only handle, and checks the stored run id, asset key, row
count and checksums.

The id is the asset's return value. The file is not a Dagster I/O manager
output, and this example does not keep the run record. The run uses
`materialize_to_memory` on `DagsterInstance.ephemeral()`. This is not an
I/O manager, a storage plugin, a remote run store, or Dagster+.

The filesystem id and the API key stay in the environment. The assets pass
the report id only.

This community example is maintained by mountable.io, independently of
Dagster. It is not an official Dagster integration. The hosted mountable.io
service is paid. The example and client are open source.

Pinned: `dagster` 1.13.27 and `mountable-sdk` 0.3.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=dagster-01-materialization-report).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/dagster-materialization
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export REPORT_STORE=mountable
```

The report belongs to the fixture user `alice`. Your backend would choose
its own user, and it would still choose the filesystem and the path.

For one filesystem and a hard monthly spend cap per end user, follow the
[capped workspace guide](https://mountable.io/docs/capped-workspace-per-user?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=dagster-01-materialization-report).
This recipe uses one existing filesystem.

## Write the report

```sh
uv run python report.py write
```

A successful run prints the report id, the Dagster run id, the asset key
`materialization_report`, the row count, the rows checksum, and the file
checksum. The report id is 16 lowercase hex characters. It is not a
filesystem id and it is not a path. The asset writes
`reports/alice/<id>.json` only after it has closed the report, then returns
that id. The write uses `If-None-Match: *`, so an existing file is left as
it is.

The file is JSON. It records the run id, the asset key, two synthetic rows,
the row count and the checksum of those rows. It holds no customer data.

If the upload fails, the asset fails and the command exits without a
`report_id=` line.

## Read it from a fresh process

In a new shell, with the same environment, pass the id and the run id the
writer printed:

```sh
uv run python report.py read --report-id "$ID" --run-id "$RUN_ID"
```

This process does not open the Dagster instance. It opens a read-only
handle and checks the stored run id, asset key, row count, rows checksum
and file checksum. A matching run prints one `match` line. It prints that
line only when those fields are the report the writer uploaded.

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

The tests call the real `@asset` functions and `materialize_to_memory`
against a fake store, and they also run two local processes over a
temporary directory. They make no paid cloud calls. They check the run id,
asset key, row count, checksums, a failed upload, and that a second create
leaves the first bytes in place.

Sources: [Defining assets](https://dagster.io/docs/guides/build/assets/defining-assets),
checked against Dagster 1.13.27 on 2026-10-27.
