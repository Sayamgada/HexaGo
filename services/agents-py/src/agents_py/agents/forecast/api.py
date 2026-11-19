"""Demand Forecast endpoint (dispatch-go proxies /v1/forecast/* here).

    GET /v1/forecast/heatmap?horizon=0|15|30
        cells {h3, boundary, demand_score, expected_earnings_per_hr, explanation, ...}, highest demand first
"""
from __future__ import annotations

from fastapi import APIRouter, HTTPException, Query

from src.agents_py.agents.forecast import agent as forecast_agent
from src.agents_py.agents.forecast.model import HORIZONS
from src.agents_py.config import settings

router = APIRouter()


@router.get("/v1/forecast/heatmap")
async def heatmap(horizon: int = Query(0, description="minutes ahead: 0, 15 or 30")):
    if not settings.FORECAST_ENABLED:
        raise HTTPException(503, "demand forecast is disabled (FORECAST_ENABLED=false)")
    if horizon not in HORIZONS:
        raise HTTPException(422, f"horizon must be one of {list(HORIZONS)}")
    a = forecast_agent.get_agent()
    interval = settings.FORECAST_INTERVAL_S or 60
    if a.stale(2 * interval):  # e.g. right after a wake-up, before the loop ran
        await forecast_agent.run_forecast(a)
    return {**a.view(horizon), "interval_s": interval}
