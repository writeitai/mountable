# Get a generated report after the FastAPI worker restarts

A small FastAPI app generates a synthetic report for a fixture user, uploads
the closed file with `mountable-sdk`, and returns an opaque id. Stop that
process and start a new one. The same request returns the same bytes.

The filesystem id and the storage path stay in the server's environment.
The caller cannot choose them. A different fixture user cannot read the
report. This is a storage handoff, not a job queue or a login system.

This community example is maintained by mountable.io, independently of
FastAPI. The hosted mountable.io service is paid. The example and client
are open source.

Pinned: `fastapi` 0.162.0, `uvicorn` 0.54.0, `httpx` 0.28.1 and
`mountable-sdk` 0.3.0.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=fastapi-01-report).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/fastapi-reports
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export REPORT_STORE=mountable
```

The two fixture tokens are `fixture-alice` and `fixture-bob`. They exist
only in this example. Your backend would use its own session instead, and
it would still choose the filesystem and the path.

For one filesystem and a hard monthly spend cap per end user, follow the
[capped workspace guide](https://mountable.io/docs/capped-workspace-per-user?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=fastapi-01-report).
This recipe uses one existing filesystem.

## Create a report

```sh
uv run uvicorn app:create_default_app --factory --host 127.0.0.1 --port 8000
```

In another shell:

```sh
curl -s -H "Authorization: Bearer fixture-alice" \
  -X POST http://127.0.0.1:8000/reports
```

A successful response is `201` and a JSON object with one field, `id`.
The id is 16 lowercase hex characters. It is not a filesystem id and it is
not a path. The server stores `reports/alice/<id>.txt`. The write uses
`If-None-Match: *`, so an existing file is left as it is.

The report text looks like this, with that request's id:

```text
report_id: <id>
user: alice
kind: synthetic
item,quantity
alpha,1
beta,2
```

If the upload fails, the response is `503` with `{"detail":"upload_failed"}`.
That JSON has no `id`.

## Read it from a fresh process

Stop the server and start it again with the same environment. Then:

```sh
curl -s -D - -H "Authorization: Bearer fixture-alice" \
  http://127.0.0.1:8000/reports/$ID
```

The body is the same report. Its SHA-256 matches the body you saved before
the restart:

```sh
curl -s -H "Authorization: Bearer fixture-alice" \
  http://127.0.0.1:8000/reports/$ID | sha256sum
```

The same id with Bob's token returns `404` and `{"detail":"not_found"}`.
The server looks only under `reports/bob/`. A missing or unknown token
returns `401`.

One command does the same check against the configured store. It creates a
report, restarts the server, compares the SHA-256, checks Bob's refusal,
and deletes the file:

```sh
uv run python app.py live
```

## Clean up

```sh
uv run python app.py cleanup --user alice --report-id "$ID"
```

This deletes that one file. A file that is already gone is left gone.

## Offline checks

```sh
uv run pytest -q
uv run ruff check .
uv run ruff format --check .
```

The tests use `TestClient` and a fake store, and they also start two local
server processes over a temporary directory. They make no paid cloud calls.
They check the report hash, a refusal for the other fixture user, and that
a failed upload returns no id.

Sources: [FastAPI testing](https://fastapi.tiangolo.com/tutorial/testing/),
[FastAPI dependencies](https://fastapi.tiangolo.com/tutorial/dependencies/),
[run a FastAPI process](https://fastapi.tiangolo.com/deployment/manually/).
