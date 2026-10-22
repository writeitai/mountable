# /// script
# requires-python = ">=3.10"
# dependencies = ["marimo", "mountable-sdk"]
# ///

import marimo

__generated_with = "0.25.2"
app = marimo.App()


@app.cell
def _():
    import csv
    import io
    import json
    import os
    import statistics

    import httpx
    import marimo as mo
    from mountable_sdk import Mountable, MountableError

    return Mountable, MountableError, csv, httpx, io, json, mo, os, statistics


@app.cell
def _(mo):
    mo.md("""
    # Explore datasets in a Mountable folder

    [Mountable](https://mountable.io/?utm_source=data-science-and-ml-workspaces&utm_medium=notebook&utm_content=nb-02-marimo)
    is a persistent filesystem for notebooks and AI agents. This notebook
    explores the CSV files in one Mountable folder and saves a summary
    for the next job to pick up. The files stay when this session ends.

    Set `MOUNTABLE_API_KEY` and `MOUNTABLE_FS_ID` in the environment
    before you start (create both in the
    [console](https://mountable.io/app?utm_source=data-science-and-ml-workspaces&utm_medium=notebook&utm_content=nb-02-marimo)).
    """)
    return


@app.cell
def _(Mountable, MountableError, os):
    client = Mountable()  # MOUNTABLE_API_KEY
    fs = client.filesystem(os.environ["MOUNTABLE_FS_ID"], mode="rw")

    def csv_files():
        try:
            page = fs.list("datasets")
        except MountableError as e:
            if e.code != "not_found":
                raise
            return []
        return [e.path for e in page.entries if e.path.endswith(".csv")]

    return client, csv_files, fs


@app.cell
def _(csv_files, fs):
    # A fresh filesystem gets a small sample, so the notebook has something to show.
    if not csv_files():
        fs.write(
            "datasets/weather.csv",
            "day,temp_c,rain_mm\n1,14.2,0.0\n2,15.8,1.2\n3,13.1,6.4\n4,16.5,0.0\n5,17.9,0.3\n",
        )
    datasets = csv_files()
    return (datasets,)


@app.cell
def _(datasets, mo):
    picked = mo.ui.dropdown(options=datasets, value=datasets[0], label="Dataset")
    picked
    return (picked,)


@app.cell
def _(csv, fs, io, mo, picked, statistics):
    rows = list(csv.DictReader(io.StringIO(fs.read(picked.value).data.decode())))
    summary = []
    for column in rows[0]:
        try:
            values = [float(r[column]) for r in rows]
        except ValueError:
            continue
        summary.append(
            {
                "column": column,
                "mean": round(statistics.mean(values), 3),
                "min": min(values),
                "max": max(values),
            }
        )
    mo.ui.table(summary, selection=None)
    return rows, summary


@app.cell
def _(fs, json, mo, picked, rows, summary):
    # Hand the result to the next job: it lands in /artifacts.
    name = picked.value.rsplit("/", 1)[-1].removesuffix(".csv")
    artifact = f"artifacts/{name}.summary.json"
    fs.write(
        artifact,
        json.dumps({"dataset": picked.value, "rows": len(rows), "columns": summary}),
    )
    mo.md(
        f"Saved `{artifact}`. A webhook on `/artifacts` tells your next job"
        " the moment it lands."
    )
    return


@app.cell
def _(mo):
    mo.md("""
    ## Start the next job when an artifact lands

    A webhook calls your job's HTTPS endpoint with a signed
    `files.changed` message for every file created, written, renamed or
    deleted under `/artifacts`, whether a notebook, an agent's mount or a
    backend made the change. Set `MOUNTABLE_ORG_ID` (from
    `mountable orgs list`) and `MOUNTABLE_WEBHOOK_URL`, then create it
    once. Your endpoint verifies each message as shown in the
    [webhooks guide](https://mountable.io/docs/webhooks?utm_source=data-science-and-ml-workspaces&utm_medium=notebook&utm_content=nb-02-marimo#verify-the-signature).
    """)
    return


@app.cell
def _(mo):
    create = mo.ui.run_button(label="Create the webhook")
    create
    return (create,)


@app.cell
def _(client, create, fs, httpx, mo, os):
    mo.stop(not create.value)
    response = httpx.post(
        f"{client.base_url}/api/v1/orgs/{os.environ['MOUNTABLE_ORG_ID']}"
        f"/filesystems/{fs.id}/webhooks",
        headers={"Authorization": f"Bearer {os.environ['MOUNTABLE_API_KEY']}"},
        json={"url": os.environ["MOUNTABLE_WEBHOOK_URL"], "path_prefix": "/artifacts"},
    )
    response.raise_for_status()
    mo.md(
        "Webhook created. Copy its signing secret into your job's secrets now;"
        f" it is shown only once: `{response.json()['secret']}`"
    )
    return


if __name__ == "__main__":
    app.run()
