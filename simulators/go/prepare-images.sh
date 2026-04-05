#!/bin/bash
# Pre-cache Docker images for the simulator to avoid registry pulls during tests

set -e

export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:${PATH:-}"

echo "Pre-caching Docker images for simulator tests..."

# Pull base images if needed
echo "Ensuring base images are cached..."
docker pull postgres:16-alpine 2>&1 | grep -E "Status|Digest" || echo "postgres:16-alpine already cached"
docker pull debian:bookworm-slim 2>&1 | grep -E "Status|Digest" || echo "debian:bookworm-slim already cached"

# Build simulator image
echo "Building go-simulator image..."
docker compose build --no-cache=false simulator 2>&1 | tail -5

# Build pgbouncer image
echo "Building pgbouncer image..."
docker compose --profile pgbouncer build --no-cache=false pgbouncer 2>&1 | tail -5

# Build pgcat image
echo "Building pgcat image..."
docker compose --profile pgcat build --no-cache=false pgcat 2>&1 | tail -5

# Build grunyas image
echo "Building grunyas image..."
docker compose --profile grunyas build --no-cache=false grunyas 2>&1 | tail -5

echo ""
echo "✓ All images cached and ready for testing"
docker images | grep -E "go-simulator|postgres|debian|pgbouncer|pgcat|grunyas"
