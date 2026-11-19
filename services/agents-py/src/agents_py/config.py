from typing import Optional

from pydantic_settings import BaseSettings, SettingsConfigDict

from src.agents_py.cors import DEFAULT_ORIGINS


class Settings(BaseSettings):
    PORT: int = 8000
    # gemini-2.x and text-embedding-004 answer 404 (retired) as of 2026-11-18.
    LLM_MODEL: str = "gemini/gemini-3.5-flash-lite"
    # Tried in order after LLM_MODEL: a 404 drops a model, a 429/503 backs off and moves on.
    LLM_FALLBACK_MODELS: str = "gemini/gemini-3.1-flash-lite,gemini/gemini-3-flash-preview"
    GEMINI_API_KEY: Optional[str] = None
    # Empty DATABASE_URL / REDIS_URL mean "run in memory" (tests, keyless local runs).
    DATABASE_URL: str = ""
    REDIS_URL: str = ""
    DISPATCH_URL: str = "http://localhost:8080"
    DEMO_MODE: bool = True
    CITY_CENTER_LAT: float = 12.9716
    CITY_CENTER_LNG: float = 77.5946
    CORS_ALLOWED_ORIGINS: str = DEFAULT_ORIGINS

    # LLM cost guardrails (see PROJECT_CONTEXT.md "LLM cost guardrails").
    LLM_ENABLED: bool = True
    LLM_TIMEOUT_S: float = 4.0
    LLM_BACKOFF_S: float = 0.5
    LLM_DAILY_CALL_LIMIT: int = 300
    LLM_DAILY_TOKEN_LIMIT: int = 200_000
    LLM_MIN_INTERVAL_S: float = 20.0
    LLM_CACHE_TTL_S: int = 600
    EMBEDDING_MODEL: str = "gemini/gemini-embedding-001"  # requested at 768 dims

    # Ops Supervisor heartbeat period; 0 disables it.
    HEARTBEAT_INTERVAL_S: float = 300.0
    # Settlement agent ledger audit period; 0 disables it.
    SETTLEMENT_SCAN_INTERVAL_S: float = 60.0
    # Surge Equilibrium surge-map recompute period; 0 disables the loop (the endpoint still computes on demand).
    SURGE_MAP_INTERVAL_S: float = 30.0
    # Demand Forecast: on/off switch and recompute period (0 disables the loop; the endpoint still computes on demand).
    FORECAST_ENABLED: bool = True
    FORECAST_INTERVAL_S: float = 60.0

    # The jurisdiction the surge map and fare quotes are priced in (a Legal & Jurisdiction city key).
    CITY_NAME: str = "Bengaluru"
    # Legal & Jurisdiction agent: rules table, regulation ingest, surge cap hook. Off -> static caps.
    LEGAL_ENABLED: bool = True
    # Ops Supervisor "Autonomous Ops Report": period of the scheduled report; 0 disables the loop.
    OPS_REPORT_ENABLED: bool = True
    OPS_REPORT_INTERVAL_S: float = 300.0

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")


settings = Settings()
