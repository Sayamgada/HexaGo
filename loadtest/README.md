# HexaGo Load Testing

This directory holds load testing and simulation scripts for HexaGo.

## Targets
- Driver telemetry ingestion throughput (Redis GEO / H3 indexing)
- WebSocket concurrency under multi-agent dispatch surges
- Reverse-proxy latency between `dispatch-go` and `agents-py`

