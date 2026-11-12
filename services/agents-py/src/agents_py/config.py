from typing import Optional
from pydantic_settings import BaseSettings, SettingsConfigDict

class Settings(BaseSettings):
    PORT: int = 8000
    LLM_MODEL: str = "gemini/gemini-2.5-flash"
    GEMINI_API_KEY: Optional[str] = None
    DATABASE_URL: str = "postgres://hexago:hexago@localhost:5432/hexago?sslmode=disable"
    REDIS_URL: str = "redis://localhost:6379/0"
    DEMO_MODE: bool = True
    CITY_CENTER_LAT: float = 12.9716
    CITY_CENTER_LNG: float = 77.5946

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

settings = Settings()
