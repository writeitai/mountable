# Keep pytest JUnit reports after the test worker exits

A test worker runs a tiny pytest suite, saves its JUnit XML and a short
failure note, and uploads both closed files with `mountable-sdk`. The worker
exits. A fresh process downloads the same files and checks every byte.

The suite is synthetic: one passing test and one test that fails on purpose.
It holds no customer logs, tokens or real test data. This recipe is a storage
handoff, not a test-reporting service.

This community example is maintained by mountable.io, independently of
pytest. The hosted mountable.io service is paid. The example and client are
open source.

Pinned: `mountable-sdk` 0.3.0 and `pytest` 8 or newer.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=pytest-01-junit-reports).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/pytest-reports
uv sync --locked
```

Your backend or CI job chooses the filesystem and the run id. Do not take
either from test output. Use a new run id for each run. A run id is 1 to 40
lowercase letters, digits or hyphens, and it must start with a letter or
digit.

## Run the suite and upload its report

```sh
uv run python reports.py run \
  --filesystem "$FS_ID" \
  --run "$RUN" \
  --dir ./out
echo "pytest exit code: $?"
```

`run` executes the suite, writes `junit.xml` and `failure.txt` into `./out`,
then uploads `pytest/<run>/junit.xml` and `pytest/<run>/failure.txt`. Each
file must be at most 1 MiB. The write uses `If-None-Match: *`, so an existing
run is left as it is. The command prints each remote path, its SHA-256 and
its size. It does not print the API key.

The command returns pytest's exit code. The synthetic failing test makes
that code 1, so the calling job still fails. An upload failure is printed
as `upload_failed` and does not change that code. After a timeout, read the
path before writing it again, and use a new run id for the next run.

A read-only reviewer cannot overwrite the report. Give the reviewer a
read-only handle or grant, as `check` below does. Keep the write key in the
trusted job's secrets.

A `402` with `filesystem_spending_cap_reached` stops that filesystem's
billable mountable.io work. Other filesystems keep working.

## Check the report from a fresh process

```sh
uv run python reports.py check \
  --filesystem "$FS_ID" \
  --run "$RUN" \
  --dir ./out
```

`check` is a new process. It opens the filesystem read-only, downloads both
files, compares them with `./out`, parses the JUnit XML and prints
`match`, each SHA-256, and the test and failure counts.

## Clean up

```sh
uv run python reports.py cleanup \
  --filesystem "$FS_ID" \
  --run "$RUN"
```

This deletes both files. A file that is already gone is left gone.

## Offline checks

```sh
uv run pytest test_reports.py -q
uv run ruff check .
uv run ruff format --check .
```

The tests use a fake filesystem and run the real synthetic suite in a
subprocess. They make no paid cloud calls. They check that the failing
test returns exit code 1, the JUnit XML parses with two tests and one
failure, a second upload to the same run is refused, changed bytes are
detected, and cleanup leaves no files.

## Read the report later

You do not need the worker's files to read the report. From any machine:

```sh
mountable files read "$FS_ID" pytest/run1/junit.xml > junit.xml
mountable files read "$FS_ID" pytest/run1/failure.txt
```

Use `check` above when `./out` is still on your machine and you want the
byte-for-byte comparison.
