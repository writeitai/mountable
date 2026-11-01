# Haystack pipeline YAML across workers

Save a small Haystack pipeline configuration on mountable.io. A fresh process
downloads and verifies the YAML, loads the pipeline locally, and produces the
same two text chunks. mountable.io maintains this community recipe separately
from Haystack. It uses built-in components without a hosted model or provider key.

The YAML describes components, settings and connections. The next worker supplies
the documents again. It does not contain a document store, an index, in-flight
execution or a recovery checkpoint. For source-reviewed fixed configuration, Git
is a good choice; this demonstrates handing a job-produced configuration file to
another worker without a commit or redeploy.

## Set up

Use Python 3.11+ and uv. Dependencies are pinned to `haystack-ai==3.4.1` and
`mountable-sdk==0.3.0`, with a lockfile. The deterministic fixture cleans two
synthetic sentences and splits by seven words using Haystack `DocumentCleaner`
and `DocumentSplitter`. No tokenizer/model download is needed. Haystack telemetry
is disabled by the example before import.

You need an existing paid mountable.io account, a saved payment method, a
filesystem and an API key permitted to access it. Set `MOUNTABLE_API_KEY` in your
private environment, never in a committed file. Backend code chooses the
filesystem and run name, not a model or untrusted caller.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/haystack-pipeline
uv sync --locked
export FS_ID="your-filesystem-id"
export RUN="pipeline1"
uv run python handoff.py publish --filesystem "$FS_ID" --run "$RUN"
```

Use a new run name each time: 1–40 lowercase letters, digits or hyphens, starting
with a letter or digit. The file is `haystack/<run>.yaml`. Publishing runs the
pipeline, calls `Pipeline.dump()` on a local file, closes that file, then uploads
it with `if_none_match=True`. Reusing the path returns `precondition_failed`
without replacing it. Never blindly retry an uncertain write: read and inspect
the owned path first, then clean up and choose a new run.

The publisher prints `sha256=<64-character digest>` followed by:

```text
["The synthetic observatory has a blue dome.", "The telescope opens at dusk."]
```

## Run in a fresh process

Copy the printed digest through trusted job metadata outside the shared prefix:

```sh
export SHA256="paste-the-digest-from-publish"
uv run python handoff.py run --filesystem "$FS_ID" --run "$RUN" --sha256 "$SHA256"
```

The new process uses a read-only SDK handle and prints the same two strings.
It accepts only this recipe’s exact known configuration, with its pinned versions,
a maximum of 16 KiB and the publisher’s SHA-256. Only after these checks does it
call `Pipeline.load()` with Haystack’s default trusted-module protections. It
refuses an environment extension of `HAYSTACK_DESERIALIZATION_ALLOWLIST`, and
never passes `unsafe=True`, adds modules or uses deserialization callbacks.

Load only configuration from your own trusted publisher. A checksum detects
changed bytes; it does not establish authorship or make an untrusted pipeline
safe. The exact-config check intentionally makes this a narrow runnable fixture,
not a general loader for other pipelines. Review new configurations and their
components before adapting it. A default module allowlist is an additional
protection, not a sandbox.

The SDK buffers downloads before the example checks their size. Keep this on your
own small files; the check is not a network download limit.

## Clean up and billing

```sh
uv run python handoff.py cleanup --filesystem "$FS_ID" --run "$RUN" --sha256 "$SHA256"
```

Cleanup checks the same fingerprint and known configuration, deletes with the
current ETag, and removes `haystack` only if empty. Wrong digests preserve the
file. If another writer changes the file, investigate it rather than bypassing
the checks. Coordinate deletion with readers. Inspect and revoke only this
recipe’s SDK sessions from your backend or the console; closing the SDK client
closes HTTP connections but does not revoke sessions, which otherwise expire.

Storage and downloads follow [usage pricing](https://mountable.io/docs/billing).
There is no separate upload or per-request tariff. Deleting a file does not undo
usage; retained content stays billed until garbage collection. The fixture YAML
is 977 bytes. One upload and one read use about EUR 0.0000000155 in download
charges and EUR 0.0000001547 for a full month of YAML storage, plus entry charges
(EUR 0.425 per million entries-month), before tax. These are tariff estimates,
not provider-cost or margin measurements. The example creates no account,
filesystem or credits. Set your own [spending cap](https://mountable.io/docs/end-user-limits).

## Offline checks

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

Tests exercise real pinned Haystack serialization/execution and the released SDK
over fake HTTP: matching fresh-process output, create-only preservation,
read-only write denial, corrupt/oversize content rejected before loading,
untrusted config rejected despite its matching hash, Haystack’s default module
protection, unchanged-file cleanup and invalid path rejection. Production
verification and billed usage are recorded in the product repository’s
`infra/prod.yaml` under `haystack_pipeline`.

Primary references: [pipeline creation](https://docs.haystack.deepset.ai/docs/creating-pipelines)
and [serialization/security](https://docs.haystack.deepset.ai/docs/serialization).
