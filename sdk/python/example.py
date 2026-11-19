"""Book a ride and stream its location (pip install websockets)."""
import asyncio

from hexago import HexaGo


async def main():
    hx = HexaGo.create_key("example")  # or HexaGo(api_key="hxg_...")
    ride = hx.request_ride("rider_demo", pickup=(12.9716, 77.5946), dropoff=(12.9352, 77.6245))["ride"]
    print("ride", ride["id"], ride["status"])
    async for frame in hx.stream(f"ride:{ride['id']}"):
        d = frame["data"]
        print(d["status"], d["driver"]["lat"], d["driver"]["lng"], f"eta {d['eta_sec']}s")
        if d["status"] in ("completed", "cancelled"):
            break

asyncio.run(main())
