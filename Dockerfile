# =============================================================================
# Unified Devices Server - Dockerfile
# LiDAR + Radar + GStreamer Combined Server
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Builder
# -----------------------------------------------------------------------------
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Copy and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY main.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s -X main.Version=2.0.0" \
    -a -installsuffix cgo \
    -o unified-devices-server \
    main.go

# -----------------------------------------------------------------------------
# Stage 2: Runtime with GStreamer + SRT support
# -----------------------------------------------------------------------------
FROM ubuntu:24.04

LABEL maintainer="Skylark Labs"
LABEL description="Unified Devices Server - LiDAR, Radar, GStreamer"
LABEL version="2.0.0"

ENV DEBIAN_FRONTEND=noninteractive

WORKDIR /app

# Install GStreamer with full SRT/RTSP support and utilities
RUN apt-get update && apt-get install -y --no-install-recommends \
    # GStreamer core and plugins
    gstreamer1.0-tools \
    gstreamer1.0-plugins-base \
    gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad \
    gstreamer1.0-plugins-ugly \
    gstreamer1.0-libav \
    gstreamer1.0-rtsp \
    # SRT support
    libsrt1.5-gnutls \
    # System utilities
    ca-certificates \
    tzdata \
    curl \
    wget \
    tini \
    procps \
    net-tools \
    iputils-ping \
    # Cleanup
    && rm -rf /var/lib/apt/lists/* \
    && apt-get clean

# Create non-root user
RUN groupadd -g 1001 devices || true && \
    useradd -u 1001 -g 1001 -s /bin/bash -m devices || useradd -s /bin/bash -m devices

# Copy binary from builder
COPY --from=builder /app/unified-devices-server .
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Create required directories
RUN mkdir -p /app/state /app/logs && \
    chown -R devices:devices /app

# =============================================================================
# Environment Variables
# =============================================================================

# Server Configuration
ENV HOST=0.0.0.0
ENV API_PORT=8090
ENV TZ=UTC

# LiDAR Configuration
ENV LIDAR_ENABLED=true
ENV LIDAR_UDP_PORT=12345
ENV LIDAR_MAX_POINTS=2000000
ENV LIDAR_VOXEL_SIZE=0.02

# Radar Configuration
ENV RADAR_ENABLED=true
ENV RADAR_TTL=0.3
ENV RADAR_MAX_RANGE=50.0

# GStreamer Configuration
ENV GSTREAMER_ENABLED=true
ENV RTSP_PORT=8560
ENV SRT_PORT=8561
ENV SRT_LATENCY=50
ENV GST_LATENCY=0
ENV SRT_PASSPHRASE=
ENV STATE_FILE=/app/state/streams_state.json

# Security Configuration
ENV API_KEY=
ENV RATE_LIMIT_ENABLED=true
ENV RATE_LIMIT_RPS=100
ENV MAX_CONNECTIONS=1000
ENV MAX_DEVICES=100

# GStreamer Debug
ENV GST_DEBUG=2
ENV GST_DEBUG_NO_COLOR=1
ENV GST_REGISTRY_FORK=no

# =============================================================================
# Ports
# =============================================================================
# API: 8090
# LiDAR UDP: 12345
# RTSP: 8560
# SRT: 8561-8580 (range for listeners)
EXPOSE 8090/tcp 12345/udp 8560/tcp 8560/udp 8561-8580/udp

# =============================================================================
# Health Check
# =============================================================================
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:${API_PORT}/health || exit 1

# =============================================================================
# Entrypoint
# =============================================================================
# Run as root for network access (host mode reduces isolation anyway)
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["./unified-devices-server"]
