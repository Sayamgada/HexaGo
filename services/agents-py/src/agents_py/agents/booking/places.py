"""Place resolution: the rider's saved places first, then Nominatim (bounded to the city).

Three demo rider personas in Bengaluru carry saved places (home, office, kid's
school, soccer practice, airport) with real coordinates. Anything else is
geocoded through Nominatim: cached, at most 1 request per second, with an
identifying User-Agent, bounded to a box around the city centre. A small
built-in gazetteer of well-known Bengaluru areas answers when Nominatim is
unreachable, so demos work offline.
"""
from __future__ import annotations

import asyncio
import logging
import math
import re
import time
from dataclasses import asdict, dataclass, field
from typing import Any, Awaitable, Callable, Dict, List, Optional, Tuple

import httpx

from src.agents_py.config import settings

log = logging.getLogger("hexago.booking.places")

NOMINATIM_URL = "https://nominatim.openstreetmap.org/search"
USER_AGENT = "HexaGo-BookingIntent/1.0 (+https://hexago-web.onrender.com; demo ride-hailing planner)"
CITY_RADIUS_DEG = 0.35  # ~38 km box around the centre (covers the airport)
GEOCODE_TTL_S = 7 * 24 * 3600


@dataclass(frozen=True)
class SavedPlace:
    key: str
    label: str
    lat: float
    lng: float
    aliases: Tuple[str, ...] = ()
    kind: str = ""  # e.g. "school" - two places of one kind make "school" ambiguous


@dataclass(frozen=True)
class Persona:
    id: str
    name: str
    blurb: str
    kids: Tuple[str, ...]
    places: Tuple[SavedPlace, ...]
    default_tier: str = "mini"

    @property
    def home(self) -> SavedPlace:
        return next(p for p in self.places if p.key == "home")

    def public(self) -> Dict[str, Any]:
        return {"id": self.id, "name": self.name, "blurb": self.blurb, "kids": list(self.kids),
                "default_tier": self.default_tier,
                "saved_places": [{"key": p.key, "label": p.label, "lat": p.lat, "lng": p.lng} for p in self.places]}


AIRPORT = SavedPlace("airport", "Kempegowda International Airport (BLR)", 13.1989, 77.7068,
                     ("airport", "the airport", "blr", "kia", "kempegowda airport", "bangalore airport",
                      "bengaluru airport", "kempegowda international airport"), "airport")

PERSONAS: Dict[str, Persona] = {p.id: p for p in (
    Persona(
        "rider_priya", "Priya Sharma", "Parent in HSR Layout; works at Manyata Tech Park; daughter Anaya.",
        ("Anaya",),
        (
            SavedPlace("home", "Home - HSR Layout Sector 2", 12.9116, 77.6474, ("home", "house", "my place")),
            SavedPlace("office", "Office - Manyata Tech Park, Hebbal", 13.0475, 77.6200,
                       ("office", "work", "workplace", "manyata", "manyata tech park")),
            SavedPlace("kid_school", "Anaya's school - National Public School, Koramangala", 12.9352, 77.6265,
                       ("school", "kid's school", "kids school", "anaya's school", "anayas school",
                        "national public school", "nps koramangala", "nps"), "school"),
            SavedPlace("soccer", "Soccer practice - Bangalore Football Stadium", 12.9690, 77.6005,
                       ("soccer practice", "soccer", "football practice", "football", "practice",
                        "soccer class", "bangalore football stadium"), "sport"),
            AIRPORT,
        ),
    ),
    Persona(
        "rider_arjun", "Arjun Rao", "Consultant in Indiranagar; office at Embassy GolfLinks; two kids.",
        ("Aanya", "Kabir"),
        (
            SavedPlace("home", "Home - Indiranagar 100 Feet Road", 12.9784, 77.6408, ("home", "house", "my place")),
            SavedPlace("office", "Office - Embassy GolfLinks", 12.9545, 77.6430,
                       ("office", "work", "workplace", "embassy golflinks", "golf links")),
            SavedPlace("school_aanya", "Aanya's school - Bishop Cotton Girls' School", 12.9667, 77.6006,
                       ("school", "kid's school", "aanya's school", "aanyas school", "girls school",
                        "bishop cotton girls school"), "school"),
            SavedPlace("school_kabir", "Kabir's school - Bishop Cotton Boys' School", 12.9660, 77.5985,
                       ("school", "kid's school", "kabir's school", "kabirs school", "boys school",
                        "bishop cotton boys school"), "school"),
            SavedPlace("soccer", "Soccer practice - Indiranagar Club grounds", 12.9719, 77.6412,
                       ("soccer practice", "soccer", "football practice", "football", "practice"), "sport"),
            AIRPORT,
        ),
        default_tier="sedan",
    ),
    Persona(
        "rider_meera", "Meera Iyer", "Engineer in Jayanagar; commutes to Electronic City; son Rohan.",
        ("Rohan",),
        (
            SavedPlace("home", "Home - Jayanagar 4th Block", 12.9250, 77.5838, ("home", "house", "my place")),
            SavedPlace("office", "Office - Infosys, Electronic City", 12.8452, 77.6602,
                       ("office", "work", "workplace", "infosys", "electronic city campus")),
            SavedPlace("kid_school", "Rohan's school - National College, Basavanagudi", 12.9480, 77.5730,
                       ("school", "kid's school", "rohan's school", "rohans school", "national college"), "school"),
            SavedPlace("soccer", "Soccer practice - Kanteerava Stadium", 12.9698, 77.5932,
                       ("soccer practice", "soccer", "football practice", "football", "practice",
                        "kanteerava stadium"), "sport"),
            AIRPORT,
        ),
    ),
)}
DEFAULT_PERSONA = "rider_priya"

