# syntax=docker/dockerfile:1.7

# ============================================================================

# Stage 1: Build the Go binary

# ============================================================================

# We use the official Go image. The -alpine variant gives us a small builder

# (~150MB) which keeps the build cache compact. The final image doesn't

# inherit from this — only the binary copies over.

FROM golang:1.22-alpine AS builder

WORKDIR /src

# Copy go.mod / go.sum first and download deps in a separate layer.

# This layer caches as long as deps don't change, so most rebuilds skip it.

COPY go.mod go.sum* ./

RUN go mod download

# Now copy the rest of the source. Changes here invalidate cache from this

# point forward, but the deps download above stays cached.

COPY . .

# Build a static binary.

#   - CGO_ENABLED=0 — modernc.org/sqlite is pure Go, no C deps. Static link.

#   - -trimpath — strips local paths from binary (smaller, more reproducible)

#   - -ldflags "-s -w" — strips debug info (smaller binary)

#   - -o /out/Agent-Go-Go — write to a known location for the next stage

RUN CGO_ENABLED=0 go build \

    -trimpath \

    -ldflags="-s -w" \

    -o /out/Agent-Go-Go \

    ./

# ============================================================================

# Stage 2: Runtime image

# ============================================================================

# We need:

#   - the Go binary

#   - Node.js + npx for the filesystem and fetch MCP servers

#   - uv (Python tool runner) for the sqlite MCP server

#   - ca-certificates for HTTPS to OpenRouter, Slack, MCP fetch targets

#

# debian:bookworm-slim gives us all this with apt-get available, and is

# small enough (~80MB base). Alpine would be smaller but uv binary

# distribution prefers glibc.

FROM debian:bookworm-slim AS runtime

# Install runtime deps in a single layer — apt cache is cleared in same RUN

# to keep the image small. -y answers prompts; --no-install-recommends skips

# optional companion packages.

RUN apt-get update && apt-get install -y --no-install-recommends \

        ca-certificates \

        curl \

        nodejs \

        npm \

    && rm -rf /var/lib/apt/lists/*

# Install uv (the Python tool runner used by mcp-server-sqlite).

# Pinned to a specific version for reproducibility — bump intentionally.

RUN curl -LsSf https://astral.sh/uv/0.5.5/install.sh | sh && \

    mv /root/.local/bin/uv /usr/local/bin/uv && \

    mv /root/.local/bin/uvx /usr/local/bin/uvx

# Pre-install the MCP servers so they don't have to be fetched on first

# launch (which would slow cold-start and could fail if npm registry is

# slow). The filesystem and fetch servers ship via npx; we cache them by

# running --help once. The sqlite server is grabbed by uv.

RUN npx -y @modelcontextprotocol/server-filesystem --help > /dev/null 2>&1 || true && \

    npx -y @modelcontextprotocol/server-fetch --help > /dev/null 2>&1 || true && \

    uv tool install mcp-server-sqlite || true

# Run as a non-root user. Containers running as root are a security

# anti-pattern; create a dedicated user with no shell/login.

RUN useradd --system --create-home --home-dir /app --shell /usr/sbin/nologin agentgogo

WORKDIR /app

# Copy the binary from the builder stage. Owned by our non-root user.

COPY --from=builder --chown=agentgogo:agentgogo /out/Agent-Go-Go /app/Agent-Go-Go

# Copy config files — these aren't secrets, they're tracked in git.

COPY --chown=agentgogo:agentgogo config/ /app/config/

# Create the data directory. In production, /app/data should be a mounted

# volume so SQLite + workspace files survive container restarts. The

# fly.toml mounts a volume here.

RUN mkdir -p /app/data/workspace && \

    chown -R agentgogo:agentgogo /app/data

USER agentgogo

# Slack POSTs to /slack/events — Fly will route 443 → 8080 here.

EXPOSE 8080

# Healthcheck against /health so Fly knows when the container is ready.

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \

    CMD curl -f http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/Agent-Go-Go"]
