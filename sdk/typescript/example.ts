// Book a ride and stream its location (Node 22+: npx tsx example.ts).
import { HexaGo, type RideUpdate } from "./hexago.ts";

const hx = await HexaGo.createKey("example"); // or new HexaGo({ apiKey: "hxg_..." })
const { ride } = await hx.requestRide("rider_demo", [12.9716, 77.5946], [12.9352, 77.6245]);
console.log("ride", ride.id, ride.status);
for await (const { data: d } of hx.stream<RideUpdate>(`ride:${ride.id}`)) {
  console.log(d.status, d.driver.lat, d.driver.lng, `eta ${d.eta_sec}s`);
  if (d.status === "completed" || d.status === "cancelled") break;
}
