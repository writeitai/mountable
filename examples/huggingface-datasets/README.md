# Reload a Hugging Face dataset from a fresh process

A worker builds a two-row synthetic `Dataset` with the Hugging Face
`datasets` library and calls `save_to_disk` with one shard. It uploads
that closed file set with `mountable-sdk`, then writes a completion
record only after every required file is stored and reads back. Stop
that process and start another one. The reader receives the dataset id,
checks each file hash, and only then calls `load_from_disk`.

The dataset id is the writer's return value. This is not a Hub upload,
a zip archive, or a remote filesystem adapter. The example does not
download anything and it does not need a Hugging Face account.

The filesystem id and the API key stay in the environment. The worker
passes the dataset id only.

This community example is maintained by mountable.io, independently of
Hugging Face. It is not an official Hugging Face integration. The hosted
mountable.io service is paid. The example and client are open source.

Pinned: `datasets` 5.1.0 and `mountable-sdk` 0.3.0. The one-shard file
names come from
[Dataset](https://huggingface.co/docs/datasets/package_reference/main_classes),
read on 2026-10-27.

## Setup

Use Python 3.11 or newer and uv. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Never put the key in a command argument.
Use an existing mountable.io account with a saved payment method, and a
filesystem the key can access. The example creates no account, filesystem
or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=hf-datasets-01-reload).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/huggingface-datasets
uv sync --locked
export MOUNTABLE_FILESYSTEM_ID="your-filesystem-id"
export DATASET_STORE=mountable
```

The dataset belongs to the fixture user `alice`. Your backend would choose
its own user, and it would still choose the filesystem and the path.

For one filesystem and a hard monthly spend cap per end user, follow the
[capped workspace guide](https://mountable.io/docs/capped-workspace-per-user?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=hf-datasets-01-reload).
This recipe uses one existing filesystem.

## Write the dataset

```sh
uv run python dataset.py write
```

A successful run prints the dataset id, the row count, the feature label
`item:string,count:int64`, the rows checksum, the file count and the byte
count. The dataset id is 16 lowercase hex characters. It is not a
filesystem id and it is not a path. The command writes these files under
`datasets/alice/<id>/`, in this order:

- `data-00000-of-00001.arrow`
- `dataset_info.json`
- `state.json`
- `complete.json`

`complete.json` is written last, and only after each of the three dataset
files reads back unchanged. Each write uses `If-None-Match: *`, so an
existing file is left as it is.

The rows are synthetic: `alpha` / `1` and `beta` / `2`. They hold no
customer data.

If an upload fails, the command exits without a completion record. A later
read of that id reports the dataset as incomplete.

## Read it from a fresh process

In a new shell, with the same environment, pass the id the writer printed:

```sh
uv run python dataset.py read --dataset-id "$ID"
```

This process checks the completion record, then each file's size and
checksum. It calls `load_from_disk` only when those checks pass, and it
checks the loaded rows and features. A matching dataset prints one
`match` line.

A second create of the same id does not replace the arrow file:

```sh
uv run python dataset.py overwrite --dataset-id "$ID"
```

That command exits with an error. The first files are unchanged. A
read-only handle cannot write them either:

```sh
uv run python dataset.py read-only-write --dataset-id "$ID"
```

One command does the same check against the configured store. It writes,
reads from a new process, rejects a replacement, rejects a read-only
write, and deletes the files:

```sh
uv run python dataset.py live
```

It does not print the API key.

## Clean up

```sh
uv run python dataset.py cleanup --dataset-id "$ID"
```

This deletes the completion record and the three dataset files. A file
that is already gone is left gone. Against mountable.io it also removes
the empty parent directories for that dataset.

## Offline checks

```sh
uv run pytest -q
uv run ruff check .
uv run ruff format --check .
```

The tests call the real `save_to_disk` and `load_from_disk` against a
fake store, and they also run two local processes over a temporary
directory. They make no paid cloud calls. They check the rows, features,
checksums, a failed upload, a changed file, a missing file, and that a
second create leaves the first bytes in place.

The guide is <https://mountable.io/docs/huggingface-datasets>.
