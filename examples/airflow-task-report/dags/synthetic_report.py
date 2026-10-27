"""TaskFlow DAG: pass a report id through XCom and keep the bytes in the file."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import pendulum
from airflow.sdk import dag, get_current_context, task

from report import (
    PUBLISH_DAG_ID,
    REPLACE_DAG_ID,
    RecipeError,
    check_stored_report,
    new_report_id,
    open_store,
    put_report,
    render_report,
)

_START = pendulum.datetime(2026, 1, 1, tz="UTC")


def report_id_from_conf() -> str:
    conf = get_current_context()["dag_run"].conf or {}
    report_id = conf.get("report_id") if isinstance(conf, dict) else None
    if not isinstance(report_id, str):
        raise RecipeError("bad_id", "Pass report_id in the DAG run conf.")
    return report_id


@dag(
    dag_id=PUBLISH_DAG_ID,
    schedule=None,
    start_date=_START,
    catchup=False,
)
def synthetic_report():
    """Upload a closed report, then check it from the next task."""

    @task
    def produce_report() -> str:
        report_id = new_report_id()
        put_report(open_store("rw"), report_id, render_report(report_id))
        print(f"report_id={report_id}")
        return report_id

    @task
    def check_report(report_id: str) -> str:
        print(check_stored_report(report_id))
        return report_id

    check_report(produce_report())


@dag(
    dag_id=REPLACE_DAG_ID,
    schedule=None,
    start_date=_START,
    catchup=False,
)
def replace_report():
    """Try to create the same id again. The first file stays."""

    @task
    def replace_same_report() -> str:
        report_id = report_id_from_conf()
        put_report(open_store("rw"), report_id, b"replaced\n")
        return report_id

    replace_same_report()


synthetic_report()
replace_report()