# Well-known areas, used when Nominatim cannot be reached.
GAZETTEER: Dict[str, Tuple[str, float, float]] = {
    "mg road": ("MG Road", 12.9756, 77.6050),
    "koramangala": ("Koramangala", 12.9352, 77.6245),
    "indiranagar": ("Indiranagar", 12.9784, 77.6408),
    "whitefield": ("Whitefield", 12.9698, 77.7500),
    "electronic city": ("Electronic City", 12.8452, 77.6602),
    "hsr layout": ("HSR Layout", 12.9116, 77.6474),
    "jayanagar": ("Jayanagar", 12.9250, 77.5838),
    "majestic": ("Majestic (KSR Bengaluru)", 12.9767, 77.5713),
    "hebbal": ("Hebbal", 13.0358, 77.5970),
    "marathahalli": ("Marathahalli", 12.9569, 77.7011),
    "malleshwaram": ("Malleshwaram", 13.0031, 77.5643),
    "cubbon park": ("Cubbon Park", 12.9763, 77.5929),
    "brigade road": ("Brigade Road", 12.9719, 77.6070),
    "ub city": ("UB City", 12.9716, 77.5960),
    "bellandur": ("Bellandur", 12.9258, 77.6764),
}


def get_persona(rider_id: str) -> Persona:
    return PERSONAS.get(rider_id or "", PERSONAS[DEFAULT_PERSONA])


_LEAD = re.compile(r"^(?:(?:my|the|our|a|an|his|her|their|to|at|from)\s+)+")
_PUNCT = re.compile(r"[^\w\s']")


def normalize(text: str) -> str:
    t = _PUNCT.sub(" ", (text or "").lower().replace("’", "'"))
    t = re.sub(r"\s+", " ", t).strip()
    return _LEAD.sub("", t).strip()


def match_saved(persona: Persona, text: str) -> List[SavedPlace]:
    """Saved places whose alias is the text, or (failing that) the longest alias inside it."""
    t = normalize(text)
    if not t:
        return []
    exact = [p for p in persona.places if t in p.aliases or t == p.key]
    if exact:
        return exact
    best, best_len = [], 0
    for p in persona.places:
        for a in p.aliases:
            if re.search(rf"\b{re.escape(a)}\b", t):
                if len(a) > best_len:
                    best, best_len = [p], len(a)
                elif len(a) == best_len and p not in best:
                    best.append(p)
    return best


@dataclass
class Place:
    label: str
    lat: float
    lng: float
    source: str  # saved | geocoded | gazetteer | current
    key: str = ""
    query: str = ""

    def public(self) -> Dict[str, Any]:
        return asdict(self)


def in_city(lat: float, lng: float, center: Tuple[float, float], radius_deg: float = CITY_RADIUS_DEG) -> bool:
    return abs(lat - center[0]) <= radius_deg and abs(lng - center[1]) <= radius_deg


