# Notebook starters

Two notebooks that keep their files in a mountable.io folder, so datasets,
feature caches and checkpoints stay when the kernel restarts or the session
times out. Both use `mountable-sdk` and read `MOUNTABLE_API_KEY` and
`MOUNTABLE_FS_ID` from the platform's secrets or the environment.

| Notebook | Shows | Open in |
| --- | --- | --- |
| [`mountable_quickstart.ipynb`](mountable_quickstart.ipynb) | a dataset saved once, a feature cache, training that resumes from its last checkpoint, and a FUSE mount where `/dev/fuse` exists | [Colab](https://colab.research.google.com/github/writeitai/mountable/blob/main/examples/notebooks/mountable_quickstart.ipynb), [Kaggle](https://www.kaggle.com/kernels/welcome?src=https://github.com/writeitai/mountable/blob/main/examples/notebooks/mountable_quickstart.ipynb), [Deepnote](https://deepnote.com/launch?url=https%3A%2F%2Fgithub.com%2Fwriteitai%2Fmountable%2Fblob%2Fmain%2Fexamples%2Fnotebooks%2Fmountable_quickstart.ipynb) |
| [`mountable_marimo.py`](mountable_marimo.py) | exploring the CSV files in the folder, saving a summary to `/artifacts`, and a webhook that tells the next job | [molab](https://molab.marimo.io/github/writeitai/mountable/blob/main/examples/notebooks/mountable_marimo.py), or `uvx marimo edit --sandbox mountable_marimo.py` |

Where the secrets go: Colab **Secrets** (the key icon), Kaggle **Add-ons →
Secrets**, and environment variables in Deepnote, Hugging Face Spaces (Space
secrets) and Jupyter on your own machine.

## Test without a filesystem

The test runs both notebooks against a local fake of the files API; nothing
reaches mountable.io:

```sh
(cd sdk/python && uv run --locked --with marimo==0.25.2 python ../../examples/notebooks/test_notebooks.py)
```
