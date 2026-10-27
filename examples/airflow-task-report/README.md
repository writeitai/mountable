# Read an Airflow task report from a fresh process

An Airflow TaskFlow DAG builds a small synthetic report, uploads the closed
file with `mountable-sdk`, and passes a 16 character id through XCom. The
reader task checks that file. Stop the DAG process and start another one.
The new process receives the id, opens a read-only handle, and checks the
same size and SHA-256.

The id is the XCom value. The report bytes stay in the file. This example
does not put the file in XCom, and it does not keep Airflow's metadata
database on mountable.io. The local test uses `airflow dags test` and a
disposable SQLite database. It does not start a scheduler.

The filesystem id and the API key stay in the environment. Tasks pass the
report id only.

This community example is maintained by mountable.io, independently of
Apache Airflow. It is not an official Airflow provider or integration. The
hosted mountable.io service is paid. The example and client are open source.

Pinned: `apache-airflow` 3.3.2, resolved with Airflow's
[constraints-3.12.txt](https://raw.githubusercontent.com/apache/airflow/constraints-3.3.2/constraints-3.12.txt)
for that release, and `mountable-sdk` 0.3.0.

## Setup

Use Python 3.12 and uv. Set `MOUNTABLE_API_KEY` in your private environment
or secret manager. Never put the key in a command argument. Use an existing
mountable.io account with a saved payment method, and a filesystem the key
can access. The example creates no account, filesystem or credits. File
operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=airflow-01-task-report).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/airflow-task-report
uv sync --locked --python 3.12
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export REPORT_STORE=mountable
```

The report belongs to the fixture user `alice`. Your backend would choose
its own user, and it would still choose the filesystem and the path.

For one filesystem and a hard monthly spend cap per end user, follow the
[capped workspace guide](https://mountable.io/docs/capped-workspace-per-user?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=airflow-01-task-report).
This recipe uses one existing filesystem.

## Write the report

```sh
uv run python report.py write
```

A successful run prints `report_id=` and 16 lowercase hex characters, then
a `match` line. The id is not a filesystem id and it is not a path. The
command runs `airflow dags test synthetic_report` against a disposable
database in `.airflow`. The producer task writes `reports/alice/<id>.txt`
only after it has closed the report, then returns the id. The reader task
receives that id. The write uses `If-None-Match: *`, so an existing file is
left as it is.

The report text looks like this, with that run's id:

```text
report_id: <id>
user: alice
kind: synthetic
item,quantity
alpha,1
beta,2
```

If the upload fails, the producer task fails and the command exits without
a `report_id=` line.

## Read it from a fresh process

In a new shell, with the same environment:

```sh
uv run python report.py read --report-id "$ID"
```

This process does not run an Airflow DAG. It opens a read-only handle. A
matching run prints one line: `match`, the path, the SHA-256 and the size
of the stored file. It prints that line only when those bytes are the
report the writer uploaded.

A second create of the same id does not replace the file:

```sh
uv run python report.py overwrite --report-id "$ID"
```

That command runs `airflow dags test replace_report` and exits with an
error. The first report is unchanged. A read-only handle cannot write it
either:

```sh
uv run python report.py read-only-write --report-id "$ID"
```

One command does the same check against the configured store. It runs the
DAG, reads from a new process, rejects a replacement, rejects a read-only
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
`.airflow` is the disposable local database. Delete that directory when
you no longer need the local DAG runs. It is not uploaded.

## Offline checks

```sh
uv run ruff format --check .
uv run ruff check .
uv run pytest -q
```

The tests call the store with a fake, and they also run `airflow dags test`
over a temporary directory. They make no paid cloud calls. They check that
XCom holds the id and not the report body, that a failed upload fails the
DAG, and that a second create leaves the first bytes in place.

Sources: [Pythonic DAGs with the TaskFlow API](https://airflow.apache.org/docs/apache-airflow/stable/tutorial/taskflow.html)
and [`airflow dags test`](https://airflow.apache.org/docs/apache-airflow/stable/cli-and-env-variables-ref.html),
checked against Apache Airflow 3.3.2 on 2026-10-27.
