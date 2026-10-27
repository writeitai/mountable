# DSPy evaluation files after the worker exits

Run a three-case, model-free DSPy evaluation. Upload its synthetic dataset,
fixed predictions and per-case results to mountable.io. A fresh reader checks
hashes and schemas, recomputes exact matches, and checks the same aggregate.
mountable.io maintains this example separately from DSPy.

The fixture is a `dspy.Module` returning fixed `dspy.Prediction` answers.
`dspy.Example(...).with_inputs("question")` supplies three arithmetic inputs
and `dspy.Evaluate` evaluates them. Two answers match. `66.67` is a fixture
percentage, not a model benchmark or a quality improvement.

## Run it

Use Python 3.11+ and uv. Dependencies are pinned to DSPy 3.4.1 and
mountable-sdk 0.3.0, with a committed lockfile. No LM key or service is needed.

For production, use your existing paid mountable.io account with a saved
payment method, an existing filesystem and an authorized API key. Put
`MOUNTABLE_API_KEY` in your private environment or secret manager; never paste
it into command arguments or Git. No account, filesystem or credits are created.

```bash
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-existing-filesystem"
RUN=$(python3 -c 'import secrets; print(secrets.token_hex(8))')
uv run python evaluation.py write --run "$RUN"
```

The successful writer prints `manifest_sha256=<64 lowercase hex characters>`.
DSPy may also log its average metric to stderr. Save that manifest digest as
`SHA` and pass it alongside `RUN` through your trusted backend to a new process:

```bash
uv run python evaluation.py read --run "$RUN" --manifest-sha256 "$SHA"
```

Expected output:

```text
match cases=3 score_percent=66.67
```

The filesystem is selected in the backend environment. The only argument
selecting files is a validated 16-character hex run id, beneath
`dspy-evaluations/`. Use one new run id per evaluation. Real applications
must authenticate their users before selecting the filesystem and run id.

## What is stored and checked

- `dataset.json`: three synthetic inputs and expected answers.
- `results.json`: fixed predictions, per-case boolean matches and score.
- `manifest.json`: format, DSPy version, run id, file sizes and SHA-256 hashes.

The entire bundle is capped at 1 MiB before upload. Files use create-only
writes (`If-None-Match: *`). The manifest is uploaded last, and its digest
is printed only after that upload succeeds. A failed upload can leave partial
files but cannot announce a complete bundle. Do not blindly retry a failed
write: inspect and clean the run, then use a new id. Writing an existing run
fails and leaves its first files intact.

The fresh reader opens an SDK `ro` handle. It checks the manifest digest
against the trusted writer value before following its fixed file names.
It verifies the schemas, exact synthetic inputs and predictions, byte lengths,
hashes, each exact-match decision and the aggregate rounded to two decimals.
Changed or missing records are refused. The reader recomputes from saved
records without invoking `Evaluate` or the predictor again.

The digest checks byte identity only when received through a trusted channel.
Recomputing the aggregate checks arithmetic, not the truth of arbitrary model
answers. These JSON records are not DSPy program serialization, an optimizer
checkpoint, or a training-resume format. Nothing downloaded is executed or
unpickled.

## Permission check and cleanup

This command must fail with `read_only` (HTTP 403 on the hosted service):

```bash
uv run python evaluation.py ro-write --run "$RUN"
```

Remove only this run, including partial uploads:

```bash
uv run python evaluation.py cleanup --run "$RUN"
```

Cleanup uses fixed paths and ETag-conditional deletes. It does not read deletion
paths from the remote manifest. The writer and reader use short-lived SDK token
sessions; these normally expire after one hour. Backend operators may revoke
sessions through the management API when a job ends.

Storage, retained versions and downloads follow the normal
[usage prices](https://mountable.io/docs/billing?utm_source=data-science-and-ml-workspaces&utm_medium=other&utm_content=dspy-01-evaluation-bundle). Logical deletion can leave
retained bytes billed until reclamation. Existing caps and quotas stay in force.
No model calls or new paid service are involved.

## Offline tests

```bash
uv run pytest -q
uv run ruff format --check .
uv run ruff check .
```

The real DSPy evaluator and released SDK run against a fake Files API bound to
an ephemeral loopback port. Tests start separate writer and reader processes,
check create-only preservation and read-only denial, remove or corrupt each file,
reject rehashed invalid records and schemas, and fail each upload in turn.
They make no production calls and do not establish production qualification.

APIs checked 2026-10-27 against the installed 3.4.1 release and its
[Evaluate source](https://github.com/stanfordnlp/dspy/blob/3.4.1/dspy/evaluate/evaluate.py).
The [modules guide](https://dspy.ai/learn/programming/modules/) and
[Evaluate documentation](https://dspy.ai/current/api/evaluation/Evaluate/)
explain the framework APIs. DSPy's
[contribution policy](https://github.com/stanfordnlp/dspy/blob/main/CONTRIBUTING.md)
forbids fully autonomous issues and PRs; this cookbook is published independently.
