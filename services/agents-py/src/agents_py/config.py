from typing import Optional

from pydantic_settings import BaseSettings, SettingsConfigDict

from src.agents_py.cors import DEFAULT_ORIGINS


class Settings(BaseSettings):
    PORT: int = 8000
    LLM_MODEL: str = "gemini/gemini-2.0-flash"
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
    LLM_TIMEOUT_S: float = 3.0
    LLM_DAILY_CALL_LIMIT: int = 300
    LLM_DAILY_TOKEN_LIMIT: int = 200_000
    LLM_MIN_INTERVAL_S: float = 20.0
    LLM_CACHE_TTL_S: int = 600
    EMBEDDING_MODEL: str = "gemini/text-embedding-004"

    # Ops Supervisor heartbeat period; 0 disables it.
    HEARTBEAT_INTERVAL_S: float = 300.0

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")


settings = Settings()
