"""Autonomous Ops Report endpoints (dispatch-go proxies /v1/ops/* here).

    GET  /v1/ops/report          latest report (generated on demand when there is none yet)
    GET  /v1/ops/reports?limit=  history, newest first (id, time, trigger, anomalies, headline)
    GET  /v1/ops/reports/{id}    one full report
    POST /v1/ops/report/run      generate now

Behind OPS_REPORT_ENABLED (default true): off, every endpoint answers 503.
"""
from __future__ import annotations

from fastapi import APIRouter, HTTPException, Query

from src.agents_py.agents.supervisor import ops_report
from src.agents_py.config import settings

router = APIRouter()


def _enabled() -> None:
    if not settings.OPS_REPORT_ENABLED:
        raise HTTPException(503, "Autonomous Ops Report is disabled (OPS_REPORT_ENABLED=false)")


@router.get("/v1/ops/report")
async def latest_report():
    _enabled()
    r = await ops_report.get_reporter()
    return await r.latest_or_stored() or await r.run("on_demand")


@router.get("/v1/ops/reports")
async def reports(limit: int = Query(20, ge=1, le=100)):
    _enabled()
    rows = await (await ops_report.get_reporter()).history(limit)
    return {"reports": rows, "count": len(rows)}


@router.get("/v1/ops/reports/{report_id}")
async def report(report_id: str):
    _enabled()
    rep = await (await ops_report.get_reporter()).get(report_id)
    if rep is None:
        raise HTTPException(404, f"no report {report_id!r}")
    return rep


@router.post("/v1/ops/report/run")
async def run_report():
    _enabled()
    return await (await ops_report.get_reporter()).run("manual")
