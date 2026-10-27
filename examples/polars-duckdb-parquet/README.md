# Hand a closed Parquet dataset from Polars to DuckDB

A writer builds a small table with Polars, closes it as a Parquet file, and
uploads it with `mountable-sdk`. The writer exits. A fresh process downloads
the same file and queries the downloaded local file with DuckDB.

The file is the handoff. DuckDB reads the local download. It does not scan
mountable.io storage, and the two processes never share a writable `.duckdb`
database. The dataset is synthetic: 90 days of made-up sales rows. It holds
no customer data.

This community example is maintained by mountable.io, independently of
Polars and DuckDB. The hosted mountable.io service is paid. The example and
client are open source.

Pinned in `uv.lock`: `mountable-sdk` 0.3.0, Polars 2.1.0, DuckDB 2.0.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=data-science-and-ml-workspaces&utm_medium=other&utm_content=polars-duckdb-parquet-01).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/polars-duckdb-parquet
uv sync --locked
```

Your backend chooses the filesystem and the run id. Use a new run id for
each run. A run id is 1 to 40 lowercase letters, digits or hyphens, and it
must start with a letter or digit.

## Write and upload

```sh
uv run python handoff.py write --filesystem "$FS_ID" --run "$RUN"
```

`write` builds the table, writes `daily.parquet`, and uploads it to
`parquet/<run>/daily.parquet`. The write uses `If-None-Match: *`, so an
existing run is left as it is. The command prints the remote path, its
SHA-256 and its size. It does not print the API key.

## Check it from a fresh process

```sh
uv run python handoff.py check --filesystem "$FS_ID" --run "$RUN" --dir ./out
```

`check` is a new process. It opens the filesystem read-only, downloads the
file into `./out`, and runs DuckDB on that local file. It checks the
columns (`day,region,units,revenue_cents`), the row count, and the total
units. It prints `match`, the SHA-256, and the totals line.

## Clean up

```sh
uv run python handoff.py cleanup --filesystem "$FS_ID" --run "$RUN"
```

## Offline tests

```sh
uv run pytest -q
```

The tests use a fake filesystem and real Polars and DuckDB. They need no
network and no API key.
