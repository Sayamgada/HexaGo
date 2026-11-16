"""CORS origin allowlist parsing, shared format with dispatch-go's CORS_ALLOWED_ORIGINS."""
import re
from typing import List, Optional, Tuple

DEFAULT_ORIGINS = "https://hexago-web.onrender.com,http://localhost:*,http://127.0.0.1:*"


def parse_origins(value: str) -> Tuple[List[str], Optional[str]]:
    """Split a comma-separated origin list into exact origins and a port-wildcard regex.

    An entry ending in ":*" (e.g. "http://localhost:*") matches that scheme and host
    on any port, or with no port. Other entries must match exactly.
    """
    exact: List[str] = []
    wildcard: List[str] = []
    for raw in value.split(","):
        entry = raw.strip().rstrip("/")
        if not entry:
            continue
        if entry.endswith(":*"):
            wildcard.append(re.escape(entry[:-2]) + r"(:\d+)?")
        else:
            exact.append(entry)
    regex = "|".join(wildcard) if wildcard else None
    return exact, regex
