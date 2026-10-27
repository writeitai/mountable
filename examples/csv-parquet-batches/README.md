# Resume CSV-to-Parquet batches after a worker restart

A worker turns a small CSV into Parquet files, one batch at a time. Each run
commits one batch and exits. The next run, in a fresh process, reads a small
manifest, skips the batches already recorded, and commits the next one.

The input is synthetic: 60 made-up sales rows in 3 batches of 20. It holds no
customer data. Each batch is a closed Parquet file with a unique name. The
manifest is written only after the batch upload reads back with the right
checksum, and each manifest update uses the ETag from the manifest read. A
competing writer stops the run instead of being overwritten. This is a
bounded recipe, not a job platform, a transaction, or a cross-machine lock.

This community example is maintained by mountable.io. The hosted
mountable.io service is paid. The example and client are open source.

Pinned in `uv.lock`: `mountable-sdk` 0.3.0 and Polars (see the lock file).

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=data-science-and-ml-workspaces&utm_medium=other&utm_content=etl-csv-parquet-batches-01).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/csv-parquet-batches
uv sync --locked
```

Your backend chooses the filesystem and the run id. Keep one run id for
every batch of a job, including the process that resumes it. Use a new
run id only for a different job. A run id is 1 to 40 lowercase letters,
digits or hyphens, and it must start with a letter or digit.

## Commit one batch at a time

```sh
uv run python etl.py run --filesystem "$FS_ID" --run "$RUN"
```

Run it three times. Each run commits one batch of 20 rows and prints
`committed`, the remote path, its SHA-256, and the batch count. Run it a
fourth time and it prints `done` without writing anything.

To see the restart case, give a second job its own run id and interrupt
its first run after the upload:

```sh
export RUN2="crash-$(date +%s)"
uv run python etl.py run --filesystem "$FS_ID" --run "$RUN2" --crash-after-upload
uv run python etl.py run --filesystem "$FS_ID" --run "$RUN2"
```

The first command uploads the batch and stops before recording it. The
second run finds that file, checks it against the rebuilt batch, and prints
`adopted` instead of uploading again. No row is counted twice.

## Check it from a fresh process

```sh
uv run python etl.py check --filesystem "$FS_ID" --run "$RUN" --dir ./out
```

`check` is a new process. It opens the filesystem read-only, downloads the
committed Parquet files into `./out`, and checks the columns
(`id,day,region,units,revenue_cents`), the row count, and that every row id
appears once. It prints `match` with the committed totals.

## Clean up

```sh
uv run python etl.py cleanup --filesystem "$FS_ID" --run "$RUN"
uv run python etl.py cleanup --filesystem "$FS_ID" --run "$RUN2"
```

Run the second line only if you tried the crash demo above.

## Offline tests

```sh
uv run pytest -q
```

The tests use a fake filesystem with ETags and real Polars. They need no
network and no API key. They cover the three-batch run, the crash between
upload and manifest, a competing manifest writer, a changed upload that is
never recorded, cleanup, read-only refusal, and bad run ids.
