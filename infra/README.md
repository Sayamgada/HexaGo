# HexaGo Infrastructure

This directory contains infrastructure-as-code manifests, container configurations, and cloud deployment specifications.

- `../render.yaml`: Primary Render Blueprint specification configuring managed Postgres (pgvector), Key-Value (Redis), dispatch-go, agents-py, and the Flutter Web static frontend.
- `../docker-compose.yml`: Local multi-service composition for development and integration testing.

