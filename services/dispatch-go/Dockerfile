# Stage 1: Multi-stage build with CGO enabled for uber/h3-go
FROM golang:1.26-bookworm AS builder

# Install C compiler tools required for uber/h3-go CGO bindings
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache Go modules layer
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with CGO enabled
ENV CGO_ENABLED=1 \
    GOOS=linux

RUN go build -ldflags="-s -w" -o /app/bin/dispatch-server ./cmd/server

# Stage 2: Minimal runtime image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/bin/dispatch-server /app/dispatch-server

ENV PORT=8080 \
    AGENTS_URL=http://agents-py:8000 \
    DEMO_MODE=true \
    CITY_CENTER_LAT=12.9716 \
    CITY_CENTER_LNG=77.5946

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD curl -f "http://localhost:${PORT:-8080}/healthz" || exit 1

CMD ["/app/dispatch-server"]