class Geocoder:
    """Nominatim search bounded to the city, cached, rate-limited to 1 request/second."""

    def __init__(self, *, center: Optional[Tuple[float, float]] = None, cache: Any = None,
                 transport: Optional[httpx.AsyncBaseTransport] = None, timeout_s: float = 4.0,
                 min_interval_s: float = 1.0, clock: Callable[[], float] = time.monotonic,
                 sleep: Callable[[float], Awaitable[Any]] = asyncio.sleep, url: str = NOMINATIM_URL):
        self.center = center or (settings.CITY_CENTER_LAT, settings.CITY_CENTER_LNG)
        self.cache = cache  # GuardStore-like: get_json / set_json
        self._memo: Dict[str, Optional[Dict[str, Any]]] = {}
        self._transport = transport
        self.timeout_s = timeout_s
        self.min_interval_s = min_interval_s
        self.clock = clock
        self.sleep = sleep
        self.url = url
        self._lock = asyncio.Lock()
        self._last: Optional[float] = None
        self.requests = 0

    def _viewbox(self) -> str:
        lat, lng = self.center
        r = CITY_RADIUS_DEG
        return f"{lng - r},{lat + r},{lng + r},{lat - r}"

    async def _throttle(self) -> None:
        if self._last is not None:
            wait = self.min_interval_s - (self.clock() - self._last)
            if wait > 0:
                await self.sleep(wait)
        self._last = self.clock()

    async def _cached(self, key: str) -> Tuple[bool, Optional[Dict[str, Any]]]:
        if key in self._memo:
            return True, self._memo[key]
        if self.cache is not None:
            try:
                hit = await self.cache.get_json(f"geocode:{key}")
            except Exception:  # noqa: BLE001
                hit = None
            if hit is not None:
                self._memo[key] = hit or None
                return True, hit or None
        return False, None

    async def _remember(self, key: str, value: Optional[Dict[str, Any]]) -> None:
        self._memo[key] = value
        if self.cache is not None:
            try:
                await self.cache.set_json(f"geocode:{key}", value or {}, GEOCODE_TTL_S)
            except Exception:  # noqa: BLE001
                pass

    async def geocode(self, text: str) -> Optional[Place]:
        key = normalize(text)
        if not key:
            return None
        hit, value = await self._cached(key)
        if not hit:
            value = await self._search(key)
            if value is not None or key not in GAZETTEER:
                await self._remember(key, value)
        if value:
            return Place(value["label"], value["lat"], value["lng"], "geocoded", query=text)
        if key in GAZETTEER:
            label, lat, lng = GAZETTEER[key]
            return Place(label, lat, lng, "gazetteer", query=text)
        return None

    async def _search(self, key: str) -> Optional[Dict[str, Any]]:
        params = {"q": f"{key}, {settings_city()}", "format": "jsonv2", "limit": "1", "countrycodes": "in",
                  "viewbox": self._viewbox(), "bounded": "1"}
        async with self._lock:
            await self._throttle()
            self.requests += 1
            try:
                async with httpx.AsyncClient(timeout=self.timeout_s, transport=self._transport,
                                             headers={"User-Agent": USER_AGENT}) as http:
                    resp = await http.get(self.url, params=params)
                if resp.status_code != 200:
                    log.warning("nominatim %r: HTTP %s", key, resp.status_code)
                    return None
                rows = resp.json()
            except (httpx.HTTPError, ValueError) as err:
                log.warning("nominatim %r failed: %s", key, err)
                return None
        if not rows:
            return None
        row = rows[0]
        lat, lng = float(row["lat"]), float(row["lon"])
        if not in_city(lat, lng, self.center):
            return None
        name = row.get("name") or str(row.get("display_name", key)).split(",")[0]
        return {"label": name, "lat": round(lat, 6), "lng": round(lng, 6)}


def settings_city() -> str:
    return "Bengaluru"


@dataclass
class Resolution:
    place: Optional[Place] = None
    options: List[SavedPlace] = field(default_factory=list)  # >1 -> ambiguous

    @property
    def ambiguous(self) -> bool:
        return self.place is None and len(self.options) > 1


async def resolve_place(persona: Persona, text: Optional[str], geocoder: Optional[Geocoder],
                        kid_hint: Optional[str] = None) -> Resolution:
    """None/empty text -> the rider's current location (home for the demo personas)."""
    if not (text or "").strip():
        h = persona.home
        return Resolution(Place(f"Current location ({h.label.split(' - ')[-1]})", h.lat, h.lng, "current", "current"))
    saved = match_saved(persona, text)
    if len(saved) > 1 and kid_hint:
        hinted = [p for p in saved if kid_hint.lower() in p.label.lower()]
        saved = hinted or saved
    if len(saved) == 1:
        p = saved[0]
        return Resolution(Place(p.label, p.lat, p.lng, "saved", p.key, text or ""))
    if len(saved) > 1:
        return Resolution(options=saved)
    if geocoder is not None:
        place = await geocoder.geocode(text)
        if place is not None:
            return Resolution(place)
    return Resolution()


def haversine_km(a: Tuple[float, float], b: Tuple[float, float]) -> float:
    r = 6371.0088
    la1, lo1, la2, lo2 = map(math.radians, (a[0], a[1], b[0], b[1]))
    h = math.sin((la2 - la1) / 2) ** 2 + math.cos(la1) * math.cos(la2) * math.sin((lo2 - lo1) / 2) ** 2
    return 2 * r * math.asin(math.sqrt(h))
