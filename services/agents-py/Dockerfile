FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PORT=8000

WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Install python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir -r requirements.txt

# Copy application source
COPY . .

EXPOSE 8000

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD curl -f "http://localhost:${PORT:-8000}/healthz" || exit 1

# Shell form so the platform-provided $PORT (e.g. Render) is honoured; defaults to 8000.
CMD ["sh", "-c", "exec uvicorn src.agents_py.main:app --host 0.0.0.0 --port ${PORT:-8000}"]

